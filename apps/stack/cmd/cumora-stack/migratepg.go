// migrate-pg —— 存量数据一次性迁入内置 pg(#285,ADR 0005 §5)。
//
// 窗口语义(参照阶段 1 逆序停机):
//
//	停链(单 unit 形态自动 stop/start;--stack-unit '' = 前台/沙箱形态
//	不触碰 systemd)→ 内部 pg bootstrap(幂等 initdb)→ 独立拉起(pg_ctl)
//	→ 备份源库(pg_dump -Fc,迁移前自动备份,源侧全程只读)
//	→ 恢复进内部库(pg_restore --exit-on-error)
//	→ 行数比对 smoke(核心表 + 全表计数,不一致 = 阻断切链)
//	→ stack.toml pg.mode 切 internal(旧 toml 随备份目录留底,一次性)
//	→ 落迁移标记(原子写;幂等面)→ 独立 pg 交还 stackd → 起链
//
// 不变量:源库全程只读;系统侧 pg 配置零改动;旧库不动,退役由用户
// 自行决定(见 docs/DEPLOY-STACK.md)。
//
// 重跑矩阵(评审 P1 的收口 —— 绝不静默销毁迁移后的新写入):
//
//	mode=internal      → 拒跑(已是受管形态;真要重迁走 runbook 手工路径)
//	mode=external 无标记 → 迁移(首迁或失败重试:失败不落标记,裸跑即重试)
//	mode=external 有标记 → no-op;--force 才重做(此时源库仍是权威,安全)
//	标记损坏           → 拒跑(状态未知;确认后删标记文件重跑)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackd"
)

// smokeTables —— 票面点名的核心面(消息/会诊转录/看板/yjs 快照)+ 登录
// 链身份表 + 两张骨干表。行数或全表计数不一致 = dump/restore 丢数据,
// 宁可阻断也不带病切链。
var smokeTables = []string{
	"messages",
	"convene_transcript",
	"board_cards",
	"document_snapshots",
	"user_identities",
	"sessions",
	"conversations",
	"participants",
}

// MigrateDeps —— 外部交互注入面(生产 NewMigrateDeps;测试逐项覆写,
// 单测不碰真 pg/systemd/pg 工具)。
type MigrateDeps struct {
	RunCmd        func(ctx context.Context, path string, args ...string) (string, error)
	CountRows     func(ctx context.Context, dsn, table string) (int, error)
	TableCount    func(ctx context.Context, dsn string) (int, error)
	ServerVersion func(ctx context.Context, dsn string) (string, error)
	ExecSQL       func(ctx context.Context, adminDSN, statement string) error
	PGAlive       func(ctx context.Context, adminDSN string) error
	// 栈 unit 控制(nil = 前台/沙箱形态,跳过)。
	StopStack  func() error
	StartStack func() error
}

