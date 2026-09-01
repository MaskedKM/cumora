// migrate-pg 单测(#285):窗口编排序、幂等 no-op、行数不一致阻断切链、
// dry-run 零写、DSN 脱敏。全 fake 依赖 —— 不碰真 pg/systemd/pg 工具。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
)

type fakeMigrate struct {
	srcCounts   map[string]int
	tgtCounts   map[string]int // nil = 同 srcCounts(全一致)
	srcDSN      string
	cmds        []string // 每次 RunCmd 的 "name args…"
	execSQLLog  []string
	pgAlive     bool // true = 内部 pg 已在跑(不 pg_ctl start)
	stopCalls   int
	startCalls  int
	failRestore bool
}

func (f *fakeMigrate) deps() MigrateDeps {
	d := MigrateDeps{
		RunCmd: func(_ context.Context, path string, args ...string) (string, error) {
			f.cmds = append(f.cmds, filepath.Base(path)+" "+strings.Join(args, " "))
			if f.failRestore && filepath.Base(path) == "pg_restore" {
				return "restore boom", fmt.Errorf("exit 1")
			}
			return "", nil
		},
		CountRows: func(_ context.Context, dsn, table string) (int, error) {
			if dsn == f.srcDSN {
				return f.srcCounts[table], nil
			}
			if f.tgtCounts == nil {
				return f.srcCounts[table], nil
			}
			return f.tgtCounts[table], nil
		},
		ExecSQL: func(_ context.Context, adminDSN, statement string) error {
			f.execSQLLog = append(f.execSQLLog, statement)
			return nil
		},
		PGAlive: func(context.Context, string) error {
			if f.pgAlive {
				return nil
			}
			return fmt.Errorf("down")
		},
		StopStack:  func() error { f.stopCalls++; return nil },
		StartStack: func() error { f.startCalls++; return nil },
	}
	return d
}

