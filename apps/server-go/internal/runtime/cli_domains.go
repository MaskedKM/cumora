// runtime 包 cli_domains —— agent 域子包的命令分发接线(#140 刀法):
// agent 内核不得 import 子包(防环),本包在构造时把 sub → 域方法闭包表
// 注入 agent.Service;每刀新域在此追加 case。
package runtime

import (
	"context"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/boards"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/calendar"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/email"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/mailbox"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/poll"
)

// wireDomainDispatch:域子包命令接线——boards(看板:kanban/card/
// claim/unclaim)、email(email/contacts)、mailbox(inbox/glance/ack/
// mute/follow)。
func wireDomainDispatch(core *agent.Service) {
	b := boards.Domain{Service: core}
	e := email.Domain{Service: core}
	m := mailbox.Domain{Service: core}
	c := calendar.Domain{Service: core}
	po := poll.Domain{Service: core}
	core.SetDomainDispatcher(func(sub string, ctx context.Context, p agent.Parsed) (agent.Result, bool) {
		switch sub {
		case "kanban":
			return b.CmdBoard(ctx, p), true
		case "card":
			return b.CmdCard(ctx, p), true
		case "claim":
			return b.CmdClaim(ctx, p, "claim"), true
		case "unclaim":
			return b.CmdClaim(ctx, p, "unclaim"), true
		case "email":
			return e.CmdEmail(ctx, p), true
		case "contacts":
			return e.CmdContacts(ctx, p), true
		case "inbox":
			return m.CmdInbox(ctx, p), true
		case "glance":
			return m.CmdGlance(ctx, p), true
		case "ack":
			return m.CmdAck(ctx, p), true
		case "mute":
			return m.CmdMute(ctx, p), true
		case "follow":
			return m.CmdFollow(ctx, p), true
		case "calendar":
			return c.CmdCalendar(ctx, p), true
		case "poll":
			return po.CmdPoll(ctx, p), true
		}
		return agent.Result{}, false
	})
}