func NewMigrateDeps(stackUnit string) MigrateDeps {
	d := probe.NewDeps()
	deps := MigrateDeps{
		RunCmd: func(ctx context.Context, path string, args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, path, args...)
			out, err := cmd.CombinedOutput()
			return string(out), err
		},
		CountRows:     countRowsPG,
		TableCount:    tableCountPG,
		ServerVersion: serverVersionPG,
		ExecSQL:       execSQLPG,
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

// MigrateState —— 幂等标记。
type MigrateState struct {
	CompletedAt  time.Time      `json:"completedAt"`
	SourceDSN    string         `json:"sourceDsn"` // 密码剥除
	SourceCounts map[string]int `json:"sourceCounts"`
	TargetCounts map[string]int `json:"targetCounts"`
	BackupFile   string         `json:"backupFile"`
}

// newMigrateDeps —— 注入缝(测试覆写;生产实现 NewMigrateDeps)。
var newMigrateDeps = NewMigrateDeps

func cmdMigratePG(args []string) int {
	fs := flag.NewFlagSet("migrate-pg", flag.ExitOnError)
	envFile := fs.String("env-file", envOr("CUMORA_ENV_FILE", defaultEnvFile()), "源 DATABASE_URL 所在 env 文件")
	configFile := fs.String("config-file", envOr("CUMORA_CONFIG_FILE", stackconfig.DefaultPath()), "stack.toml 路径")
	currentDir := fs.String("current-dir", envOr("CUMORA_CURRENT_DIR", ""), "release 制品目录(缺省 toml 派生)")
	backupDir := fs.String("backup-dir", "", "备份目录(缺省 <data>/backups;留最近 3 份)")
	force := fs.Bool("force", false, "重做迁移(仅当当前仍是 external 形态——internal 形态下恒拒)")
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
	// 受管形态恒拒(重跑矩阵的根门):迁移后的新写入在内置库里,重做
	// = DROP 掉它们再灌入早已冻结的源库快照 —— 静默数据丢失,绝不。
	if cfg.PG.Mode == stackconfig.ModeInternal {
		fmt.Fprintf(os.Stderr, "migrate-pg: 拒跑 —— stack.toml 已是 pg.mode=internal(受管形态)。真需重迁:见 docs/DEPLOY-STACK.md 的手工路径(pg_restore 备份)\n")
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

	// 3) 幂等面。dry-run 绕过 no-op(计划总是可看);标记损坏按
	//    "状态未知"拒跑,不 fail-open 到破坏性重做。
	stateExists := false
	if st, err := loadMigrateState(statePath); err == nil {
		stateExists = true
		if !*force && !*dryRun {
			if *jsonOut {
				printJSON(map[string]any{"already": true, "state": st})
			} else {
				fmt.Printf("migrate-pg: 存在 %s 的迁移标记(备份 %s;若为手工回滚后的重迁,加 --force)\n",
					st.CompletedAt.Format("2006-01-02 15:04"), st.BackupFile)
			}
			return 0
		}
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "migrate-pg: 迁移标记 %s 损坏(%v)—— 状态未知拒跑;确认从未迁移可删该文件后重跑\n", statePath, err)
		return 2
	}

	// 4) 制品面:迁移工具随载荷走。
	pgBin := filepath.Join(*currentDir, "pg", "bin")
	for _, tool := range []string{"postgres", "initdb", "pg_ctl", "pg_dump", "pg_restore"} {
		if _, err := os.Stat(filepath.Join(pgBin, tool)); err != nil {
			fmt.Fprintf(os.Stderr, "migrate-pg: %s 缺失于 %s(制品载荷不含迁移工具面?)\n", tool, pgBin)
			return 2
		}
	}

	// 5) 源探活 + 基线:连不上/版本不对就不停链(不把窗口开在天灾上)。
	ver, err := deps.ServerVersion(ctx, srcDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: 源库连不上,不动栈: %v\n", err)
		return 2
	}
	if major := pgMajor(ver); major != 0 && major != 16 {
		fmt.Fprintf(os.Stderr, "migrate-pg: 源库大版本 %s 与内置 pg16 不匹配(dump/restore 有坎)—— 停链前拒绝\n", ver)
		return 2
	}
	srcCounts := map[string]int{}
	for _, tbl := range smokeTables {
		n, err := deps.CountRows(ctx, srcDSN, tbl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrate-pg: 源表 %s 计数失败: %v\n", tbl, err)
			return 2
		}
		srcCounts[tbl] = n
	}
	srcTableCount, err := deps.TableCount(ctx, srcDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: 源库表计数失败: %v\n", err)
		return 2
	}

	if *dryRun {
		printDryRun(cfg, srcDSN, srcCounts, srcTableCount, *backupDir, stateExists)
		return 0
	}

	// 6) 互斥锁(评审 P3:双跑交错 DROP/CREATE 不可想象)。
	lockPath := filepath.Join(cfg.Data.Home, "migrate-pg.lock")
	lock, err := lockFile(lockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: %v\n", err)
		return 2
	}
	defer os.Remove(lockPath)
	defer lock.Close()

	// pg_ctl 的 -o 是空格分词的透传串,rundir 带空白会碎(评审 P3)。
	if strings.ContainsAny(cfg.RunDir(), " \t") {
		fmt.Fprintf(os.Stderr, "migrate-pg: run 目录 %s 含空白(pg_ctl -o 分词会碎)—— 改 stack.toml\n", cfg.RunDir())
		return 2
	}

	// 7) 停链(数据静默窗口从这开始)。中断安全原则:后续任一步失败,
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

	// 8) 内部 pg bootstrap + 独立拉起。收养(上一轮遗留的孤儿)与自发
	//    等价:结束前都 pg_ctl stop 交还 stackd(评审 P2:不交还必撞
	//    postmaster.pid)。清理用独立 ctx —— 主 ctx 超时后 defer 仍要
	//    能跑(评审 P2)。
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := stackd.EnsureInternalPG(pgBin, cfg.PGDataDir(), cfg.RunDir(), log); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: %v\n", err)
		return 2
	}
	if err := deps.PGAlive(ctx, cfg.AdminDSN()); err != nil {
		if out, err := deps.RunCmd(ctx, filepath.Join(pgBin, "pg_ctl"),
			"-D", cfg.PGDataDir(), "-o", fmt.Sprintf("-k %s -h ''", cfg.RunDir()),
			"-w", "-t", "60", "start"); err != nil {
			fmt.Fprintf(os.Stderr, "migrate-pg: 内部 pg 拉起失败: %v\n%s\n", err, out)
			return 2
		}
	} else {
		fmt.Println("migrate-pg: 内部 pg 已在运行(收养;结束时会交还 stackd)")
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer stopCancel()
		if err := deps.PGAlive(stopCtx, cfg.AdminDSN()); err == nil {
			if out, serr := deps.RunCmd(stopCtx, filepath.Join(pgBin, "pg_ctl"),
				"-D", cfg.PGDataDir(), "-m", "fast", "-w", "stop"); serr != nil {
				fmt.Fprintf(os.Stderr, "migrate-pg: 内部 pg 停止失败(stackd 接管会撞 postmaster.pid,手动处理): %v\n%s\n", serr, out)
			}
		}
	}()

	// 9) 目标库清位:恢复物含全量 schema,预置 schema 只会撞车
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

	// 10) 备份源库(迁移前自动备份;pg_dump 只读;.part 暂存 + 原子就位,
	//     截断的失败产物不会混进 backups/;留最近 3 份)。
	if err := os.MkdirAll(*backupDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: 建备份目录: %v\n", err)
		return 2
	}
	backupFile := filepath.Join(*backupDir, fmt.Sprintf("cumora-premigrate-%s.dump", time.Now().Format("20060102-150405")))
	part := backupFile + ".part"
	fmt.Printf("migrate-pg: 备份源库 → %s\n", backupFile)
	if out, err := deps.RunCmd(ctx, filepath.Join(pgBin, "pg_dump"),
		"--format=custom", "--no-owner", "--no-privileges",
		"--dbname", sslDisabled(srcDSN), "--file", part); err != nil {
		os.Remove(part)
		fmt.Fprintf(os.Stderr, "migrate-pg: pg_dump 失败(源库未动;修正后直接重跑): %v\n%s\n", err, out)
		return 2
	}
	if err := os.Rename(part, backupFile); err != nil {
		os.Remove(part)
		fmt.Fprintf(os.Stderr, "migrate-pg: 备份就位失败: %v\n", err)
		return 2
	}
	pruneBackups(*backupDir, 3)

	// 11) 恢复进内部库。
	fmt.Println("migrate-pg: 恢复进内置 pg …")
	if out, err := deps.RunCmd(ctx, filepath.Join(pgBin, "pg_restore"),
		"--exit-on-error", "--no-owner", "--no-privileges",
		"--dbname", cfg.InternalDSN(), backupFile); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: pg_restore 失败(源库未动;修正后直接重跑): %v\n%s\n", err, out)
		return 2
	}

	// 12) 行数比对 smoke:六+二表逐表 + 全表计数;不一致 = 阻断切链
	//     (数据完整性优先于窗口时长)。
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
			fmt.Fprintf(os.Stderr, "migrate-pg: 行数不一致 %s: 源 %d ≠ 目标 %d —— 不切链(旧链路可用;修正后直接重跑)\n",
				tbl, srcCounts[tbl], tgtCounts[tbl])
			return 2
		}
	}
	tgtTableCount, err := deps.TableCount(ctx, cfg.InternalDSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: 目标库表计数失败: %v\n", err)
		return 2
	}
	if srcTableCount != tgtTableCount {
		fmt.Fprintf(os.Stderr, "migrate-pg: 表数不一致: 源 %d ≠ 目标 %d —— 不切链(旧链路可用;修正后直接重跑)\n",
			srcTableCount, tgtTableCount)
		return 2
	}

	// 13) 切链:toml pg.mode=internal(旧 toml 一次性留底 —— 本命令在
	//     internal 形态下恒拒,此处必为首次,不存在覆写回退材料问题)。
	preSnap := filepath.Join(*backupDir, "stack.toml.premigrate")
	if _, err := os.Stat(preSnap); err != nil && os.IsNotExist(err) {
		if data, rerr := os.ReadFile(*configFile); rerr == nil {
			_ = os.WriteFile(preSnap, data, 0o644)
		}
	}
	cfg.PG.Mode = stackconfig.ModeInternal
	if err := stackconfig.Save(*configFile, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-pg: 切 stack.toml 失败(数据已恢复,手工把 pg.mode 改 internal): %v\n", err)
		return 2
	}

	// 14) 落标记(原子写;失败大声但不回滚 —— 数据已迁移,internal
	//     恒拒门保证了缺标记也不 fail-open)→ 汇报(defer 收尾:
	//     停内部 pg → 起链)。
	st := MigrateState{
		CompletedAt:  time.Now(),
		SourceDSN:    redactDSN(srcDSN),
		SourceCounts: srcCounts,
		TargetCounts: tgtCounts,
		BackupFile:   backupFile,
	}
	if data, merr := json.MarshalIndent(st, "", "  "); merr == nil {
		tmp := statePath + ".tmp"
		if werr := os.WriteFile(tmp, append(data, '\n'), 0o600); werr != nil {
			fmt.Fprintf(os.Stderr, "migrate-pg: 警告 —— 迁移标记写入失败(%v);internal 恒拒门兜底,不影响本次结果\n", werr)
		} else if rerr := os.Rename(tmp, statePath); rerr != nil {
			fmt.Fprintf(os.Stderr, "migrate-pg: 警告 —— 迁移标记落位失败(%v);internal 恒拒门兜底\n", rerr)
		}
	}
	if *jsonOut {
		printJSON(map[string]any{"migrated": true, "state": st, "tableCount": tgtTableCount, "sourceTableCount": srcTableCount})
	} else {
		printMigrateSummary(st, srcTableCount, tgtTableCount)
	}
	return 0
}

