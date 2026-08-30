// wsx/presence —— 人在场计数(对齐 TS ws.ts 的 humanConnections):
// 同一用户的多标签/多设备连接合并计数,0→1 翻 'avail',末连接断开翻
// 'resting'。翻转经 PresenceSetter 落库并按租户广播(runtime.SetStatus)。
// agent 不走这里——他们有自己的运行时租约与自动过期。
package wsx

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type humanCounters struct {
	mu sync.Mutex
	n  map[string]int
}

// humanConnect:该用户新增一条 WS 连接;从 0→1 时翻 'avail'(异步,
// 失败只告警——在场翻转不该拖住握手路径,对齐 TS 的 void 语义)。
func (g *Gateway) humanConnect(userID string) {
	g.humans.mu.Lock()
	if g.humans.n == nil {
		g.humans.n = map[string]int{}
	}
	g.humans.n[userID]++
	first := g.humans.n[userID] == 1
	g.humans.mu.Unlock()
	if first {
		g.flipStatus(userID, "avail")
	}
}

// humanDisconnect:连接拆除;计数归零翻 'resting'。计数本就为 0 的
// 重复拆除是防御路径,不翻转。
func (g *Gateway) humanDisconnect(userID string) {
	g.humans.mu.Lock()
	n := g.humans.n[userID]
	if n <= 1 {
		delete(g.humans.n, userID)
	} else {
		g.humans.n[userID] = n - 1
	}
	g.humans.mu.Unlock()
	if n == 1 {
		g.flipStatus(userID, "resting")
	}
}

func (g *Gateway) flipStatus(userID, status string) {
	if g.presence == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := g.presence.SetStatus(ctx, userID, status); err != nil {
			slog.Warn("ws presence flip failed", "user", userID, "status", status, "err", err)
		}
	}()
}
