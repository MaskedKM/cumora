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

// 空闲层:持续出声不死;断流后在 idle 窗口判死。
func TestWatchdogActivityDefersThenFires(t *testing.T) {
	fired := make(chan string, 1)
	w := newTestWatchdog(turnWatchdogConfig{firstOutput: 40 * wtestMS, idle: 150 * wtestMS, toolIdle: 150 * wtestMS}, fired)
	w.Arm()
	// 持续出声 500ms(> idle 窗口)——不得判死。
	for i := 0; i < 10; i++ {
		w.Activity(false, false)
		time.Sleep(50 * wtestMS)
	}
	select {
	case r := <-fired:
		t.Fatalf("持续出声被误判死: %s", r)
	default:
	}
	// 断流 → idle 窗口内判死(给 2s 余量吸收调度抖动)。
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

// Disarm 后不再判死;fire 只触发一次。
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
	<-fired
	w.Activity(false, false)
	select {
	case r := <-fired:
		t.Fatalf("判死后二次触发: %s", r)
	case <-time.After(200 * wtestMS):
	}
}
