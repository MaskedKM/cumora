// migrate-pg —— 存量数据一次性迁入内置 pg(#285,ADR 0005 §5)。
//
// 窗口语义(参照阶段 1 逆序停机):
//
//	停链(单 unit 形态自动 stop/start;--stack-unit '' = 前台/沙箱形态
//	不触碰 systemd)→ 内部 pg bootstrap(幂等 initdb)→ 独立拉起(pg_ctl)
//	→ 备份源库(pg_dump -Fc,迁移前自动备份,源侧全程只读)
//	→ 恢复进内部库(pg_restore --exit-on-error)
//	→ 行数比对 smoke(核心表,不一致 = 阻断切链)
//	→ stack.toml pg.mode 切 internal(旧 toml 随备份目录留底)
//	→ 落迁移标记(幂等面)→ 独立 pg 停掉交还 stackd → 起链
//
// 不变量:源库全程只读;系统侧 pg 配置零改动;旧库不动,退役由用户
// 自行决定(见 docs/DEPLOY-STACK.md)。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackd"
)

// smokeTables —— 票面点名的核心面(消息/会诊转录/看板/yjs 快照)+ 两张
// 骨干表。行数不一致 = dump/restore 丢数据,宁可阻断也不带病切链。
var smokeTables = []string{
	"messages",
	"convene_transcript",
	"board_cards",
	"document_snapshots",
	"conversations",
	"participants",
}

// MigrateDeps —— 外部交互注入面(生产 NewMigrateDeps;测试逐项覆写,
// 单测不碰真 pg/systemd/pg 工具)。
type MigrateDeps struct {
	RunCmd    func(ctx context.Context, path string, args ...string) (string, error)
	CountRows func(ctx context.Context, dsn, table string) (int, error)
	ExecSQL   func(ctx context.Context, adminDSN, statement string) error
	PGAlive   func(ctx context.Context, adminDSN string) error
	// 栈 unit 控制(nil = 前台/沙箱形态,跳过)。
	StopStack  func() error
	StartStack func() error
}

// newMigrateDeps —— 注入缝(测试覆写;生产实现 NewMigrateDeps)。
var newMigrateDeps = NewMigrateDeps

func NewMigrateDeps(stackUnit string) MigrateDeps {
	d := probe.NewDeps()
	deps := MigrateDeps{
		RunCmd: func(ctx context.Context, path string, args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, path, args...)
			out, err := cmd.CombinedOutput()
			return string(out), err
		},
		CountRows: countRowsPG,
		ExecSQL:   execSQLPG,
		PGAlive: func(ctx context.Context, adminDSN string) error {
			_, err := d.PG(ctx, adminDSN)
			return err
		},
	}
	if stackUnit != "" {
		unit := stackUnit
		deps.StopStack = func() error { return systemd("stop", unit) }
		deps.StartStack = func() error { return systemd("start", unit) }
	}
	return deps
}

// MigrateState —— 幂等标记(在且未 --force = no-op;重跑天然安全)。
type MigrateState struct {
	CompletedAt  time.Time      `json:"completedAt"`
	SourceDSN    string         `json:"sourceDsn"` // 密码剥除
	SourceCounts map[string]int `json:"sourceCounts"`
	TargetCounts map[string]int `json:"targetCounts"`
	BackupFile   string         `json:"backupFile"`
}

