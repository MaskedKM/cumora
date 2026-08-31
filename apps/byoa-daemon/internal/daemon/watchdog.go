// daemon 包 watchdog —— #259 活性判死:按"还在不在出声"判,不按墙钟。
// 三层:
//
//	① 首声层:会话首个 turn 开始后 firstOutput 窗口内无任何输出 → 判死
//	   (会话级语义,对齐 multica handshake 层——抓的是 spawn 即挂/凭证坏
//	   在握手;持久会话"首声"= 首个模型往返,大上下文 prefill 可达分钟级,
//	   故仅首个 turn 启用且默认窗给到 10min,后续轮只用空闲层);
//	② 空闲层:常规 idle 窗口无输出 → 判死(默认 30 分钟,grilling 共识——
//	   聊天型产品彻底安静 30 分钟基本可判死);
//	③ 工具在飞层:引擎在跑工具(commandExecution/tool_call 等)时换更大
//	   的 toolBudget 窗口——npm install/大编译是合法长静默,误杀即事故。
//	   工具态有粘性:普通事件只重置计时不降窗,唯工具落地(tool_result/
//	   item completed)才回常规窗口(grok 无显式工具落地信号,工具态保持
//	   到轮结算——方向是晚杀不误杀)。
//
// 触发动作由各会话注入(结算在飞 turn + 杀进程);墙钟保险
// CUMORA_TURN_TIMEOUT_MS 保留、默认关,两者谁先触发谁结算(幂等)。
// stderr 输出也算"出声"(引擎在重试网络时吐 stderr 是活着的证据)。
// 配置在会话构造时读一次(env 改动需重孵化会话才生效)。idle=0 关闭
// 空闲层(连带工具层;首声层独立配置)。
package daemon

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// 活性配置(env 毫秒;0 = 关闭该层):
//
//	CUMORA_TURN_FIRST_OUTPUT_MS  默认 600000(10 分钟首声;大上下文 prefill 余量)
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
		firstOutput: watchdogEnvMS("CUMORA_TURN_FIRST_OUTPUT_MS", 10*60*1000),
		idle:        idle,
		toolIdle:    tool,
	}
}

// activityWatchdog:单 turn 的活性看门狗。非并发嵌入会话结构——Arm/
// Activity/Disarm 可从任意 goroutine 调(泵/结算/定时器三方),自身持锁。
//
// 迟滞 fire 防护:Go 的 Timer.Stop 挡不住"已到期、回调已在跑"的旧窗口。
// 每次布表/Disarm 自增代数,fire 回调核对捕获代数——旧窗口的迟到 fire
// 一律 no-op,绝不误杀刚被新事件续命的健康会话。
type activityWatchdog struct {
	onFire func(reason string) // 只触发一次
	cfg    turnWatchdogConfig

	mu     sync.Mutex
	timer  *time.Timer
	gen    uint64 // 布表代数:reset/Disarm 自增,fire 按捕获代数核对
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

// Arm:会话首个 turn 开表——首声层(firstOutput)先行,出过声后切 idle/tool。
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

// ArmIdle:非首个 turn 开表——跳过首声层直接空闲窗(大上下文 prefill 的
// 常规静默不是病;首声层只负责"会话起来就僵"的形态)。
func (w *activityWatchdog) ArmIdle() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cfg.idle <= 0 {
		return
	}
	w.fired = false
	w.inTool = false
	w.armed = true
	w.resetLocked(w.cfg.idle)
}

// Activity:引擎出声。三态:toolStart=进入工具在飞(窗口换 toolIdle);
// toolEnd=工具落地(回常规窗口);plain=普通事件(只重置计时,保持当前
// 层级——工具在飞时的 stderr 杂音/token 通知不得把大预算打回 idle)。
func (w *activityWatchdog) Activity(toolStart, toolEnd bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.armed || w.fired {
		return
	}
	switch {
	case toolStart:
		w.inTool = true
	case toolEnd:
		w.inTool = false
	}
	if w.cfg.idle <= 0 {
		return // 空闲层关闭:出声后不再计时(仅首声层时)
	}
	d := w.cfg.idle
	if w.inTool && w.cfg.toolIdle > d {
		d = w.cfg.toolIdle
	}
	w.resetLocked(d)
}

// Disarm:turn 结算/进程死亡——停表。之后的 Activity 为 no-op。
func (w *activityWatchdog) Disarm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = false
	w.inTool = false
	w.gen++ // 迟到的旧窗口 fire 一律作废
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

func (w *activityWatchdog) resetLocked(d time.Duration) {
	w.gen++
	g := w.gen
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(d, func() { w.fire(g) })
}

// fire:窗口耗尽且代数未过期。只触发一次;reason 供失败分类(#262 认
// engine-timeout)。onFire 在锁外调用(会话侧要拿自己的锁)。
func (w *activityWatchdog) fire(gen uint64) {
	w.mu.Lock()
	if w.fired || !w.armed || gen != w.gen {
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
