// chain 单测(#282 PR-A):顺序拉起、external 探测翻转、失败截链、
// managed 的 Probe→Gate 缺省嫁接。
package chain

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MaskedKM/cumora/apps/stack/internal/supervise"
)

func TestBringUpExternalThenManaged(t *testing.T) {
	m := supervise.New(supervise.Options{})
	defer m.Shutdown()

	// 顺序语义:external 先就绪,managed 后起 —— 用 gate 计数钉住
	// "managed 的门只在 external 过后才开始轮询"。
	gateCalls := 0
	var firstGateAt time.Time
	externalReadyAt := time.Now()

	err := BringUp(context.Background(), []Node{
		{Name: "pg", Mode: External,
			Probe: func(context.Context) error { return nil }},
		{Name: "svc", Mode: Managed,
			Child: &supervise.Child{Name: "svc", Path: "/bin/sh",
				Args: []string{"-c", "sleep 30"}},
			Probe: func(context.Context) error {
				gateCalls++
				if firstGateAt.IsZero() {
					firstGateAt = time.Now()
				}
				return nil // 首探即绿
			}},
	}, m)
	if err != nil {
		t.Fatalf("BringUp: %v", err)
	}
	if gateCalls < 1 {
		t.Fatal("managed 门应至少探一次")
	}
	if firstGateAt.Before(externalReadyAt) {
		t.Fatal("managed 门不应先于 external 就绪")
	}
	st := m.States()
	if len(st) != 1 || st[0].Name != "svc" || !st[0].Running {
		t.Fatalf("managed 子进程应就位: %+v", st)
	}
}

func TestExternalProbeFlipsReady(t *testing.T) {
	m := supervise.New(supervise.Options{})
	defer m.Shutdown()
	n := 0
	start := time.Now()
	err := BringUp(context.Background(), []Node{{
		Name: "pg", Mode: External,
		Probe: func(context.Context) error {
			n++
			if n < 3 {
				return errors.New("not yet")
			}
			return nil
		},
		ProbeEvery:   15 * time.Millisecond,
		ProbeTimeout: 2 * time.Second,
	}}, m)
	if err != nil {
		t.Fatalf("第三次探测应就绪: %v", err)
	}
	if n != 3 {
		t.Fatalf("应恰好探 3 次,实得 %d", n)
	}
	if time.Since(start) > time.Second {
		t.Fatal("轮询节奏应遵循 ProbeEvery")
	}
}

func TestExternalTimeoutAbortsChain(t *testing.T) {
	m := supervise.New(supervise.Options{})
	defer m.Shutdown()
	later := &supervise.Child{Name: "later", Path: "/bin/sh", Args: []string{"-c", "sleep 30"}}
	err := BringUp(context.Background(), []Node{
		{Name: "pg", Mode: External,
			Probe:        func(context.Context) error { return errors.New("永远红") },
			ProbeEvery:   15 * time.Millisecond,
			ProbeTimeout: 100 * time.Millisecond},
		{Name: "svc", Mode: Managed, Child: later},
	}, m)
	if err == nil {
		t.Fatal("external 超时应报错")
	}
	if st := m.States(); len(st) != 0 {
		t.Fatalf("截链后后续 managed 不应启动: %+v", st)
	}
}

func TestManagedProbeBecomesGate(t *testing.T) {
	// Probe 嫁接为缺省 Gate:门红到底 → Start 报错(门超时语义)。
	m := supervise.New(supervise.Options{})
	defer m.Shutdown()
	err := BringUp(context.Background(), []Node{{
		Name: "svc", Mode: Managed,
		Child: &supervise.Child{Name: "svc", Path: "/bin/sh",
			Args:        []string{"-c", "sleep 30"},
			GateTimeout: 100 * time.Millisecond, GateEvery: 20 * time.Millisecond},
		Probe: func(context.Context) error { return errors.New("gate red") },
	}}, m)
	if err == nil {
		t.Fatal("门超时应使 BringUp 失败")
	}
}
