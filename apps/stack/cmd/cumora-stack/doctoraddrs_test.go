package main

import (
	"net"
	"testing"

	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
)

// 评审 P0 回归锁:Name 是展示标签、Addr 必须是可拨号裸地址(net.Dial
// 直接消费);写反 = 所有机器 doctor 恒退 1。
func TestBuildDoctorAddrsDialable(t *testing.T) {
	for _, mode := range []string{stackconfig.ModeInternal, stackconfig.ModeExternal} {
		cfg := stackconfig.Defaults()
		cfg.PG.Mode = mode
		cfg.Redis.Mode = mode
		for _, a := range buildDoctorAddrs(cfg) {
			if _, _, err := net.SplitHostPort(a.Addr); err != nil {
				t.Errorf("[%s] %s.Addr 不可拨号(%v): %+v", mode, a.Name, err, a)
			}
			if a.Name == "" {
				t.Errorf("[%s] Addr %s 缺 Name 标签", mode, a.Addr)
			}
		}
	}
}

// 受管形态过滤 TCP 5432/6379 info 位;external 保留。
func TestBuildDoctorAddrsModeFiltering(t *testing.T) {
	cfg := stackconfig.Defaults()
	cfg.PG.Mode = stackconfig.ModeInternal
	cfg.Redis.Mode = stackconfig.ModeExternal
	addrs := buildDoctorAddrs(cfg)
	for _, a := range addrs {
		if a.Addr == "127.0.0.1:5432" {
			t.Error("受管 pg 不应再探 TCP 5432")
		}
		if a.Addr != "127.0.0.1:6379" && a.Kind == "info" {
			t.Errorf("external redis 应保留 6379 info 位: %+v", a)
		}
	}
	hasRedis := false
	for _, a := range addrs {
		if a.Addr == "127.0.0.1:6379" {
			hasRedis = true
		}
	}
	if !hasRedis {
		t.Error("external redis 的 6379 info 位丢失")
	}
}

// daemon.env 缺省双位置回退与 stackd 同规则(评审 P2)。
func TestDefaultDaemonEnvFileFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// 规范位与存量位都不在 → 存量位(行为零变)。
	if got := defaultDaemonEnvFile(); got != stackconfig.LegacyDaemonEnvPath() {
		t.Fatalf("无规范位应回退存量: %s", got)
	}
}
