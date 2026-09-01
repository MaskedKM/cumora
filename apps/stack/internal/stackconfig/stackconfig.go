// stackconfig —— stack.toml 的单一事实源(#284,ADR 0005 §4)。
//
// 职责:机器事实(数据根/端口/socket/引擎发现/保留份数)的结构化承载,
// 严格校验(未知键/坏值即红,doctor 消费同一错误面);凭据绝不入内——
// 这是 toml 与 env 文件的分界线,import-env 依此分类。
//
// 优先级约定(全 CLI/守护一致):flag > env > stack.toml > 内置缺省。
// 本包只提供"toml 值或内置缺省",flag/env 覆盖在调用方组装。
package stackconfig

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// CurrentVersion —— schema 版本。不认识的版本 = 拒载(前向不兼容时新版本
// 号阻断旧二进制静默误读,而不是半懂不懂地跑)。
const CurrentVersion = 1

// Mode —— pg/redis 的供给形态。
const (
	ModeInternal = "internal" // stackd 拉起受管实例(unix socket,净机缺省)
	ModeExternal = "external" // 系统级服务,探测等就绪(存量部署导入缺省)
)

// Config —— stack.toml 全量 schema。字段即文档:机器事实,无凭据。
type Config struct {
	Version int           `toml:"version"`
	Data    DataConfig    `toml:"data"`
	Net     NetConfig     `toml:"net"`
	PG      ServiceConfig `toml:"pg"`
	Redis   ServiceConfig `toml:"redis"`
	Engines EnginesConfig `toml:"engines"`
	Stack   StackConfig   `toml:"stack"`
}

// DataConfig —— 数据落点。Home 之下派生全部状态目录(见派生方法)。
type DataConfig struct {
	Home string `toml:"home"`
	// UploadsDir 覆盖上传落点(缺省 <Home>/uploads);机器事实(env 键
	// CUMORA_UPLOADS_DIR 的 toml 之家)。
	UploadsDir string `toml:"uploads_dir,omitempty"`
}

// NetConfig —— 服务监听面。与 stackd 注入 server/sidecar 的 env 值同源。
type NetConfig struct {
	ServerAddr  string `toml:"server_addr"`  // CUMORA_GO_LISTEN(127.0.0.1:5181)
	SidecarPort int    `toml:"sidecar_port"` // YJS_SIDECAR_PORT(5182)
}

// ServiceConfig —— pg/redis 共用:只声明形态,位置全由 Data 派生。
type ServiceConfig struct {
	Mode string `toml:"mode"` // internal | external
}

// EnginesConfig —— 引擎发现附加目录(叠加在 internal/engdirs 扫描之上)。
type EnginesConfig struct {
	ExtraDirs []string `toml:"extra_dirs"`
}

// StackConfig —— 栈版本策略(#286 消费)。
type StackConfig struct {
	KeepReleases int `toml:"keep_releases"` // releases/ 保留份数(回滚深度)
}

// Defaults —— 无 toml 时的内置缺省(legacy 布局 = 本机生产现状)。
// pg/redis 缺省 external:没有 toml 的既有部署保持阶段 1 行为零变;
// 净机路径由 import-env 显式写 internal。
func Defaults() Config {
	return Config{
		Version: CurrentVersion,
		Data:    DataConfig{Home: homeDir(".local/share/cumora")},
		Net:     NetConfig{ServerAddr: "127.0.0.1:5181", SidecarPort: 5182},
		PG:      ServiceConfig{Mode: ModeExternal},
		Redis:   ServiceConfig{Mode: ModeExternal},
		Engines: EnginesConfig{ExtraDirs: []string{}},
		Stack:   StackConfig{KeepReleases: 3},
	}
}

// ===== 派生路径(Data 为源,单一计算点)=====

func (c Config) ReleasesDir() string { return filepath.Join(c.Data.Home, "releases") }
func (c Config) CurrentDir() string  { return filepath.Join(c.Data.Home, "current") }
func (c Config) PGDataDir() string   { return filepath.Join(c.Data.Home, "pgdata") }
func (c Config) RunDir() string      { return filepath.Join(c.Data.Home, "run") }
func (c Config) StateFile() string   { return filepath.Join(c.Data.Home, "stackd-state.json") }
func (c Config) UploadsDir() string {
	if c.Data.UploadsDir != "" {
		return c.Data.UploadsDir
	}
	return filepath.Join(c.Data.Home, "uploads")
}