func cmdMigratePG(args []string) int {
	fs := flag.NewFlagSet("migrate-pg", flag.ExitOnError)
	envFile := fs.String("env-file", envOr("CUMORA_ENV_FILE", defaultEnvFile()), "源 DATABASE_URL 所在 env 文件")
	configFile := fs.String("config-file", envOr("CUMORA_CONFIG_FILE", stackconfig.DefaultPath()), "stack.toml 路径")
	currentDir := fs.String("current-dir", envOr("CUMORA_CURRENT_DIR", ""), "release 制品目录(缺省 toml 派生)")
	backupDir := fs.String("backup-dir", "", "备份目录(缺省 <data>/backups)")
	force := fs.Bool("force", false, "重做迁移(重 dump + 重恢复;默认已迁移即 no-op)")
	dryRun := fs.Bool("dry-run", false, "只做源探测与计划,不 dump 不切换")
	jsonOut := fs.Bool("json", false, "JSON 输出")
	stackUnit := fs.String("stack-unit", envOr("CUMORA_STACKD_UNIT", "cumora.service"),
		"受管栈 unit(停链/起链);空串 = 不触碰 systemd(前台/沙箱形态)")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	deps := newMigrateDeps(*stackUnit)

	// 1) 配置面:toml 必须在且可载(迁移本身就是切 toml 的动作)。
	cfg, err := stackconfig.Load(*configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: stack.toml 非法或缺失(先 import-env): %v\n", err)
		return 2
	}
	if *currentDir == "" {
		*currentDir = cfg.CurrentDir()
	}
	if *backupDir == "" {
		*backupDir = filepath.Join(cfg.Data.Home, "backups")
	}
	statePath := filepath.Join(cfg.Data.Home, "migrate-pg.state.json")

	// 2) 源 DSN:env 文件 → OS env。缺 = 没有可迁的东西。
	srcDSN := dsnFromEnvFile(*envFile)
	if srcDSN == "" {
		srcDSN = os.Getenv("DATABASE_URL")
	}
	if srcDSN == "" {
		fmt.Fprintf(os.Stderr, "migrate-pg: 源 DATABASE_URL 缺失(%s 与 OS env 均无)—— 没有可迁移的存量库\n", *envFile)
		return 2
	}

	// 3) 幂等:已迁移 no-op(源侧永远只读,重跑无害;--force 重做)。
	if !*force {
		if st, err := loadMigrateState(statePath); err == nil {
			if *jsonOut {
				printJSON(map[string]any{"already": true, "state": st})
			} else {
				fmt.Printf("migrate-pg: 已于 %s 迁移过(备份 %s);重做加 --force\n",
					st.CompletedAt.Format("2006-01-02 15:04"), st.BackupFile)
			}
			return 0
		}
	}

	// 4) 制品面:迁移工具随载荷走。
	pgBin := filepath.Join(*currentDir, "pg", "bin")
	for _, tool := range []string{"postgres", "initdb", "pg_ctl", "pg_dump", "pg_restore"} {
		if _, err := os.Stat(filepath.Join(pgBin, tool)); err != nil {
			fmt.Fprintf(os.Stderr, "migrate-pg: %s 缺失于 %s(制品载荷不含迁移工具面?)\n", tool, pgBin)
			return 2
		}
	}

	// 5) 源探活 + 基线行数:连不上就别停链(不把窗口开在天灾上)。
	srcCounts := map[string]int{}
	for _, tbl := range smokeTables {
		n, err := deps.CountRows(ctx, srcDSN, tbl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrate-pg: 源表 %s 计数失败(源库连不上?): %v\n", tbl, err)
			return 2
		}
		srcCounts[tbl] = n
	}

	if *dryRun {
		printDryRun(cfg, srcDSN, srcCounts, *backupDir)
		return 0
	}

	// 6) 停链(数据静默窗口从这开始)。中断安全原则:后续任一步失败,
	//    旧链路数据零动,defer 起链恢复原状。
	if deps.StopStack != nil {
		fmt.Println("migrate-pg: 停链(迁移窗口开)")
		if err := deps.StopStack(); err != nil {
			fmt.Fprintf(os.Stderr, "migrate-pg: 停链失败,中止(栈未动数据未动): %v\n", err)
			return 2
		}
	}
	defer func() {
		if deps.StartStack != nil {
			if err := deps.StartStack(); err != nil {
				fmt.Fprintf(os.Stderr, "migrate-pg: 起链失败 —— 数据已迁移,手动 systemctl --user start %s: %v\n", *stackUnit, err)
			}
		}
	}()

	// 7) 内部 pg bootstrap + 独立拉起(stackd 已停时我们代管,完毕交还)。
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := stackd.EnsureInternalPG(pgBin, cfg.PGDataDir(), cfg.RunDir(), log); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: %v\n", err)
		return 2
	}
	weStarted := false
	if err := deps.PGAlive(ctx, cfg.AdminDSN()); err != nil {
		if out, err := deps.RunCmd(ctx, filepath.Join(pgBin, "pg_ctl"),
			"-D", cfg.PGDataDir(), "-o", fmt.Sprintf("-k %s -h ''", cfg.RunDir()),
			"-w", "-t", "60", "start"); err != nil {
			fmt.Fprintf(os.Stderr, "migrate-pg: 内部 pg 拉起失败: %v\n%s\n", err, out)
			return 2
		}
		weStarted = true
		defer func() {
			// 交还 stackd 前必须干净停掉(它要自己拉起同目录 postgres,
			// socket/postmaster.pid 冲突会让链式拉起红门)。
			if out, err := deps.RunCmd(ctx, filepath.Join(pgBin, "pg_ctl"),
				"-D", cfg.PGDataDir(), "-m", "fast", "-w", "stop"); err != nil {
				fmt.Fprintf(os.Stderr, "migrate-pg: 内部 pg 停止失败(stackd 接管会撞 postmaster.pid,手动处理): %v\n%s\n", err, out)
			}
		}()
	}

	// 8) 目标库清位:恢复物含全量 schema,预置 schema 只会撞车
	//    (DROP WITH FORCE 断掉残余连接;首迁 = 库不存在,容忍)。
	if err := deps.ExecSQL(ctx, cfg.AdminDSN(),
		`DROP DATABASE IF EXISTS cumora WITH (FORCE)`); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: 目标库清位失败: %v\n", err)
		return 2
	}
	if err := deps.ExecSQL(ctx, cfg.AdminDSN(), `CREATE DATABASE cumora`); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		fmt.Fprintf(os.Stderr, "migrate-pg: 建目标库失败: %v\n", err)
		return 2
	}

	// 9) 备份源库(迁移前自动备份;pg_dump 只读)。
	if err := os.MkdirAll(*backupDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: 建备份目录: %v\n", err)
		return 2
	}
	backupFile := filepath.Join(*backupDir, fmt.Sprintf("cumora-premigrate-%s.dump", time.Now().Format("20060102-150405")))
	fmt.Printf("migrate-pg: 备份源库 → %s\n", backupFile)
	if out, err := deps.RunCmd(ctx, filepath.Join(pgBin, "pg_dump"),
		"--format=custom", "--no-owner", "--no-privileges",
		"--dbname", sslDisabled(srcDSN), "--file", backupFile); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: pg_dump 失败(源库未动): %v\n%s\n", err, out)
		return 2
	}

	// 10) 恢复进内部库。
	fmt.Println("migrate-pg: 恢复进内置 pg …")
	if out, err := deps.RunCmd(ctx, filepath.Join(pgBin, "pg_restore"),
		"--exit-on-error", "--no-owner", "--no-privileges",
		"--dbname", cfg.InternalDSN(), backupFile); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: pg_restore 失败(源库未动,修后重跑 --force): %v\n%s\n", err, out)
		return 2
	}

	// 11) 行数比对 smoke:不一致 = 阻断切链(数据完整性优先于窗口时长)。
	tgtCounts := map[string]int{}
	for _, tbl := range smokeTables {
		n, err := deps.CountRows(ctx, cfg.InternalDSN(), tbl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrate-pg: 目标表 %s 计数失败: %v\n", tbl, err)
			return 2
		}
		tgtCounts[tbl] = n
	}
	for _, tbl := range smokeTables {
		if srcCounts[tbl] != tgtCounts[tbl] {
			fmt.Fprintf(os.Stderr, "migrate-pg: 行数不一致 %s: 源 %d ≠ 目标 %d —— 不切链(旧链路可用,重跑 --force)\n",
				tbl, srcCounts[tbl], tgtCounts[tbl])
			return 2
		}
	}

	// 12) 切链:toml pg.mode=internal(旧 toml 随备份目录留底)。
	if data, err := os.ReadFile(*configFile); err == nil {
		_ = os.WriteFile(filepath.Join(*backupDir, "stack.toml.premigrate"), data, 0o644)
	}
	cfg.PG.Mode = stackconfig.ModeInternal
	if err := stackconfig.Save(*configFile, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: 切 stack.toml 失败(数据已恢复,手工把 pg.mode 改 internal): %v\n", err)
		return 2
	}

	// 13) 落标记 → 汇报(defer 收尾:停内部 pg → 起链)。
	st := MigrateState{
		CompletedAt:  time.Now(),
		SourceDSN:    redactDSN(srcDSN),
		SourceCounts: srcCounts,
		TargetCounts: tgtCounts,
		BackupFile:   backupFile,
	}
	if data, err := json.MarshalIndent(st, "", "  "); err == nil {
		_ = os.WriteFile(statePath, append(data, '\n'), 0o600)
	}
	if weStarted {
		fmt.Println("migrate-pg: 内部 pg 交还 stackd(独立实例停,链式拉起接管)")
	}
	if *jsonOut {
		printJSON(map[string]any{"migrated": true, "state": st})
	} else {
		printMigrateSummary(st)
	}
	return 0
}

