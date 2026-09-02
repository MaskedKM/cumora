// daemon 包 —— BYOA daemon 的 Go 骨架(#63),对齐(已退役 TS server
// 树中)agents/computer/daemon.ts 的协议面:配对、心跳、agent 同步、
// wake-stream 消费(SSE + 重连退避 + 轮询兜底)、run 生命周期上报、
// 会话恢复(session 落盘跨重启 resume)。引擎本体是 #64–#66;
// 本包只定义 EngineAdapter 接口 + 测试用 stub 引擎。
package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Version:构建期注入(-ldflags "-X …Version=v0.3.0");缺省 dev。
// CUMORA_VERSION 环境变量优先(对齐 resolveCurrentVersion 的 env 语义)。
var Version = "0.0.0-dev"

func currentVersion() string {
	if v := strings.TrimSpace(os.Getenv("CUMORA_VERSION")); v != "" {
		return v
	}
	return Version
}

// supervised:CUMORA_SUPERVISED=1(服务管理器拉起时置位,心跳上报)。
func supervised() bool { return os.Getenv("CUMORA_SUPERVISED") == "1" }

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}

func configDir() string   { return filepath.Join(homeDir(), ".cumora") }
func configPath() string  { return filepath.Join(configDir(), "computer.json") }
func agentsRoot() string  { return filepath.Join(configDir(), "agents") }
func sessionsDir() string { return filepath.Join(configDir(), "sessions") }
func runningPath() string { return filepath.Join(configDir(), "running.json") }

// DaemonConfig:~/.cumora/computer.json 的形状(对齐 TS)。
type DaemonConfig struct {
	ServerURL   string `json:"serverUrl"`
	ComputerID  string `json:"computerId"`
	DeviceToken string `json:"deviceToken"`
}

func loadConfig() (*DaemonConfig, error) {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return nil, err
	}
	var cfg DaemonConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func saveConfig(cfg *DaemonConfig) error {
	if err := os.MkdirAll(configDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), b, 0o600)
}

/* ───────── 间隔与超时(默认值对齐 daemon.ts;env 可覆盖以便测试) ───────── */

func envMS(name string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return def
}

// httpTimeout:短请求上限(TS:20s,CUMORA_HTTP_TIMEOUT_MS 可调,下限 1s)。
// 半开 socket 会让 promise 永不 settle——catch 看不到不抛错的挂死,一个
// 卡住的 socket 能拖死整机的 agent。wake-stream 是故意的长连接,不受此限。
func httpTimeout() time.Duration {
	t := envMS("CUMORA_HTTP_TIMEOUT_MS", 20_000*time.Millisecond)
	if t < time.Second {
		return time.Second // TS Math.max(1_000, …) 同下限
	}
	return t
}

const (
	agentPollDef     = 60 * time.Second        // agent 列表同步
	heartbeatDef     = 30 * time.Second        // computer 心跳
	inboxPollDef     = 20 * time.Second        // wake-stream 断流时的轮询兜底
	wakeDebounce     = 2500 * time.Millisecond // 唤醒合并窗(一阵爆发=一次大脑)
	runHeartbeat     = 60 * time.Second        // 长 turn 保活(防 10min 陈旧清扫)
	tokenRefreshSkew = 5 * time.Minute         // 到期前 5min 刷新 runtime token
	shutdownGraceDef = 15 * time.Second        // 优雅停机:等在飞 turn 落完
	streamStableMS   = 60_000                  // SSE 稳定阈值(稳定过→重连退避归零)
	// sseHealthGrace:#134 pollLoop 门控的活性宽限——服务端每 25s 一条
	// ping 注释(wakebus ssePingEvery),取 3 倍余量;窗口内有过任何行
	// 即健康,轮询兜底静默。
	sseHealthGrace = 75 * time.Second
	// healthyPollDef:#134 评审 P2 安全网——ping 由 wakebus 自身节拍器
	// 直发,服务端 Redis 断连时"聋但 ping 着"(Deliver 失败、ping 照
	// 流),门控会把这段误判为健康而完全静默。健康期低频安全网轮询封顶
	// 该场景的拾取延迟;20s→5min = 静默期压力降至 1/15。
	healthyPollDef = 5 * time.Minute
	maxLogBytes    = 20 << 20 // daemon.log 轮转阈值
	logRotateEvery = 5 * time.Minute
)

func agentPollInterval() time.Duration   { return envMS("CUMORA_AGENT_POLL_MS", agentPollDef) }
func heartbeatInterval() time.Duration   { return envMS("CUMORA_HEARTBEAT_MS", heartbeatDef) }
func inboxPollInterval() time.Duration   { return envMS("CUMORA_INBOX_POLL_MS", inboxPollDef) }
func healthyPollInterval() time.Duration { return envMS("CUMORA_HEALTHY_POLL_MS", healthyPollDef) }
func shutdownGrace() time.Duration       { return envMS("CUMORA_SHUTDOWN_GRACE_MS", shutdownGraceDef) }

// AgentInfo:GET /api/computers/me/agents 的行形状。
type AgentInfo struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Role         *string `json:"role"`
	SystemPrompt *string `json:"systemPrompt"`
	Engine       *string `json:"engine"`
	Model        *string `json:"model"`
	FastModel    *string `json:"fastModel"`
	// ChatRegister:#24 聊天体语域开关(human-audience 会话说人话)。
	// 服务端列 NOT NULL DEFAULT true;nil(旧版服务端/迁移空档)按开。
	ChatRegister *bool `json:"chatRegister"`
}

// chatRegisterOn:开关语义收口——nil/true = 开(#24 默认)。
func (a AgentInfo) chatRegisterOn() bool {
	return a.ChatRegister == nil || *a.ChatRegister
}

// detectHostName:配对上报的主机名(对齐 detectHostName 的有效语义:
// hostname 命令优先,空则 os.Hostname)。
func detectHostName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "My computer"
}
