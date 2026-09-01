// chain —— stackd 的依赖链定义与顺序拉起(#282 PR-A)。
//
// 阶段 1 形态:pg/redis = External(探测等就绪,进程归系统级),
// sidecar/server/daemon = Managed(supervise 子进程)。#283 打包链落地
// 后,pg/redis 节点从 External 切 Managed 即可——引擎与顺序语义不变。
package chain

import (
	"context"
	"fmt"
	"time"

	"github.com/MaskedKM/cumora/apps/stack/internal/supervise"
)

type Mode string

const (
	// External —— 非受管依赖:轮询 Probe 直到就绪(如系统级 pg/redis)。
	External Mode = "external"
	// Managed —— supervise 子进程:spawn + 健康门(Probe 可作门)。
	Managed Mode = "managed"
)

// Node —— 链上一个节点。External 必填 Probe;Managed 必填 Child
// (Probe 非空时作为 Child.Gate 的缺省门)。
//
// Probe 的 ctx 义务:调用方带超时(External=ProbeTimeout,Managed=
// Child.GateTimeout),实现必须尊重 —— 一个阻塞不返回的 Probe 会把
// 整条 BringUp 挂死。
type Node struct {
	Name         string
	Mode         Mode
	Child        *supervise.Child
	Probe        func(ctx context.Context) error
	ProbeEvery   time.Duration // External 轮询间隔;0 = 250ms
	ProbeTimeout time.Duration // External 就绪总预算;0 = 60s
}

func (n Node) probeEvery() time.Duration {
	if n.ProbeEvery > 0 {
		return n.ProbeEvery
	}
	return 250 * time.Millisecond
}

func (n Node) probeTimeout() time.Duration {
	if n.ProbeTimeout > 0 {
		return n.ProbeTimeout
	}
	return 60 * time.Second
}

// BringUp —— 按序拉起整链:External 探测就绪 → Managed 起子进程过门。
// 任一节点失败立即返回(后续节点不起——依赖链语义);ctx 取消中止等待。
func BringUp(ctx context.Context, nodes []Node, m *supervise.Manager) error {
	for _, n := range nodes {
		switch n.Mode {
		case External:
			if err := waitExternal(ctx, n); err != nil {
				return err
			}
		case Managed:
			if n.Child == nil {
				return fmt.Errorf("chain: managed 节点 %s 缺 Child", n.Name)
			}
			child := *n.Child
			if child.Gate == nil && n.Probe != nil {
				child.Gate = n.Probe
			}
			if err := m.Start(child); err != nil {
				return fmt.Errorf("chain: managed %s 拉起失败: %w", n.Name, err)
			}
		default:
			return fmt.Errorf("chain: 节点 %s 未知模式 %q", n.Name, n.Mode)
		}
	}
	return nil
}

// waitExternal —— 轮询单个 External 节点到就绪。ticker 在本函数内
// 创建与销毁(不随节点数累积),错误统一带 chain: 前缀与节点名。
func waitExternal(ctx context.Context, n Node) error {
	if n.Probe == nil {
		return fmt.Errorf("chain: external 节点 %s 缺 Probe", n.Name)
	}
	pctx, cancel := context.WithTimeout(ctx, n.probeTimeout())
	defer cancel()
	tick := time.NewTicker(n.probeEvery())
	defer tick.Stop()
	var lastErr error
	for {
		lastErr = n.Probe(pctx)
		if lastErr == nil {
			return nil
		}
		if pctx.Err() == nil {
			select {
			case <-pctx.Done():
			case <-tick.C:
				continue
			}
		}
		// 到期或外层取消:区分归因。
		if ctx.Err() != nil {
			return fmt.Errorf("chain: external %s 等待被取消: %w", n.Name, ctx.Err())
		}
		return fmt.Errorf("chain: external %s 未在 %s 内就绪: %w",
			n.Name, n.probeTimeout(), lastErr)
	}
}
