// daemon 包 watchdog —— #259 活性判死:按"还在不在出声"判,不按墙钟。
// 三层:
//
//	① 首声层:turn 开始后 firstOutput 窗口内无任何输出 → 判死(抓启动即
//	   挂/凭证坏在握手上的形态,即 multica 的 handshake 语义失活层);
//	② 空闲层:常规 idle 窗口无输出 → 判死(默认 30 分钟,grilling 共识——
//	   聊天型产品彻底安静 30 分钟基本可判死);
//	③ 工具在飞层:引擎在跑工具(commandExecution/tool_call 等)时换更大
//	   的 toolBudget 窗口——npm install/大编译是合法长静默,误杀即事故。
//
// 触发动作由各会话注入(结算在飞 turn + 杀进程,形态同既有
// CUMORA_TURN_TIMEOUT_MS 路径);墙钟保险保留、默认关,两者谁先触发谁
// 结算(settle 幂等)。stderr 输出也算"出声"(引擎在重试网络时吐 stderr
// 是活着的证据)。
package daemon

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// 活性配置(env 毫秒;0 = 关闭该层):
//
//	CUMORA_TURN_FIRST_OUTPUT_MS  默认 180000(3 分钟首声)
//	CUMORA_TURN_IDLE_MS          默认 1800000(30 分钟空闲;grilling #259)
//	CUMORA_TURN_TOOL_IDLE_MS     默认 2× idle(工具在飞加预算)
type turnWatchdogConfig struct {
	firstOutput time.Duration
	idle        time.Duration
	toolIdle    time.Duration
}

func watchdogEnvMS(name string, def int64) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return time.Duration(def) * time.Millisecond
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Duration(n) * time.Millisecond
	}
	return time.Duration(def) * time.Millisecond
}

func loadTurnWatchdogConfig() turnWatchdogConfig {
	idle := watchdogEnvMS("CUMORA_TURN_IDLE_MS", 30*60*1000)
	tool := watchdogEnvMS("CUMORA_TURN_TOOL_IDLE_MS", int64(2*idle/time.Millisecond))
	return turnWatchdogConfig{
		firstOutput: watchdogEnvMS("CUMORA_TURN_FIRST_OUTPUT_MS", 3*60*1000),
		idle:        idle,
		toolIdle:    tool,
	}
}

// activityWatchdog:单 turn 的活性看门狗。非并发嵌入会话结构——Arm/
// Activity/Disarm 可从任意 goroutine 调(泵/结算/定时器三方),自身持锁。
type activityWatchdog struct {
	onFire func(reason string) // 只触发一次
	cfg    turnWatchdogConfig

	mu     sync.Mutex
	timer  *time.Timer
	inTool bool
	fired  bool
	armed  bool
}

func newActivityWatchdog(onFire func(string)) *activityWatchdog {
	return newActivityWatchdogWith(loadTurnWatchdogConfig(), onFire)
}

func newActivityWatchdogWith(cfg turnWatchdogConfig, onFire func(string)) *activityWatchdog {
	return &activityWatchdog{onFire: onFire, cfg: cfg}
}

// Arm:turn 开始。首声层先行——窗口 firstOutput,出过声后切 idle/tool。
func (w *activityWatchdog) Arm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cfg.idle <= 0 && w.cfg.firstOutput <= 0 {
		return // 全关
	}
	w.fired = false
	w.inTool = false
	w.armed = true
	w.resetLocked(w.cfg.firstOutput)
}

// Activity:引擎出声(toolStart=进入工具在飞;任何非 toolStart 事件=离开)。
// toolEnd 保留在签名里作语义显式标记(调用方可传,效果同普通事件)。
func (w *activityWatchdog) Activity(toolStart, toolEnd bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.armed || w.fired {
		return
	}
	if toolStart {
		w.inTool = true
	} else {
		w.inTool = false
	}
	d := w.cfg.idle
	if w.inTool && w.cfg.toolIdle > d {
		d = w.cfg.toolIdle
	}
	if d <= 0 {
		return // idle 层关闭(仅首声层启用时):出声后不再计时
	}
	w.resetLocked(d)
}

// Disarm:turn 结算/进程死亡——停表。之后的 Activity 为 no-op。
func (w *activityWatchdog) Disarm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = false
	w.inTool = false
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

func (w *activityWatchdog) resetLocked(d time.Duration) {
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(d, w.fire)
}

// fire:窗口耗尽。只触发一次;reason 供失败分类(#262 认 engine-timeout)。
func (w *activityWatchdog) fire() {
	w.mu.Lock()
	if w.fired || !w.armed {
		w.mu.Unlock()
		return
	}
	w.fired = true
	w.armed = false
	w.timer = nil
	inTool := w.inTool
	w.mu.Unlock()
	reason := "engine idle watchdog: no engine output in window — aborted; session will respawn (--resume)"
	if inTool {
		reason = "engine idle watchdog: tool in flight went silent past tool budget — aborted; session will respawn (--resume)"
	}
	if w.onFire != nil {
		w.onFire(reason)
	}
}
