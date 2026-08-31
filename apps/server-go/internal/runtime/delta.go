// runtime 包 delta —— /runtime/message-delta(#210):daemon 把引擎产出
// 的文本前缀按块上报,服务端按租户广播 message.delta(只上屏不入库)。
// 终局仍是 /runtime/cli reply 的 MessageNew:前端按 (conversationId,
// authorId) 收口换真消息,delta 态天然幂等——done=true 或 message.new
// 先到都各自退场,乱序/重复无害。
package runtime

import (
	"net/http"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// deltaFrameCap:单帧 delta 的长度帽(UTF-16 码元,与 httpx.UTF16Cap 同
// 算法)。daemon 侧已做块级合并,超帽即失控信号——截断保广播面,拒绝
// 会把已产出前缀整个丢掉(下一帧仍会来,截断的最坏结果是前端气泡少一
// 截尾巴,终局 message.new 兜底)。
const deltaFrameCap = 16 * 1024

// handleMessageDelta:#210 流式增量上报面。authorId 恒取 JWT(被
// compromise 的 daemon 冒充不了别人);目标会话须属 token 租户且本
// agent 是成员(与 notices 同款成员门——delta 虽只是瞬态上屏,也不能
// 让任意 token 往别人的会话里喷帧)。companyId 取 token 租户,token 无
// 租户时按会话解析;两者皆空则不发(空 companyId 会被 wsx 桥拒路由,
// 发布即死帧)。
func (s *Service) handleMessageDelta(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	conversationID := bodyStr(body, "conversationId")
	messageID := bodyStr(body, "messageId")
	delta := bodyStr(body, "delta")
	if conversationID == "" || messageID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "conversationId, messageId required")
		return
	}
	sequence := 0
	if f, ok := bodyFloat(body, "sequence"); ok {
		sequence = int(f)
	}
	done := bodyBool(body, "done")

	member, err := s.IsConversationMember(r.Context(), conversationID, agentID, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if !member {
		httpx.WriteError(w, http.StatusForbidden, "not a member of that conversation")
		return
	}
	tag := companyID
	if tag == nil {
		// token 无租户(agent 未入伙的边缘形态)→ 按会话解析;解析不出
		// 租户则无事可做——桥不路由无租户事件。
		resolved, e := s.GetConversationCompanyId(r.Context(), conversationID)
		if e != nil {
			httpx.WriteInternalError(w, r, e)
			return
		}
		tag = resolved
	}
	if tag != nil {
		events.MessageDelta(r.Context(), *tag, conversationID, messageID, agentID,
			sliceUTF16(delta, deltaFrameCap), sequence, done)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