func loadMigrateState(path string) (MigrateState, error) {
	var st MigrateState
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}

func printDryRun(cfg stackconfig.Config, srcDSN string, srcCounts map[string]int, backupDir string) {
	fmt.Println("[migrate-pg dry-run]")
	fmt.Printf("  源库      %s\n", redactDSN(srcDSN))
	fmt.Printf("  目标      内置 pg(%s)\n", cfg.PGDataDir())
	fmt.Printf("  备份目录  %s\n", backupDir)
	fmt.Println("  源行数:")
	for _, tbl := range smokeTables {
		fmt.Printf("    %-22s %d\n", tbl, srcCounts[tbl])
	}
	fmt.Println("  动作: 停链 → 备份 → 恢复 → 行数比对 → toml 切 internal → 起链")
}

func printMigrateSummary(st MigrateState) {
	fmt.Println("[migrate-pg 完成]")
	fmt.Printf("  备份      %s(可独立恢复:pg_restore --dbname <目标> <file>)\n", st.BackupFile)
	fmt.Println("  切换      stack.toml pg.mode=internal(旧 toml 在备份目录)")
	fmt.Println("  行数比对:")
	for _, tbl := range smokeTables {
		fmt.Printf("    %-22s %d = %d\n", tbl, st.SourceCounts[tbl], st.TargetCounts[tbl])
	}
	fmt.Println("  旧库未动(全程只读);确认新栈稳定后可自行退役,见 docs/DEPLOY-STACK.md")
}

