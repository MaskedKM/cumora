// runtime 包 cli_domains —— agent 域子包的命令分发接线(#140 刀法):
// agent 内核不得 import 子包(防环),本包在构造时把 sub → 域方法闭包表
// 注入 agent.Service;每刀新域在此追加 case。
package runtime

import (
	"context"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/boards"
)

// wireDomainDispatch:boards(看板)面——kanban/card/claim/unclaim。
func wireDomainDispatch(core *agent.Service) {
	core.SetDomainDispatcher(func(sub string, ctx context.Context, p agent.Parsed) (agent.Result, bool) {
		d := boards.Domain{Service: core}
		switch sub {
		case "kanban":
			return d.CmdBoard(ctx, p), true
		case "card":
			return d.CmdCard(ctx, p), true
		case "claim":
			return d.CmdClaim(ctx, p, "claim"), true
		case "unclaim":
			return d.CmdClaim(ctx, p, "unclaim"), true
		}
		return agent.Result{}, false
	})
}
