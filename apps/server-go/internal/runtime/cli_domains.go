// runtime 包 cli_domains —— agent 域子包的命令分发接线(#140 刀法):
// agent 内核不得 import 子包(防环),本包在构造时把 sub → 域方法闭包表
// 注入 agent.Service;每刀新域在此追加 case。
package runtime

import (
	"context"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/avatar"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/boards"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/calendar"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/doc"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/email"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/mailbox"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/poll"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/ship"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/skills"
	"github.com/MaskedKM/cumora/apps/server-go/internal/agent/tools"
)

// wireDomainDispatch:域子包命令接线——boards(看板:kanban/card/
// claim/unclaim)、email(email/contacts)、mailbox(inbox/glance/ack/
// mute/follow)、calendar/poll、doc/ship、avatar/image/skills、
// tools(react/dm/pull-group/palette)。
func wireDomainDispatch(core *agent.Service) {
	b := boards.Domain{Service: core}
	e := email.Domain{Service: core}
	m := mailbox.Domain{Service: core}
	c := calendar.Domain{Service: core}
	po := poll.Domain{Service: core}
	d := doc.Domain{Service: core}
	sh := ship.Domain{Service: core}
	av := avatar.Domain{Service: core}
	tl := tools.Domain{Service: core}
	sk := skills.Domain{Service: core}
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
		case "doc":
			return d.CmdDoc(ctx, p), true
		case "ship":
			return sh.CmdShip(ctx, p), true
		case "avatar":
			return av.CmdAvatar(ctx, p), true
		case "image":
			return av.CmdImage(ctx, p), true
		case "skills":
			return sk.CmdSkills(ctx, p), true
		case "react":
			return tl.RunTool(ctx, "react", p), true
		case "dm":
			return tl.RunTool(ctx, "dm_with", p), true
		case "pull-group":
			return tl.RunTool(ctx, "pull_group", p), true
		case "palette":
			return tl.RunTool(ctx, "palette", p), true
		}
		return agent.Result{}, false
	})
}

// GenerateAgentAvatar:启动/管理面的头像生成引导(avatar 域实装,
// main.go 与 domains/agents·devtools 挂载回调用)的壳面代理。
func (s *Service) GenerateAgentAvatar(ctx context.Context, agentID, tenant string) (string, error) {
	return (&avatar.Domain{Service: s.Service}).GenerateAgentAvatar(ctx, agentID, tenant)
}
