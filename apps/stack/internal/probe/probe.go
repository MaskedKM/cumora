// probe —— cumora-stack 的外部世界探针层(#281)。
//
// doctor/status 的一切外部交互(pg/redis/systemd/HTTP/TCP/文件)都以
// Deps 上的函数字段表达:生产用 NewDeps() 装真实现,测试注入 fake——
// 单测不碰宿主 pg/redis,与 #146 runner 的"零 env 一次性栈"精神一致。
package probe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// PGInfo —— pg 探活结果。PgvectorAvailable 来自 pg_available_extensions:
// 为 false 时 server 的 vector 列无从建立,doctor 直接判红。
type PGInfo struct {
	Version           string
	PgvectorAvailable bool
}

// UnitState —— systemctl --user show 的最小面。Load=masked 是 Mint 类
// 假绿事故的显性化位(#281 票面要求 masked 单列,不与进程活性混谈)。
type UnitState struct {
	Load      string // loaded | masked | not-found | error
	Active    string // active | inactive | failed | activating | …
	Sub       string // running | dead | auto-restart | …
	Timestamp string // ExecMainStartTimestamp 原样;空 = 未启动过
}

// Deps —— 可注入探针集合。字段全部为函数,零方法:fake 只需覆写所需项。
type Deps struct {
	// PG 连库探活(server_version + pgvector 可用性)。dsn 为空串时由调用方兜底。
	PG func(ctx context.Context, dsn string) (PGInfo, error)
	// EnsureDatabase —— 幂等建库(受管 pg 首启钩子,#284):先查
	// pg_database 再 CREATE,已存在零动作。独立成探针字段是为了
	// 可注入(stackd 门在测试里不吃真 pgx 连接)。
	EnsureDatabase func(ctx context.Context, adminDSN, dbname string) error
	// Redis PING。
	Redis func(ctx context.Context, url string) error
	// Systemd 读取一个 user unit 的状态。systemctl 不存在/非 systemd 环境
	// 返回错误,由上层决定如何呈现。
	Systemd func(unit string) (UnitState, error)
	// HTTP GET,bearer 非空时携带。网络错误返回 (0, err);HTTP 4xx/5xx
	// 不是错误 —— 调用方按语义分辨(livez 503 = Redis 红的诚实信号)。
	HTTP func(url, bearer string) (int, error)
	// Dial TCP 探活(端口占用检查)。
	Dial func(addr string) error
	// LookPath 在 PATH + 额外目录里找可执行文件(引擎发现)。
	LookPath func(name string, extraDirs []string) (string, error)
	ReadFile func(path string) ([]byte, error)
	// Readlink 解析 symlink(报告 current 指向)。
	Readlink func(path string) (string, error)
}

// NewDeps —— 生产探针。
func NewDeps() Deps {
	client := &http.Client{Timeout: 3 * time.Second}
	return Deps{
		PG: func(ctx context.Context, dsn string) (PGInfo, error) {
			if dsn == "" {
				dsn = "postgres://localhost:5432/cumora"
			}
			// 与 server-go config.withSSLModeDisabled 平价:本机 pg 无 TLS,
			// pgx 默认 prefer 会握手失败。doctor 的 DSN 语义必须与被诊断的
			// 服务一致,否则会出现 doctor 绿/服务红(或反之)的假信号。
			dsn = withSSLModeDisabled(dsn)
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				return PGInfo{}, err
			}
			defer conn.Close(context.Background())
			var version string
			if err := conn.QueryRow(ctx, "SELECT version()").Scan(&version); err != nil {
				return PGInfo{}, err
			}
			if parts := strings.SplitN(version, " ", 3); len(parts) >= 2 {
				version = parts[0] + " " + parts[1] // "PostgreSQL 16.15 (Ubuntu …)" → "PostgreSQL 16.15"
			}
			var n int
			if err := conn.QueryRow(ctx,
				"SELECT count(*) FROM pg_available_extensions WHERE name = 'vector'").Scan(&n); err != nil {
				return PGInfo{}, err
			}
			return PGInfo{Version: version, PgvectorAvailable: n > 0}, nil
		},
		EnsureDatabase: func(ctx context.Context, adminDSN, dbname string) error {
			conn, err := pgx.Connect(ctx, adminDSN)
			if err != nil {
				return fmt.Errorf("EnsureDatabase 连接: %w", err)
			}
			defer conn.Close(context.Background())
			var n int
			if err := conn.QueryRow(ctx,
				"SELECT count(*) FROM pg_database WHERE datname = $1", dbname).Scan(&n); err != nil {
				return fmt.Errorf("EnsureDatabase 查询: %w", err)
			}
			if n > 0 {
				return nil
			}
			// 标识符不能参数化;dbname 来自装配层常量/受校验配置,加引号防御。
			if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, dbname)); err != nil {
				if strings.Contains(err.Error(), "already exists") {
					return nil // 竞态双建(重启窗口)按已就位处理
				}
				return fmt.Errorf("EnsureDatabase 建库: %w", err)
			}
			return nil
		},
		Redis: func(ctx context.Context, url string) error {
			if url == "" {
				url = "redis://localhost:6379"
			}
			opts, err := redis.ParseURL(url)
			if err != nil {
				return err
			}
			// 一次性 CLI 里也要 Close:连接池与 reaper goroutine 不随
			// Ping 返回回收,#282 stackd 长驻复用本探针层时是真泄漏。
			c := redis.NewClient(opts)
			defer c.Close()
			return c.Ping(ctx).Err()
		},
		Systemd: func(unit string) (UnitState, error) {
			out, err := exec.Command("systemctl", "--user", "show", unit,
				"--property=LoadState", "--property=ActiveState",
				"--property=SubState", "--property=ExecMainStartTimestamp").Output()
			if err != nil {
				return UnitState{}, fmt.Errorf("systemctl --user show %s: %w", unit, err)
			}
			st := UnitState{}
			for _, line := range strings.Split(string(out), "\n") {
				k, v, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				switch k {
				case "LoadState":
					st.Load = v
				case "ActiveState":
					st.Active = v
				case "SubState":
					st.Sub = v
				case "ExecMainStartTimestamp":
					st.Timestamp = v
				}
			}
			return st, nil
		},
		HTTP: func(url, bearer string) (int, error) {
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				return 0, err
			}
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			resp, err := client.Do(req)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()
			return resp.StatusCode, nil
		},
		Dial: func(addr string) error {
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				return err
			}
			return c.Close()
		},
		LookPath: func(name string, extraDirs []string) (string, error) {
			if p, err := exec.LookPath(name); err == nil {
				return p, nil
			}
			for _, dir := range extraDirs {
				p := filepath.Join(dir, name)
				if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
					return p, nil
				}
			}
			return "", fmt.Errorf("%s: not found in PATH nor %v", name, extraDirs)
		},
		ReadFile: os.ReadFile,
		Readlink: func(path string) (string, error) {
			return filepath.EvalSymlinks(path)
		},
	}
}

// ParseEnvFile —— systemd EnvironmentFile 兼容解析:KEY=VALUE 行,容忍
// 前导 export、成对引号、# 注释与空行。不做变量插值(unit 也不做)。
func ParseEnvFile(data []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if v == "" {
			out[k] = ""
			continue
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out
}

// withSSLModeDisabled —— server-go config 同名逻辑的平价拷贝:URL 未显式
// 指定 sslmode 时追加 disable。锁在两个模块间同步的锚点是本函数注释;
// server-go 侧改语义时此处必须跟(反向亦然)。
func withSSLModeDisabled(url string) string {
	if strings.Contains(url, "sslmode=") {
		return url
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "sslmode=disable"
}
