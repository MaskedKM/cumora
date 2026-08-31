package daemon

import (
	"strings"
	"testing"
	"time"
)

const wtestMS = time.Millisecond

func newTestWatchdog(cfg turnWatchdogConfig, fired chan string) *activityWatchdog {
	return newActivityWatchdogWith(cfg, func(reason string) { fired <- reason })
}

// 首声层:Arm 后彻底沉默 → 在 firstOutput 窗口判死。
func TestWatchdogFiresOnStartupSilence(t *testing.T) {
	fired := make(chan string, 1)
	w := newTestWatchdog(turnWatchdogConfig{firstOutput: 40 * wtestMS, idle: 10 * time.Second, toolIdle: 10 * time.Second}, fired)
	w.Arm()
	defer w.Disarm()
	select {
	case r := <-fired:
		if !strings.Contains(r, "idle watchdog") {
			t.Fatalf("reason 缺 idle watchdog 标记: %s", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("首声窗口耗尽未判死")
	}
}

// ArmIdle(非首个 turn):跳过首声层——即使 firstOutput 很小也不判死。
func TestWatchdogArmIdleSkipsFirstOutput(t *testing.T) {
	fired := make(chan string, 1)
	w := newTestWatchdog(turnWatchdogConfig{firstOutput: 40 * wtestMS, idle: 10 * time.Second, toolIdle: 10 * time.Second}, fired)
	w.ArmIdle()
	defer w.Disarm()
	select {
	case r := <-fired:
		t.Fatalf("ArmIdle 不得启用首声层: %s", r)
	case <-time.After(300 * wtestMS):
	}
}

// 空闲层:持续出声不死;断流后在 idle 窗口判死。
func TestWatchdogActivityDefersThenFires(t *testing.T) {
	fired := make(chan string, 1)
	w := newTestWatchdog(turnWatchdogConfig{firstOutput: 80 * wtestMS, idle: 300 * wtestMS, toolIdle: 300 * wtestMS}, fired)
	w.Arm()
	// 持续出声 600ms(> idle 窗口)——不得判死。
	for i := 0; i < 12; i++ {
		w.Activity(false, false)
		time.Sleep(50 * wtestMS)
	}
	select {
	case r := <-fired:
		t.Fatalf("持续出声被误判死: %s", r)
	default:
	}
	// 断流 → idle 窗口内判死(2s 余量吸收调度抖动)。
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("断流后未在 idle 窗口判死")
	}
}

// 工具层:工具在飞换 toolBudget——短于 toolIdle 的静默不判死,超过才判。
func TestWatchdogToolBudgetExtends(t *testing.T) {
	fired := make(chan string, 1)
	w := newTestWatchdog(turnWatchdogConfig{firstOutput: 40 * wtestMS, idle: 100 * wtestMS, toolIdle: 400 * wtestMS}, fired)
	w.Arm()
	w.Activity(true, false) // 工具启动
	defer w.Disarm()
	select {
	case <-fired:
		t.Fatal("工具在飞按 idle 窗口误杀(应按 toolBudget)")
	case <-time.After(250 * wtestMS):
	}
	select {
	case r := <-fired:
		if !strings.Contains(r, "tool in flight") {
			t.Fatalf("工具层判死原因不含 tool in flight: %s", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("工具窗口耗尽未判死")
	}
}

// 工具态粘性(评审 #276 P2-2):工具在飞期间的普通事件(stderr 杂音/
// token 通知)只重置计时不降窗——idle 窗口过了仍不判死,须按 toolIdle。
func TestWatchdogToolStickyAcrossPlainEvents(t *testing.T) {
	fired := make(chan string, 1)
	w := newTestWatchdog(turnWatchdogConfig{firstOutput: 40 * wtestMS, idle: 200 * wtestMS, toolIdle: 500 * wtestMS}, fired)
	w.Arm()
	w.Activity(false, false) // 过首声
	w.Activity(true, false)  // 工具启动
	defer w.Disarm()
	// 工具在飞,普通事件每 80ms 一次直到 t=320ms。无粘性会在最后一次
	// 事件 + idle(200ms)= ~520ms 判死;有粘性须 toolIdle(500ms)。
	for i := 0; i < 4; i++ {
		time.Sleep(80 * wtestMS)
		w.Activity(false, false)
	}
	select {
	case r := <-fired:
		t.Fatalf("工具态被普通事件打回 idle 窗口(粘性失效): %s", r)
	case <-time.After(350 * wtestMS): // t≈670ms:> 320+200,只有无粘性才会已判死
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("工具窗口耗尽未判死")
	}
}

// 工具落地(正常路径)回常规窗口。
func TestWatchdogToolEndRestoresIdleWindow(t *testing.T) {
	fired := make(chan string, 1)
	w := newTestWatchdog(turnWatchdogConfig{firstOutput: 40 * wtestMS, idle: 100 * wtestMS, toolIdle: 500 * wtestMS}, fired)
	w.Arm()
	w.Activity(false, false)
	w.Activity(true, false) // 工具
	w.Activity(false, true) // 工具落地 → 回 idle 窗
	defer w.Disarm()
	select {
	case r := <-fired:
		if strings.Contains(r, "tool in flight") {
			t.Fatalf("工具已落地却按 toolBudget 判死: %s", r)
		}
	case <-time.After(250 * wtestMS):
		t.Fatal("回 idle 窗后未判死(idle=100ms,静默已 250ms)")
	}
}

// Disarm 后不再判死;fire 只触发一次(带护栏,回归时快速失败)。
func TestWatchdogDisarmAndFireOnce(t *testing.T) {
	fired := make(chan string, 1)
	w := newTestWatchdog(turnWatchdogConfig{firstOutput: 40 * wtestMS, idle: 10 * time.Second, toolIdle: 10 * time.Second}, fired)
	w.Arm()
	w.Activity(false, false)
	w.Disarm()
	select {
	case r := <-fired:
		t.Fatalf("Disarm 后仍判死: %s", r)
	case <-time.After(200 * wtestMS):
	}
	// 重新 Arm→静默→判死一次;判死后的迟滞 Activity 不得二次触发。
	w.Arm()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("重新 Arm 后未判死")
	}
	w.Activity(false, false)
	select {
	case r := <-fired:
		t.Fatalf("判死后二次触发: %s", r)
	case <-time.After(200 * wtestMS):
	}
}

// 迟滞 fire 防护(评审 #276 P1-1):旧窗口到期的瞬间来了新事件——迟到的
// fire 不得把刚被续命的会话判死。
func TestWatchdogStaleFireNoOp(t *testing.T) {
	fired := make(chan string, 1)
	// firstOutput 极小:几乎必然在 Activity 的同时已有到期回调在跑。
	w := newTestWatchdog(turnWatchdogConfig{firstOutput: 1 * wtestMS, idle: 500 * wtestMS, toolIdle: 500 * wtestMS}, fired)
	w.Arm()
	for i := 0; i < 50; i++ {
		w.Activity(false, false)
		time.Sleep(2 * wtestMS)
	}
	select {
	case r := <-fired:
		t.Fatalf("持续出声被迟滞 fire 误判死: %s", r)
	default:
	}
	w.Disarm()
}