// countRowsPG / execSQLPG —— 迁移面的 pgx 直连(行数比对与 DDL 不经
// 探针层:探针面是只读探活,DDL 是迁移独有职责)。
func countRowsPG(ctx context.Context, dsn, table string) (int, error) {
	conn, err := pgx.Connect(ctx, sslDisabled(dsn))
	if err != nil {
		return 0, err
	}
	defer conn.Close(context.Background())
	var n int
	// 表名来自包内常量表,非用户输入;引号防御与 ensureDatabase 同例。
	if err := conn.QueryRow(ctx,
		fmt.Sprintf(`SELECT count(*) FROM public.%q`, table)).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func execSQLPG(ctx context.Context, adminDSN, statement string) error {
	conn, err := pgx.Connect(ctx, sslDisabled(adminDSN))
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, statement)
	return err
}

// redactDSN —— 标记文件里的源 DSN 剥密码(凭据不进状态文件/输出)。
// 手术式替换而非 url 重编码:url.UserPassword 会对 *** 做百分号转义
// (%2A%2A%2A),脱敏产物反而不可读。
func redactDSN(dsn string) string {
	if at := strings.Index(dsn, "@"); at > 0 {
		head := dsn[:at]
		if scheme := strings.Index(head, "://"); scheme >= 0 {
			if colon := strings.Index(head[scheme+3:], ":"); colon >= 0 {
				return head[:scheme+3+colon+1] + "***" + dsn[at:]
			}
		}
		return dsn
	}
	// keyword 形态:password=… 截到下一个空格。
	if i := strings.Index(dsn, "password="); i >= 0 {
		end := i + len("password=")
		for end < len(dsn) && dsn[end] != ' ' {
			end++
		}
		return dsn[:i] + "password=***" + dsn[end:]
	}
	return dsn
}

// dsnFromEnvFile —— 从 env 文件取 DATABASE_URL(不进 OS env,不回显)。
func dsnFromEnvFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return probe.ParseEnvFile(data)["DATABASE_URL"]
}

// sslDisabled —— server-go/withSSLModeDisabled 同语义(probe 未导出,
// 为两个调用点导出探针层 API 不值得;注释锁同步义务)。URL 与 keyword
// 两种 DSN 形态的分隔符不同(?& vs 空格)。
func sslDisabled(dsn string) string {
	if strings.Contains(dsn, "sslmode=") {
		return dsn
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if strings.Contains(dsn, "?") {
			return dsn + "&sslmode=disable"
		}
		return dsn + "?sslmode=disable"
	}
	return dsn + " sslmode=disable"
}
