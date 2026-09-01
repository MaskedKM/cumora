// migrate-pg 单测(#285):窗口编排序、重跑矩阵(internal 恒拒/幂等
// no-op/损坏拒)、行数与表数比对阻断、dry-run 零写、收养路径、DSN
// 脱敏(含密码带 @)。全 fake 依赖 —— 不碰真 pg/systemd/pg 工具。
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
	srcVersion  string // "" = "16.15"
	srcTableN   int
	tgtTableN   int // 0 = 同 srcTableN
	cmds        []string
	execSQLLog  []string
	pgAlive     bool // true = 内部 pg 已在跑(收养路径)
	started     bool // pg_ctl start 后为真(交还判活的状态机)
	stopCalls   int
	startCalls  int
	failRestore bool
}

func (f *fakeMigrate) deps() MigrateDeps {
	ver := f.srcVersion
	if ver == "" {
		ver = "16.15"
	}
	srcTableN := f.srcTableN
	if srcTableN == 0 {
		srcTableN = 67
	}
	tgtTableN := f.tgtTableN
	if tgtTableN == 0 {
		tgtTableN = srcTableN
	}
	d := MigrateDeps{
		RunCmd: func(_ context.Context, path string, args ...string) (string, error) {
			f.cmds = append(f.cmds, filepath.Base(path)+" "+strings.Join(args, " "))
			if filepath.Base(path) == "pg_ctl" && len(args) > 0 && args[len(args)-1] == "start" {
				f.started = true
			}
			// pg_dump 桩:把 --file 落盘(后续 rename 语义依赖它存在)。
			if filepath.Base(path) == "pg_dump" {
				for i, a := range args {
					if a == "--file" && i+1 < len(args) {
						_ = os.WriteFile(args[i+1], []byte("PGDMP-fake"), 0o600)
					}
				}
			}
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
		TableCount: func(_ context.Context, dsn string) (int, error) {
			if dsn == f.srcDSN {
				return srcTableN, nil
			}
			return tgtTableN, nil
		},
		ServerVersion: func(context.Context, string) (string, error) { return ver, nil },
		ExecSQL: func(_ context.Context, adminDSN, statement string) error {
			f.execSQLLog = append(f.execSQLLog, statement)
			return nil
		},
		PGAlive: func(context.Context, string) error {
			// pg_ctl start 之后即活(fake 世界的状态机;收养 = 初始即活)。
			if f.pgAlive || f.started {
				return nil
			}
			return fmt.Errorf("down")
		},
		StopStack:  func() error { f.stopCalls++; return nil },
		StartStack: func() error { f.startCalls++; return nil },
	}
	return d
}

func fullCounts(n int) map[string]int {
	m := map[string]int{}
	for _, t := range smokeTables {
		m[t] = n
	}
	return m
}

// setupMigrate —— 造一个可跑的迁移现场:toml(external)+ env 文件(DSN)+
// 制品桩 + 已 bootstrap 的 pgdata(PG_VERSION 在,跳 initdb)。
func setupMigrate(t *testing.T, mode string) (cfgDir, envFile string, cfg stackconfig.Config) {
	t.Helper()
	dir := t.TempDir()
	cfgDir = filepath.Join(dir, "config")
	dataHome := filepath.Join(dir, "data")
	current := filepath.Join(dir, "current")

	cfg = stackconfig.Defaults()
	cfg.Data.Home = dataHome
	cfg.PG.Mode = mode
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

func withFake(t *testing.T, f *fakeMigrate) {
	t.Helper()
	orig := newMigrateDeps
	newMigrateDeps = func(string) MigrateDeps { return f.deps() }
	t.Cleanup(func() { newMigrateDeps = orig })
}

func TestMigrateHappyPath(t *testing.T) {
	cfgDir, envFile, cfg := setupMigrate(t, stackconfig.ModeExternal)
	f := &fakeMigrate{srcCounts: fullCounts(7), srcDSN: "postgres://u:secret@127.0.0.1:5432/cumora"}
	withFake(t, f)

	if code := cmdMigratePG(migrateArgs(cfgDir, envFile)); code != 0 {
		t.Fatalf("迁移应退 0: %d", code)
	}
	if f.stopCalls != 1 || f.startCalls != 1 {
		t.Fatalf("停/起链各一次: stop=%d start=%d", f.stopCalls, f.startCalls)
	}
	joined := strings.Join(f.cmds, "\n")
	if !strings.Contains(joined, "pg_ctl -D ") || !strings.Contains(joined, " start") {
		t.Fatalf("应 pg_ctl start 内部 pg: %s", joined)
	}
	if !strings.Contains(joined, "pg_dump --format=custom") || !strings.Contains(joined, "127.0.0.1:5432") {
		t.Fatalf("应 pg_dump 源库: %s", joined)
	}
	if !strings.Contains(joined, "pg_restore --exit-on-error") {
		t.Fatalf("应 pg_restore: %s", joined)
	}
	// 交还:独立 pg 必停(stop 在 defer,起链前)。
	if !strings.Contains(joined, " stop") {
		t.Fatalf("独立 pg 应交还(ctl stop): %s", joined)
	}
	if len(f.execSQLLog) != 2 || !strings.Contains(f.execSQLLog[0], "DROP DATABASE IF EXISTS cumora") ||
		f.execSQLLog[1] != "CREATE DATABASE cumora" {
		t.Fatalf("清位 DDL: %v", f.execSQLLog)
	}
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
	if strings.Contains(st.SourceDSN, "secret") || !strings.Contains(st.SourceDSN, "***") {
		t.Fatalf("状态文件 DSN 应脱敏: %s", st.SourceDSN)
	}
	// 锁文件释放。
	if _, err := os.Stat(filepath.Join(cfg.Data.Home, "migrate-pg.lock")); !os.IsNotExist(err) {
		t.Fatal("锁文件应已释放")
	}
}

// 重跑矩阵根门:internal 形态恒拒(迁移后新写入绝不被静默销毁)。
func TestMigrateRefusesInternalMode(t *testing.T) {
	cfgDir, envFile, _ := setupMigrate(t, stackconfig.ModeInternal)
	f := &fakeMigrate{srcCounts: fullCounts(1), srcDSN: "postgres://u:s@h/db"}
	withFake(t, f)

	if code := cmdMigratePG(migrateArgs(cfgDir, envFile, "--force")); code != 2 {
		t.Fatalf("internal 形态应恒拒: %d", code)
	}
	if f.stopCalls != 0 || len(f.cmds) != 0 {
		t.Fatalf("恒拒零动作: stop=%d cmds=%v", f.stopCalls, f.cmds)
	}
}

// 收养路径:内部 pg 已在跑 → 不再 start,但结束仍交还(stop)。
func TestMigrateAdoptsAndHandsBack(t *testing.T) {
	cfgDir, envFile, _ := setupMigrate(t, stackconfig.ModeExternal)
	f := &fakeMigrate{srcCounts: fullCounts(1), srcDSN: "postgres://u:secret@127.0.0.1:5432/cumora", pgAlive: true}
	withFake(t, f)

	if code := cmdMigratePG(migrateArgs(cfgDir, envFile)); code != 0 {
		t.Fatalf("收养迁移应退 0: %d", code)
	}
	joined := strings.Join(f.cmds, "\n")
	if strings.Contains(joined, " start") {
		t.Fatalf("收养不应再 start: %s", joined)
	}
	if !strings.Contains(joined, "pg_ctl -D ") || !strings.Contains(joined, " stop") {
		t.Fatalf("收养也应交还(ctl stop): %s", joined)
	}
}

// 行数不一致 = 阻断切链;defer 仍起链恢复服务。
func TestMigrateCountMismatchBlocksCutover(t *testing.T) {
	cfgDir, envFile, cfg := setupMigrate(t, stackconfig.ModeExternal)
	tgt := fullCounts(7)
	tgt["messages"] = 6
	f := &fakeMigrate{srcCounts: fullCounts(7), tgtCounts: tgt, srcDSN: "postgres://u:secret@127.0.0.1:5432/cumora"}
	withFake(t, f)

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

// 表数不一致同样阻断(全库面比六表严)。
func TestMigrateTableCountMismatchBlocks(t *testing.T) {
	cfgDir, envFile, _ := setupMigrate(t, stackconfig.ModeExternal)
	f := &fakeMigrate{srcCounts: fullCounts(3), srcDSN: "postgres://u:secret@127.0.0.1:5432/cumora", srcTableN: 67, tgtTableN: 66}
	withFake(t, f)
	if code := cmdMigratePG(migrateArgs(cfgDir, envFile)); code != 2 {
		t.Fatalf("表数不一致应退 2: %d", code)
	}
}

// 幂等:external + 标记在 → no-op;--force 才重做。
func TestMigrateIdempotentNoopAndForce(t *testing.T) {
	cfgDir, envFile, cfg := setupMigrate(t, stackconfig.ModeExternal)
	state := `{"completedAt":"2026-09-01T00:00:00Z","sourceDsn":"x","sourceCounts":{},"targetCounts":{},"backupFile":"/b"}`
	if err := os.WriteFile(filepath.Join(cfg.Data.Home, "migrate-pg.state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeMigrate{srcCounts: fullCounts(1), srcDSN: "postgres://u:secret@127.0.0.1:5432/cumora"}
	withFake(t, f)

	if code := cmdMigratePG(migrateArgs(cfgDir, envFile)); code != 0 {
		t.Fatalf("标记在应 no-op 退 0: %d", code)
	}
	if f.stopCalls != 0 || len(f.cmds) != 0 {
		t.Fatalf("no-op 不应动栈/跑工具: stop=%d cmds=%v", f.stopCalls, f.cmds)
	}

	// --force 重做(external 形态:源库仍是权威,安全)。
	if code := cmdMigratePG(migrateArgs(cfgDir, envFile, "--force")); code != 0 {
		t.Fatalf("force 重做应退 0: %d", code)
	}
	if f.stopCalls != 1 || f.startCalls != 1 {
		t.Fatalf("force 应完整走窗: stop=%d start=%d", f.stopCalls, f.startCalls)
	}
}

// 标记损坏 = 状态未知 → 拒跑(不 fail-open)。
func TestMigrateCorruptStateRefused(t *testing.T) {
	cfgDir, envFile, cfg := setupMigrate(t, stackconfig.ModeExternal)
	if err := os.WriteFile(filepath.Join(cfg.Data.Home, "migrate-pg.state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeMigrate{srcCounts: fullCounts(1), srcDSN: "postgres://u:s@h/db"}
	withFake(t, f)

	if code := cmdMigratePG(migrateArgs(cfgDir, envFile, "--force")); code != 2 {
		t.Fatalf("损坏标记应拒: %d", code)
	}
	if f.stopCalls != 0 {
		t.Fatal("拒跑不应停链")
	}
}

// dry-run:绕过幂等门,计划总是可看;零写零停。
func TestMigrateDryRun(t *testing.T) {
	cfgDir, envFile, _ := setupMigrate(t, stackconfig.ModeExternal)
	f := &fakeMigrate{srcCounts: fullCounts(1), srcDSN: "postgres://u:secret@127.0.0.1:5432/cumora"}
	withFake(t, f)

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

// 源 DSN 缺失:窗口开之前就退(t.Setenv 钉死宿主 DATABASE_URL 干扰)。
func TestMigrateMissingSourceFailsFast(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cfgDir, _, _ := setupMigrate(t, stackconfig.ModeExternal)
	emptyEnv := filepath.Join(filepath.Dir(cfgDir), "empty.env")
	if err := os.WriteFile(emptyEnv, []byte("GITHUB_CLIENT_ID=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeMigrate{}
	withFake(t, f)

	if code := cmdMigratePG(migrateArgs(cfgDir, emptyEnv)); code != 2 {
		t.Fatalf("无源应退 2: %d", code)
	}
	if f.stopCalls != 0 {
		t.Fatal("无源不应停链")
	}
}

// 源大版本 ≠16:停链前拒绝。
func TestMigrateSourceMajorMismatchRefused(t *testing.T) {
	cfgDir, envFile, _ := setupMigrate(t, stackconfig.ModeExternal)
	f := &fakeMigrate{srcCounts: fullCounts(1), srcDSN: "postgres://u:s@h/db", srcVersion: "17.2"}
	withFake(t, f)
	if code := cmdMigratePG(migrateArgs(cfgDir, envFile)); code != 2 {
		t.Fatalf("大版本不匹配应退 2: %d", code)
	}
	if f.stopCalls != 0 {
		t.Fatal("版本预检失败不应停链")
	}
}

// restore 失败:不切链、旧 toml 完好、仍起链。
func TestMigrateRestoreFailureKeepsOldChain(t *testing.T) {
	cfgDir, envFile, _ := setupMigrate(t, stackconfig.ModeExternal)
	f := &fakeMigrate{srcCounts: fullCounts(1), srcDSN: "postgres://u:secret@127.0.0.1:5432/cumora", failRestore: true}
	withFake(t, f)

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
	cases := []struct{ in, mustNot, must string }{
		{"postgres://u:secret@127.0.0.1:5432/cumora", "secret", "***"},
		// 密码带 @(评审 P2:旧实现整串泄漏/尾段泄漏)。
		{"postgres://u:se@cret@127.0.0.1:5432/db", "cret", "***"},
		{"postgres://u:p@127.0.0.1:5432/db?x=1", "=p@", "***"},
		{"host=/run user=cumora password=abc dbname=x", "abc", "password=***"},
		{"postgres://u@127.0.0.1:5432/db", "", ""},
		{"host=/run user=cumora dbname=x", "", ""},
	}
	for _, c := range cases {
		got := redactDSN(c.in)
		if c.mustNot != "" && strings.Contains(got, c.mustNot) {
			t.Errorf("%s 泄漏 %q: %s", c.in, c.mustNot, got)
		}
		if c.must != "" && !strings.Contains(got, c.must) {
			t.Errorf("%s 缺 %q: %s", c.in, c.must, got)
		}
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