// setupMigrate —— 造一个可跑的迁移现场:toml(external)+ env 文件(DSN)+
// 制品桩(pg/bin 五件)+ 已 bootstrap 的 pgdata(PG_VERSION 在,跳 initdb)。
func setupMigrate(t *testing.T, counts map[string]int) (cfgDir, envFile string, cfg stackconfig.Config) {
	t.Helper()
	dir := t.TempDir()
	cfgDir = filepath.Join(dir, "config")
	dataHome := filepath.Join(dir, "data")
	current := filepath.Join(dir, "current")

	cfg = stackconfig.Defaults()
	cfg.Data.Home = dataHome
	// 手写 toml(不走 Save,模拟 import-env 产物亦可;走 Save 更真)。
	cfg.PG.Mode = stackconfig.ModeExternal
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := stackconfig.Save(filepath.Join(cfgDir, "stack.toml"), cfg); err != nil {
		t.Fatal(err)
	}

	envFile = filepath.Join(dir, "stack.env")
	if err := os.WriteFile(envFile, []byte("DATABASE_URL=postgres://u:secret@127.0.0.1:5432/cumora\nGITHUB_CLIENT_ID=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	pgBin := filepath.Join(current, "pg", "bin")
	if err := os.MkdirAll(pgBin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"postgres", "initdb", "pg_ctl", "pg_dump", "pg_restore"} {
		if err := os.WriteFile(filepath.Join(pgBin, tool), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 已 bootstrap:PG_VERSION 在 → EnsureInternalPG 零动作(桩 initdb 不被执行)。
	if err := os.MkdirAll(filepath.Join(dataHome, "pgdata"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataHome, "pgdata", "PG_VERSION"), []byte("16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataHome, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	return cfgDir, envFile, cfg
}

func migrateArgs(cfgDir, envFile string, extra ...string) []string {
	return append([]string{
		"--config-file", filepath.Join(cfgDir, "stack.toml"),
		"--env-file", envFile,
		"--current-dir", filepath.Join(filepath.Dir(cfgDir), "current"),
	}, extra...)
}

func TestMigrateHappyPath(t *testing.T) {
	cfgDir, envFile, cfg := setupMigrate(t, map[string]int{
		"messages": 100, "convene_transcript": 5, "board_cards": 8,
		"document_snapshots": 3, "conversations": 20, "participants": 7,
	})
	f := &fakeMigrate{srcCounts: map[string]int{
		"messages": 100, "convene_transcript": 5, "board_cards": 8,
		"document_snapshots": 3, "conversations": 20, "participants": 7,
	}, srcDSN: "postgres://u:secret@127.0.0.1:5432/cumora"}
	orig := newMigrateDeps
	newMigrateDeps = func(string) MigrateDeps { return f.deps() }
	t.Cleanup(func() { newMigrateDeps = orig })

	if code := cmdMigratePG(migrateArgs(cfgDir, envFile)); code != 0 {
		t.Fatalf("迁移应退 0: %d", code)
	}
	// 编排序:先停链,后 dump/restore,再起链(defer)。
	if f.stopCalls != 1 || f.startCalls != 1 {
		t.Fatalf("停/起链各一次: stop=%d start=%d", f.stopCalls, f.startCalls)
	}
	joined := strings.Join(f.cmds, "\n")
	if !strings.Contains(joined, "pg_ctl -D ") || !strings.Contains(joined, " start") {
		t.Fatalf("应 pg_ctl start 内部 pg: %s", joined)
	}
	if !strings.Contains(joined, "pg_dump --format=custom") || !strings.Contains(joined, srcDSNFor(f.srcDSN)) {
		t.Fatalf("应 pg_dump 源库: %s", joined)
	}
	if !strings.Contains(joined, "pg_restore --exit-on-error") {
		t.Fatalf("应 pg_restore: %s", joined)
	}
	if !strings.Contains(joined, " stop") {
		t.Fatalf("独立 pg 应交还(ctl stop): %s", joined)
	}
	// 目标库清位 DDL。
	if len(f.execSQLLog) != 2 || !strings.Contains(f.execSQLLog[0], "DROP DATABASE IF EXISTS cumora") ||
		f.execSQLLog[1] != "CREATE DATABASE cumora" {
		t.Fatalf("清位 DDL: %v", f.execSQLLog)
	}
	// 切链:toml → internal;旧 toml 留底;状态文件落盘。
	after, err := stackconfig.Load(filepath.Join(cfgDir, "stack.toml"))
	if err != nil || after.PG.Mode != stackconfig.ModeInternal {
		t.Fatalf("toml 应切 internal: %+v %v", after.PG, err)
	}
	backupDir := filepath.Join(cfg.Data.Home, "backups")
	if _, err := os.Stat(filepath.Join(backupDir, "stack.toml.premigrate")); err != nil {
		t.Fatalf("旧 toml 留底: %v", err)
	}
	st, err := loadMigrateState(filepath.Join(cfg.Data.Home, "migrate-pg.state.json"))
	if err != nil {
		t.Fatalf("状态文件: %v", err)
	}
	if st.SourceCounts["messages"] != 100 || st.TargetCounts["messages"] != 100 {
		t.Fatalf("行数入档: %+v", st)
	}
	if strings.Contains(st.SourceDSN, "secret") || !strings.Contains(st.SourceDSN, "***") {
		t.Fatalf("状态文件 DSN 应脱敏: %s", st.SourceDSN)
	}
}

// 行数不一致 = 阻断切链(数据完整性优先);defer 仍起链恢复服务。
func TestMigrateCountMismatchBlocksCutover(t *testing.T) {
	cfgDir, envFile, cfg := setupMigrate(t, nil)
	f := &fakeMigrate{
		srcCounts: map[string]int{"messages": 100, "convene_transcript": 5, "board_cards": 8,
			"document_snapshots": 3, "conversations": 20, "participants": 7},
		tgtCounts: map[string]int{"messages": 99, "convene_transcript": 5, "board_cards": 8,
			"document_snapshots": 3, "conversations": 20, "participants": 7},
		srcDSN: "postgres://u:secret@127.0.0.1:5432/cumora",
	}
	orig := newMigrateDeps
	newMigrateDeps = func(string) MigrateDeps { return f.deps() }
	t.Cleanup(func() { newMigrateDeps = orig })

	if code := cmdMigratePG(migrateArgs(cfgDir, envFile)); code != 2 {
		t.Fatalf("行数不一致应退 2: %d", code)
	}
	after, err := stackconfig.Load(filepath.Join(cfgDir, "stack.toml"))
	if err != nil || after.PG.Mode != stackconfig.ModeExternal {
		t.Fatalf("不应切链: %+v %v", after.PG, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Data.Home, "migrate-pg.state.json")); !os.IsNotExist(err) {
		t.Fatal("阻断时不应落状态文件")
	}
	if f.startCalls != 1 {
		t.Fatalf("失败也应起链恢复服务: %d", f.startCalls)
	}
}

// 幂等:已迁移 → no-op(不停链不 dump)。
func TestMigrateIdempotentNoop(t *testing.T) {
	cfgDir, envFile, cfg := setupMigrate(t, map[string]int{
		"messages": 1, "convene_transcript": 1, "board_cards": 1,
		"document_snapshots": 1, "conversations": 1, "participants": 1,
	})
	state := `{"completedAt":"2026-09-01T00:00:00Z","sourceDsn":"x","sourceCounts":{},"targetCounts":{},"backupFile":"/b"}`
	if err := os.WriteFile(filepath.Join(cfg.Data.Home, "migrate-pg.state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeMigrate{srcDSN: "postgres://u:secret@127.0.0.1:5432/cumora"}
	orig := newMigrateDeps
	newMigrateDeps = func(string) MigrateDeps { return f.deps() }
	t.Cleanup(func() { newMigrateDeps = orig })

	if code := cmdMigratePG(migrateArgs(cfgDir, envFile)); code != 0 {
		t.Fatalf("重跑应 no-op 退 0: %d", code)
	}
	if f.stopCalls != 0 || len(f.cmds) != 0 {
		t.Fatalf("no-op 不应动栈/跑工具: stop=%d cmds=%v", f.stopCalls, f.cmds)
	}
}

// dry-run:源探测+计划,零写零停。
func TestMigrateDryRun(t *testing.T) {
	cfgDir, envFile, _ := setupMigrate(t, map[string]int{
		"messages": 1, "convene_transcript": 1, "board_cards": 1,
		"document_snapshots": 1, "conversations": 1, "participants": 1,
	})
	f := &fakeMigrate{srcCounts: map[string]int{
		"messages": 1, "convene_transcript": 1, "board_cards": 1,
		"document_snapshots": 1, "conversations": 1, "participants": 1,
	}, srcDSN: "postgres://u:secret@127.0.0.1:5432/cumora"}
	orig := newMigrateDeps
	newMigrateDeps = func(string) MigrateDeps { return f.deps() }
	t.Cleanup(func() { newMigrateDeps = orig })

	if code := cmdMigratePG(migrateArgs(cfgDir, envFile, "--dry-run")); code != 0 {
		t.Fatalf("dry-run 应退 0: %d", code)
	}
	if f.stopCalls != 0 || len(f.cmds) != 0 || len(f.execSQLLog) != 0 {
		t.Fatalf("dry-run 零副作用: stop=%d cmds=%v ddl=%v", f.stopCalls, f.cmds, f.execSQLLog)
	}
	after, err := stackconfig.Load(filepath.Join(cfgDir, "stack.toml"))
	if err != nil || after.PG.Mode != stackconfig.ModeExternal {
		t.Fatal("dry-run 不应切链")
	}
}

// 源 DSN 缺失:窗口开之前就退。
func TestMigrateMissingSourceFailsFast(t *testing.T) {
	cfgDir, envFile, _ := setupMigrate(t, nil)
	emptyEnv := filepath.Join(filepath.Dir(envFile), "empty.env")
	if err := os.WriteFile(emptyEnv, []byte("GITHUB_CLIENT_ID=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeMigrate{}
	orig := newMigrateDeps
	newMigrateDeps = func(string) MigrateDeps { return f.deps() }
	t.Cleanup(func() { newMigrateDeps = orig })

	if code := cmdMigratePG(migrateArgs(cfgDir, emptyEnv)); code != 2 {
		t.Fatalf("无源应退 2: %d", code)
	}
	if f.stopCalls != 0 {
		t.Fatal("无源不应停链")
	}
}

// restore 失败:不切链、旧 toml 完好、仍起链。
func TestMigrateRestoreFailureKeepsOldChain(t *testing.T) {
	cfgDir, envFile, _ := setupMigrate(t, nil)
	f := &fakeMigrate{
		srcCounts: map[string]int{"messages": 1, "convene_transcript": 1, "board_cards": 1,
			"document_snapshots": 1, "conversations": 1, "participants": 1},
		srcDSN:      "postgres://u:secret@127.0.0.1:5432/cumora",
		failRestore: true,
	}
	orig := newMigrateDeps
	newMigrateDeps = func(string) MigrateDeps { return f.deps() }
	t.Cleanup(func() { newMigrateDeps = orig })

	if code := cmdMigratePG(migrateArgs(cfgDir, envFile)); code != 2 {
		t.Fatalf("restore 失败应退 2: %d", code)
	}
	after, err := stackconfig.Load(filepath.Join(cfgDir, "stack.toml"))
	if err != nil || after.PG.Mode != stackconfig.ModeExternal {
		t.Fatal("restore 失败不应切链")
	}
	if f.startCalls != 1 {
		t.Fatal("失败应起链恢复服务")
	}
}

func TestRedactDSN(t *testing.T) {
	if got := redactDSN("postgres://u:secret@127.0.0.1:5432/cumora"); strings.Contains(got, "secret") || !strings.Contains(got, "***") {
		t.Fatalf("URL 形态脱敏: %s", got)
	}
	if got := redactDSN("host=/run user=cumora password=abc dbname=cumora"); strings.Contains(got, "abc") || !strings.Contains(got, "password=***") {
		t.Fatalf("keyword 形态脱敏: %s", got)
	}
	if got := redactDSN("host=/run user=cumora dbname=x"); !strings.Contains(got, "dbname=x") {
		t.Fatalf("无密码原样: %s", got)
	}
}

func TestSSlDisabledForms(t *testing.T) {
	if got := sslDisabled("postgres://h/db"); got != "postgres://h/db?sslmode=disable" {
		t.Fatalf("URL 无参: %s", got)
	}
	if got := sslDisabled("postgres://h/db?x=1"); got != "postgres://h/db?x=1&sslmode=disable" {
		t.Fatalf("URL 带参: %s", got)
	}
	if got := sslDisabled("host=/run dbname=x"); got != "host=/run dbname=x sslmode=disable" {
		t.Fatalf("keyword: %s", got)
	}
	if got := sslDisabled("postgres://h/db?sslmode=require"); got != "postgres://h/db?sslmode=require" {
		t.Fatalf("显式 sslmode 不覆盖: %s", got)
	}
}

// srcDSNFor —— pg_dump 命令行里 DSN 经 sslDisabled 追参,断言用前缀即可。
func srcDSNFor(dsn string) string { return strings.Split(dsn, "?")[0] }