// RedisSocket —— 受管 redis 的 unix socket(6379 被占,内置实例零 TCP)。
func (c Config) RedisSocket() string { return filepath.Join(c.RunDir(), "redis.sock") }

// InternalDSN —— 受管 pg 的连接串:socket-only(listen_addresses=”),
// trust-local(socket 目录 0700,凭据不落任何文件),db=cumora。
func (c Config) InternalDSN() string {
	return fmt.Sprintf("host=%s user=cumora dbname=cumora sslmode=disable", c.RunDir())
}

// AdminDSN —— 维护连接(postgres 库:CREATE DATABASE / 探活门)。
func (c Config) AdminDSN() string {
	return fmt.Sprintf("host=%s user=cumora dbname=postgres sslmode=disable", c.RunDir())
}

// InternalRedisURL —— 受管 redis 的 URL 面(go-redis ParseURL 的 unix:// 形态,
// server 的 REDIS_URL 注入值与 doctor/status 探测共用)。
func (c Config) InternalRedisURL() string { return "unix://" + c.RedisSocket() }

// ===== 载入与校验 =====

// Load —— 严格载入:未写出的字段继承内置缺省(手写文件允许省段),
// 未知键/坏值/版本不符均为硬错误(doctor 转红,stackd 拒启)。文件不
// 存在返回错误(调用方用 LoadOrDefaults 区分"未创建")。
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("stackconfig: 读 %s: %w", path, err)
	}
	c := Defaults()
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&c)
	if err != nil {
		return Config{}, fmt.Errorf("stackconfig: %s 不是合法 toml: %w", path, err)
	}
	// 严格面:undecoded = schema 之外的字段。键名漂移有静默断链前科(#272),
	// 未知键必须当场报红而不是默默忽略。
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return Config{}, fmt.Errorf("stackconfig: %s 含未知字段: %s(拼错或来自更新版本)", path, strings.Join(keys, ", "))
	}
	c.normalize()
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("stackconfig: %s 校验失败: %w", path, err)
	}
	return c, nil
}

// LoadOrDefaults —— 文件在则严格载入,不在则内置缺省。found 区分两条路径
// (doctor 对"未创建"只提示,对"坏文件"判红)。
func LoadOrDefaults(path string) (c Config, found bool, err error) {
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return Defaults(), false, nil
		}
		return Config{}, false, fmt.Errorf("stackconfig: stat %s: %w", path, statErr)
	}
	c, err = Load(path)
	return c, true, err
}

// normalize —— ~ 展开与可选字段补齐(校验前调用)。
func (c *Config) normalize() {
	c.Data.Home = expandHome(c.Data.Home)
	c.Data.UploadsDir = expandHome(c.Data.UploadsDir)
	for i, d := range c.Engines.ExtraDirs {
		c.Engines.ExtraDirs[i] = expandHome(d)
	}
}

// Validate —— 语义校验。错误信息面向设置页/doctor 直接展示。
func (c Config) Validate() error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("version = %d,本程序只认 %d(制品与配置来自不同代?)", c.Version, CurrentVersion)
	}
	if !filepath.IsAbs(c.Data.Home) {
		return fmt.Errorf("data.home %q 必须是绝对路径", c.Data.Home)
	}
	if c.Data.UploadsDir != "" && !filepath.IsAbs(c.Data.UploadsDir) {
		return fmt.Errorf("data.uploads_dir %q 必须是绝对路径", c.Data.UploadsDir)
	}
	if err := validateAddr(c.Net.ServerAddr); err != nil {
		return fmt.Errorf("net.server_addr: %w", err)
	}
	if c.Net.SidecarPort < 1 || c.Net.SidecarPort > 65535 {
		return fmt.Errorf("net.sidecar_port = %d 越界(1-65535)", c.Net.SidecarPort)
	}
	for name, mode := range map[string]string{"pg": c.PG.Mode, "redis": c.Redis.Mode} {
		if mode != ModeInternal && mode != ModeExternal {
			return fmt.Errorf("%s.mode = %q 未知(internal|external)", name, mode)
		}
	}
	for _, d := range c.Engines.ExtraDirs {
		if !filepath.IsAbs(d) {
			return fmt.Errorf("engines.extra_dirs 含非绝对路径 %q", d)
		}
	}
	if c.Stack.KeepReleases < 1 || c.Stack.KeepReleases > 10 {
		return fmt.Errorf("stack.keep_releases = %d 越界(1-10)", c.Stack.KeepReleases)
	}
	return nil
}

func validateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q 不是 host:port 形态: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("%q 缺 host", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q 端口越界", addr)
	}
	return nil
}

// ===== 落盘(import-env 生成用;Load 严格面保证往返一致)=====

// Save —— 写 stack.toml(0644:无凭据)。模板手写以带引导注释——严格 Load +
// 往返测试(Save→Load 等值)锁定模板与 schema 不漂移。
func Save(path string, c Config) error {
	c.normalize()
	if err := c.Validate(); err != nil {
		return fmt.Errorf("stackconfig: 拒绝写出非法配置: %w", err)
	}
	var b strings.Builder
	b.WriteString("# stack.toml —— Cumora Stack 机器事实(ADR 0005 §4)。\n")
	b.WriteString("# 由 `cumora-stack import-env` 生成;手工可改,未知字段/坏值会被拒载。\n")
	b.WriteString("# 凭据(token/key/DSN 密码)绝不写这里 —— 它们住在同目录 stack.env。\n\n")
	b.WriteString(fmt.Sprintf("version = %d\n\n", c.Version))
	b.WriteString("[data]\n")
	b.WriteString(fmt.Sprintf("home = %q\n", c.Data.Home))
	if c.Data.UploadsDir != "" {
		b.WriteString(fmt.Sprintf("uploads_dir = %q\n", c.Data.UploadsDir))
	}
	b.WriteString("\n[net]\n")
	b.WriteString(fmt.Sprintf("server_addr = %q\n", c.Net.ServerAddr))
	b.WriteString(fmt.Sprintf("sidecar_port = %d\n", c.Net.SidecarPort))
	b.WriteString("\n[pg]\n")
	b.WriteString(fmt.Sprintf("mode = %q\n", c.PG.Mode))
	b.WriteString("\n[redis]\n")
	b.WriteString(fmt.Sprintf("mode = %q\n", c.Redis.Mode))
	b.WriteString("\n[engines]\n")
	if len(c.Engines.ExtraDirs) > 0 {
		b.WriteString("extra_dirs = [\n")
		for _, d := range c.Engines.ExtraDirs {
			b.WriteString(fmt.Sprintf("  %q,\n", d))
		}
		b.WriteString("]\n")
	} else {
		b.WriteString("extra_dirs = []\n")
	}
	b.WriteString("\n[stack]\n")
	b.WriteString(fmt.Sprintf("keep_releases = %d\n", c.Stack.KeepReleases))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("stackconfig: 写 %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("stackconfig: 原子替换 %s: %w", path, err)
	}
	return nil
}

// DefaultPath —— stack.toml 的规范位(XDG_CONFIG_HOME/cumora/stack.toml)。
// 所有入口(CLI/stackd/wizard)共用这一个位置。
func DefaultPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "cumora", "stack.toml")
	}
	return filepath.Join(homeDir(".config"), "cumora", "stack.toml")
}

// StackEnvPath / DaemonEnvPath —— 规范 env 文件位(import-env 的产物、
// unit EnvironmentFile 的指向、stackd daemon-env 的缺省)。
func StackEnvPath() string {
	return filepath.Join(filepath.Dir(DefaultPath()), "stack.env")
}

func DaemonEnvPath() string {
	return filepath.Join(filepath.Dir(DefaultPath()), "daemon.env")
}

// LegacyDaemonEnvPath —— 存量布局(~/.cumora/daemon.env);规范位缺文件时
// 的回退,存量部署行为零变。
func LegacyDaemonEnvPath() string {
	return homeDir(".cumora", "daemon.env")
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

func homeDir(rel ...string) string {
	h, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(rel...)
	}
	return filepath.Join(append([]string{h}, rel...)...)
}
