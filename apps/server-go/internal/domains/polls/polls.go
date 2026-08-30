// domains/polls —— 投票 HTTP 面(#121,#117-a):人类经 UI 的建票/投票/
// 关闭。引擎在 internal/polls(与 agent CLI、过期清扫器同源);本包只做
// 鉴权门(requireCompany + 会话成员 404 遮蔽)、请求强转(String(x ?? ”)
// 语义)与 PollError→HTTP 状态映射。行为对齐 749863e router.ts 三路由。
package polls

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	engine "github.com/MaskedKM/cumora/apps/server-go/internal/polls"
)

// pollHttpError:引擎可预期错误带状态;其余 500(对齐 pollHttpError)。
func pollHttpError(w http.ResponseWriter, r *http.Request, err error) {
	if pe, ok := err.(*engine.PollError); ok {
		httpx.WriteError(w, pe.Status, pe.Msg)
		return
	}
	httpx.WriteInternalError(w, r, err)
}

// requireConversationMember:跨租户/不存在/非成员一律 404 'not found'
// (不可探测语义,同 TS)。
func requireConversationMember(w http.ResponseWriter, r *http.Request, db *sql.DB, uid, companyID, conversationID string) bool {
	var membersJSON string
	if db.QueryRowContext(r.Context(),
		`SELECT members::text FROM conversations WHERE id = $1 AND company_id = $2 LIMIT 1`,
		conversationID, companyID).Scan(&membersJSON) != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return false
	}
	var members []string
	_ = json.Unmarshal([]byte(membersJSON), &members)
	for _, m := range members {
		if m == uid {
			return true
		}
	}
	httpx.WriteError(w, http.StatusNotFound, "not found")
	return false
}

func CreatePoll(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return
	}
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&raw)
	keyAny := func(k string) any {
		var v any
		_ = json.Unmarshal(raw[k], &v)
		return v
	}
	// 强转对齐 TS:conversationId/question 走 String(x ?? ''),mode 仅
	// 'multi' 收敛,optionIds/optionIds 元素 String 化,expiresInMinutes
	// 仅 number 透传。
	conversationID := httpx.JSStringOrNullish(keyAny("conversationId"))
	if conversationID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "conversationId required")
		return
	}
	if !requireConversationMember(w, r, db, uid, companyID, conversationID) {
		return
	}
	var options []string
	if arr, ok := keyAny("options").([]any); ok {
		for _, o := range arr {
			options = append(options, httpx.JSStringOrNullish(o))
		}
	}
	mode := "single"
	if s, ok := keyAny("mode").(string); ok && s == "multi" {
		mode = "multi"
	}
	var expiresIn *float64
	if f, ok := keyAny("expiresInMinutes").(float64); ok {
		expiresIn = &f
	}
	created, perr := engine.Create(r.Context(), db, engine.CreateArgs{
		ConversationID: conversationID, CompanyID: companyID, AuthorID: uid,
		Question: httpx.JSStringOrNullish(keyAny("question")), Mode: mode,
		Options: options, ExpiresInMinutes: expiresIn,
	})
	if perr != nil {
		pollHttpError(w, r, perr)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"messageId": created.MessageID,
		"sequence":  created.Sequence,
		"poll":      created.Poll,
	})
}

func CastVote(db *sql.DB, w http.ResponseWriter, r *http.Request, messageId string) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return
	}
	messageID := messageId
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&raw)
	var arr []any
	_ = json.Unmarshal(raw["optionIds"], &arr)
	var optionIDs []string
	for _, x := range arr {
		// String(x ?? '') 后滤空(TS .map(String).filter(Boolean))。
		if s := httpx.JSStringOrNullish(x); s != "" {
			optionIDs = append(optionIDs, s)
		}
	}
	event, perr := engine.CastVote(r.Context(), db, engine.CastVoteArgs{
		MessageID: messageID, CompanyID: companyID, VoterParticipant: uid,
		VoterKind: "human", OptionIDs: optionIDs,
	})
	if perr != nil {
		pollHttpError(w, r, perr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tallies": event.Tallies, "poll": event.Poll})
}

func ClosePoll(db *sql.DB, w http.ResponseWriter, r *http.Request, messageId string) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return
	}
	event, perr := engine.ClosePoll(r.Context(), db, engine.CloseArgs{
		MessageID: messageId, CompanyID: companyID,
		ActorID: &uid, Reason: "manual",
	})
	if perr != nil {
		pollHttpError(w, r, perr)
		return
	}
	// 幂等关闭:closed=false + poll=null(TS !!event 形状)。
	closed := event != nil
	var poll any
	if event != nil {
		poll = event.Poll
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"closed": closed, "poll": poll})
}