func loadMigrateState(path string) (MigrateState, error) {
	var st MigrateState
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("解析失败: %w", err)
	}
	if st.CompletedAt.IsZero() {
		return st, errors.New("缺 completedAt")
	}
	return st, nil
}

// lockFile —— O_EXCL 互斥(进程退出即释;残留锁 = 上一轮硬死,提示手工清)。
func lockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("已有 migrate-pg 在跑或残留锁 %s(硬死后遗留可删): %w", path, err)
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return f, nil
}

// pruneBackups —— 留最新 n 份 dump(按修改时间;premigrate toml 不动)。
func pruneBackups(dir string, n int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var dumps []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".dump") {
			dumps = append(dumps, filepath.Join(dir, e.Name()))
		}
	}
	if len(dumps) <= n {
		return
	}
	// 文件名即时间戳,字典序 = 时间序。
	for i := 0; i < len(dumps)-n; i++ {
		_ = os.Remove(dumps[i])
	}
}

func pgMajor(version string) int {
	// "16.15 (Ubuntu …)" / "16.15"
	first := strings.Fields(version)
	if len(first) == 0 {
		return 0
	}
	major, _, _ := strings.Cut(first[0], ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

func printDryRun(cfg stackconfig.Config, srcDSN string, srcCounts map[string]int, tableCount int, backupDir string, stateExists bool) {
	fmt.Println("[migrate-pg dry-run]")
	if stateExists {
		fmt.Println("  备注      存在迁移标记(实际执行需 --force)")
	}
	fmt.Printf("  源库      %s\n", redactDSN(srcDSN))
	fmt.Printf("  目标      内置 pg(%s)\n", cfg.PGDataDir())
	fmt.Printf("  备份目录  %s\n", backupDir)
	fmt.Println("  源行数:")
	for _, tbl := range smokeTables {
		fmt.Printf("    %-22s %d\n", tbl, srcCounts[tbl])
	}
	fmt.Printf("  源表数    %d\n", tableCount)
	fmt.Println("  动作: 停链 → 备份 → 恢复 → 行数+表数比对 → toml 切 internal → 起链")
}

func printMigrateSummary(st MigrateState, srcTableCount, tgtTableCount int) {
	fmt.Println("[migrate-pg 完成]")
	fmt.Printf("  备份      %s(可独立恢复:pg_restore --dbname <目标> <file>)\n", st.BackupFile)
	fmt.Println("  切换      stack.toml pg.mode=internal(旧 toml 在备份目录)")
	fmt.Println("  行数比对:")
	for _, tbl := range smokeTables {
		fmt.Printf("    %-22s %d = %d\n", tbl, st.SourceCounts[tbl], st.TargetCounts[tbl])
	}
	fmt.Printf("  表数比对  %d = %d\n", srcTableCount, tgtTableCount)
	fmt.Println("  旧库未动(全程只读);确认新栈稳定后可自行退役,见 docs/DEPLOY-STACK.md")
}

// countRowsPG / tableCountPG / serverVersionPG / execSQLPG —— 迁移面的
// pgx 直连(探针层是只读探活,DDL/计数是迁移独有职责)。
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

func tableCountPG(ctx context.Context, dsn string) (int, error) {
	conn, err := pgx.Connect(ctx, sslDisabled(dsn))
	if err != nil {
		return 0, err
	}
	defer conn.Close(context.Background())
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r'`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func serverVersionPG(ctx context.Context, dsn string) (string, error) {
	conn, err := pgx.Connect(ctx, sslDisabled(dsn))
	if err != nil {
		return "", err
	}
	defer conn.Close(context.Background())
	var v string
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&v); err != nil {
		return "", err
	}
	return v, nil
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

// redactDSN —— 标记/输出里的源 DSN 剥密码。URL 形态经 net/url 解析重拼
// (密码里的 @/:/? 由解析器正确切分);keyword 形态按 password= 词截断。
// 凭据不进状态文件与 stdout。
func redactDSN(dsn string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			// 解析失败的 URL 形态到不了这里(前置探活已拒),占位防御。
			return "postgres://<unparseable-dsn>"
		}
		if u.User == nil {
			return dsn
		}
		if _, has := u.User.Password(); !has {
			return dsn
		}
		u.User = url.User(u.User.Username())
		rest := u.String()
		// 在 authority 的 @ 前插 ***:重拼后 user 段已无密码,直接定位。
		if at := strings.Index(rest, "@"); at >= 0 {
			return rest[:at] + ":***" + rest[at:]
		}
		return rest
	}
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
