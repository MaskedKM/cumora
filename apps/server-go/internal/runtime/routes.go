// runtime 包 routes —— /runtime/* HTTP 面(对齐 已退役 TS server 的 agents/runtime/
// server.ts):BYOA daemon 专属端点,逐一手 AgentRuntimeClient 方法。认证:
// 每请求携带 agent-runtime JWT(Authorization: Bearer);agentId 一律取自
// token 而非请求体——被 compromise 的 daemon 冒充不了别人的 agent。
// 挂载于 /runtime(不嵌 /api:cookie 中间件会拒掉这些请求,也不该让
// pod 共享人类会话 cookie 路径)。
//
// #60 范围注:/runtime/cli(runCli 世界动作命令面,cli.ts ≈6100 行)与
// /agenda 的 remote-classify 分支(OpenAI/适配器 Responses 调用)拆至后
// 续票;remote 分支按 TS 分类器故障语义走确定性回退(断供期等价行为)。
package runtime

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
	"github.com/MaskedKM/cumora/apps/server-go/internal/costing"
	"github.com/MaskedKM/cumora/apps/server-go/internal/obs"

	reg "github.com/MaskedKM/cumora/apps/server-go/internal/computers"
	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/runtime"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"

	"github.com/MaskedKM/cumora/apps/server-go/internal/sched"
)

// Mount:runtime tag(34 路由)走契约生成物(#187 批次 9)。34 个接口
// 方法均为 `s.auth(s.handleX)` 薄包装 —— handler 体零触碰,鉴权语义
// (Bearer agent-runtime JWT + panic 兜底)与逐路由包装完全同源。
func (s *Service) Mount(mux *http.ServeMux) {
	_ = contract.HandlerFromMux(&Server{Svc: s}, mux)
}

// Server:runtime tag 的 ServerInterface 实现 —— 34 个薄包装到
// *Service 的 s.auth(s.handleX)。独立类型而非直接挂 *Service:后者
// 经嵌入 *agent.Service 已有 LoadInbox/LoadContext/LoadClimate/
// LoadFaces 同名业务方法(daemon/内部调用面,签名不同),接口方法名
// 与之冲突。
type Server struct{ Svc *Service }

var _ contract.ServerInterface = (*Server)(nil)

/* ───────── ServerInterface 薄包装(34 条,体在下方 handle* 不动) ───────── */

func (h *Server) WakeStream(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleWakeStream)(w, r)
}

func (h *Server) RuntimeCli(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleCli)(w, r)
}

func (h *Server) LoadPersona(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handlePersona)(w, r)
}

func (h *Server) ResolveConversationCompany(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleConversationCompanyId)(w, r)
}

func (h *Server) LoadInbox(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleInbox)(w, r)
}

func (h *Server) InboxTriagePayload(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleInboxTriagePayload)(w, r)
}

func (h *Server) LoadAgenda(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleAgenda)(w, r)
}

func (h *Server) AgendaVerdict(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleAgendaVerdict)(w, r)
}

func (h *Server) MemoryQuery(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleMemoryQuery)(w, r)
}

func (h *Server) LoadContext(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleContext)(w, r)
}

func (h *Server) LoadClimate(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleClimate)(w, r)
}

func (h *Server) LoadSkills(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleSkills)(w, r)
}

func (h *Server) LoadFaces(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleFaces)(w, r)
}

func (h *Server) LoadSystemPrompt(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleSystemPrompt)(w, r)
}

func (h *Server) LoadRoster(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleRoster)(w, r)
}

func (h *Server) ReportStatus(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleStatus)(w, r)
}

func (h *Server) StatusHeartbeat(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleStatusHeartbeat)(w, r)
}

func (h *Server) RuntimeTyping(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleTyping)(w, r)
}

func (h *Server) StartRun(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleCreateRun)(w, r)
}

func (h *Server) RecordEvent(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleRecordEvent)(w, r)
}

func (h *Server) RecordTriage(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleRecordTriage)(w, r)
}

func (h *Server) RecordLlmCall(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleLlmCalls)(w, r)
}

func (h *Server) RunHeartbeat(w http.ResponseWriter, r *http.Request, runId string) {
	h.Svc.auth(h.Svc.handleRunHeartbeat)(w, r)
}

func (h *Server) FinishRun(w http.ResponseWriter, r *http.Request, runId string) {
	h.Svc.auth(h.Svc.handleRunFinish)(w, r)
}

func (h *Server) BusyHeartbeat(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleBusyHeartbeat)(w, r)
}

func (h *Server) BusyClear(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleBusyClear)(w, r)
}

func (h *Server) ThinkingMark(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleThinkingMark)(w, r)
}

func (h *Server) ThinkingUnmark(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleThinkingUnmark)(w, r)
}

func (h *Server) ThinkingPeek(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleThinkingPeek)(w, r)
}

func (h *Server) WorklogClaim(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleWorklogClaim)(w, r)
}

func (h *Server) WorklogRelease(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleWorklogRelease)(w, r)
}

func (h *Server) WorklogPeek(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleWorklogPeek)(w, r)
}

func (h *Server) RuntimeMarkRead(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleMarkRead)(w, r)
}

func (h *Server) PostNotice(w http.ResponseWriter, r *http.Request) {
	h.Svc.auth(h.Svc.handleNotices)(w, r)
}

// auth:Bearer agent-runtime JWT → claims 注入;失败 401。
func (s *Service) auth(next func(w http.ResponseWriter, r *http.Request, agentID string, companyID *string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			httpx.WriteError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		token := strings.TrimSpace(header[len("Bearer "):])
		agentID, companyID, ok := reg.VerifyAgentToken(token)
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		// withAgent:handler panic → 500(对齐 TS withAgent 的 try/catch),
		// 不让单请求炸掉整个进程。
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("[runtime] handler panicked", "method", r.Method, "path", r.URL.Path, "panic", rec)
				// 500 豁免(#214):panic recover 面无 error 对象;固定文案
				// 对齐 TS withAgent 的 catch 形状。
				httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next(w, r, agentID, companyID)
	}
}

// readJSON:读体 → map;空/坏体给空 map(handler 自行判定必填字段)。
// readJSON:TS express.json({limit:'34mb'}) 语义(#94)——坏体/超体 400
// 'invalid JSON body'(此前吞成 {} 会让无必填字段端点静默 200 默认值);
// EOF/空体沿用 {} 语义(TS 同样放行)。返回 (body, ok);ok=false 时
// 响应已写完,调用方直返。
func readJSON(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body := map[string]any{}
	if r.Body == nil {
		return body, true
	}
	// 上限对齐 TS 全局挂载(index.ts:117 express.json({limit:'34mb'}),
	// 在 /runtime 挂载之前生效;mirror 测试的 4mb 只是 harness 同形,
	// 不影响语义。评审 MINOR1:4mb 会把 /runtime/llm-calls 的大 extras
	// 截成 400 丢台账——34mb 是 production 真语义。
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 34<<20))
	if err := dec.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]any{}, true
		}
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return nil, false
	}
	return body, true
}

func bodyStr(body map[string]any, key string) string {
	v, _ := body[key].(string)
	return v
}

func bodyBool(body map[string]any, key string) bool {
	v, _ := body[key].(bool)
	return v
}

func bodyFloat(body map[string]any, key string) (float64, bool) {
	v, ok := body[key].(float64)
	return v, ok
}

// strPtrOfRaw:TS `x ?? null` 的可选串语义——键存在且为串(含空串)→指针,
// 否则 nil。
func strPtrOfRaw(body map[string]any, key string) *string {
	if v, ok := body[key].(string); ok {
		return &v
	}
	return nil
}

func bodyStrSlice(body map[string]any, key string) []string {
	raw, ok := body[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// sliceUTF16:TS String.slice(0,n) 按 UTF-16 码元(#94;cli_read 的
// utf16Slice 已随 agent 包拆出 #140,此处直用 httpx.UTF16Cap 同算法)。
func sliceUTF16(s string, n int) string { return httpx.UTF16Cap(s, n) }

// uuidHex:httpx.UUIDHex 的本包别名(#140 observability 拆包后,cli 面
// 的既有调用点零改动)。
func uuidHex() string { return httpx.UUIDHex() }

/* ───────── wake-stream / cli ───────── */

// handleWakeStream:SSE 长响应——服务端把 wake/steer 事件推给 daemon。
func (s *Service) handleWakeStream(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	if s.Bus == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "wake bus unavailable")
		return
	}
	s.Bus.Attach(agentID, w, r.Context())
}

// handleCli:世界动作命令面(#89)。daemon 的 cumora shim 把 argv POST
// 到这里;JWT 钉死身份——剥净调用方 --as 后注入 --as <sub>(防御纵深:
// parseArgs 取最后一次出现,不剥就被冒充),再交 RunCli 分发。
func (s *Service) handleCli(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	// TS 语义:argv 非数组才 400;数组里的非字符串元素被过滤而非拒收。
	rawArgv, ok := body["argv"].([]any)
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "argv (string[]) required")
		return
	}
	argv := make([]string, 0, len(rawArgv))
	for _, a := range rawArgv {
		if str, isStr := a.(string); isStr {
			argv = append(argv, str)
		}
	}
	res := s.RunCli(r.Context(), agent.BuildRuntimeArgv(agentID, argv))
	ok2, text, exitCode, sideEffects := res.HTTPShape()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"text":        text,
		"exitCode":    exitCode,
		"ok":          ok2,
		"sideEffects": sideEffects,
	})
}

/* ───────── 读面 ───────── */

func personaJSON(p *agent.Persona) map[string]any {
	if p == nil {
		return nil
	}
	return map[string]any{
		"id":        p.ID,
		"name":      p.Name,
		"role":      p.Role,
		"style":     p.Style,
		"model":     p.Model,
		"companyId": p.CompanyID,
	}
}

func (s *Service) handlePersona(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	p, err := s.GetPersona(r.Context(), agentID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"persona": personaJSON(p)})
}

func (s *Service) handleConversationCompanyId(w http.ResponseWriter, r *http.Request, _ string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	conversationID := bodyStr(body, "conversationId")
	if conversationID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "conversationId required")
		return
	}
	companyID, err := s.GetConversationCompanyId(r.Context(), conversationID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	var out any
	if companyID != nil {
		out = *companyID
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"companyId": out})
}

// handleInbox:默认把本次浮出的行按会话推进"已见"边界(调用方会把它们
// 展示给 agent:daemon 的 snapshotUnread → brief → 大脑)。探测型调用
// (maybeSteer 的"要不要 steer"、引擎故障目标查找)传 ?probe=1 跳过推进
// ——那些路径未必把行注入会话,推进会让基线越过大脑实际所见图,
// 重复碰撞漏过 cmdReply 预检(bram 看到 Iris 的"1"事故)。
func (s *Service) handleInbox(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	rows, err := s.LoadInbox(r.Context(), agentID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	probe := strings.TrimSpace(r.URL.Query().Get("probe")) == "1"
	if !probe && len(rows) > 0 {
		perConvoMaxSeq := map[string]int64{}
		var convos []string
		for _, row := range rows {
			cid := sched.RowStr(row, "conversation_id")
			if _, seen := perConvoMaxSeq[cid]; !seen {
				convos = append(convos, cid)
			}
			if seq := sched.RowInt(row, "sequence"); seq > perConvoMaxSeq[cid] {
				perConvoMaxSeq[cid] = seq
			}
		}
		for _, cid := range convos {
			s.RecordSeen(agentID, cid, perConvoMaxSeq[cid])
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

// gatherClaimsByConvo:每个含未读非系统消息的会话的在途 worklog claim
// ("此处有真工作"权威信号),scopeKey = conversation_id——cumora claim
// --in <convo> 写的同一 scope。逐会话尽力而为,一个会话的 Redis 打嗝
// 不饿死整个闸门。
func (s *Service) gatherClaimsByConvo(inbox []map[string]any) map[string][]agent.WorklogEntry {
	seen := map[string]bool{}
	var convos []string
	for _, m := range inbox {
		if sched.RowStr(m, "kind") == "system" {
			continue
		}
		cid := sched.RowStr(m, "conversation_id")
		if !seen[cid] {
			seen[cid] = true
			convos = append(convos, cid)
		}
	}
	out := map[string][]agent.WorklogEntry{}
	for _, cid := range convos {
		if held := s.PeekWorklog(cid); len(held) > 0 {
			out[cid] = held
		}
	}
	return out
}

// handleInboxTriagePayload:BYOA——服务端组 triage 请求(有 DB 拿
// inbox+context),但不跑模型:返回空箱判定或 {instructions,input}。
// daemon 在其本地小脑上跑——判断永不离开操作者的机器,不花云配额。
// 无正则做决定:可行动性/模式全由小模型判。
func (s *Service) handleInboxTriagePayload(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	ctx := r.Context()
	persona, err := s.GetPersona(ctx, agentID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if persona == nil {
		httpx.WriteError(w, http.StatusNotFound, "agent not found")
		return
	}
	inbox, err := s.LoadInbox(ctx, agentID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	convoSet := map[string]bool{}
	var convoIDs []string
	for _, m := range inbox {
		cid := sched.RowStr(m, "conversation_id")
		if !convoSet[cid] {
			convoSet[cid] = true
			convoIDs = append(convoIDs, cid)
		}
	}
	// 内容无关成本地板(不是回环判定)。daemon 每 20s 自轮询、绕过调度
	// 器扇出的限速,失控会无界转本地模型——用与 wake 路径相同的激活
	// 预算封顶。是否回复仍 100% 是小模型的事,这里只防成本失控。
	if len(convoIDs) > 0 && !s.ConsumeTurnToken(agentID) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"verdict": map[string]any{
			"actionable": false,
			"reason":     "turn-rate floor: over activation budget this minute — deferring (the next minute or a human revives it)",
			"promptNote": "",
			"source":     "rate-limited",
		}})
		return
	}
	contextRows, err := s.LoadContext(ctx, agentID, companyID, convoIDs)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	claimsByConvo := s.gatherClaimsByConvo(inbox)
	humanActive := false
	if persona.CompanyID != "" {
		humanActive, _ = s.HumanRecentlyActive(ctx, persona.CompanyID, 10)
	}
	req := sched.BuildTriageRequest(agentID, persona, inbox, contextRows, claimsByConvo, humanActive)
	if req.Verdict != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"verdict": req.Verdict})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"instructions": *req.Instructions,
		"input":        *req.Input,
		"failClosed":   req.FailClosed,
	})
}

/* ───────── agenda(daemon 轮询对) ───────── */

// handleAgenda:BYOA 板感知——服务端收集该 agent 的可行动议程(非 done
// 列里指派/@点名的看板卡 + 到期日历事件)。byoa 路由返回分类器载荷
// (instructions/input + 原始 agenda 供本地回退),daemon 本地 classify
// 后把判定 POST 回 /agenda/verdict;remote 路由同步云分类(#89)。
func (s *Service) handleAgenda(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	ctx := r.Context()
	if companyID == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"actionable": false})
		return
	}
	agenda, err := s.Sched.GatherAgentAgenda(ctx, agentID, *companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if len(agenda.Cards) == 0 && len(agenda.Events) == 0 && len(agenda.Stalls) == 0 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"actionable": false, "cards": 0, "events": 0, "stalls": 0})
		return
	}
	persona, err := s.GetPersona(ctx, agentID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if persona == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"actionable": false})
		return
	}
	route := s.Sched.ResolveCerebellumRouteForAgent(ctx, agentID)
	built := sched.BuildAgendaClassifierRequest(persona, agenda, time.Now().UnixMilli())
	if route == "byoa" {
		if built.Verdict != nil {
			httpx.WriteJSON(w, http.StatusOK, s.Sched.FinalizeAgendaVerdict(ctx, agenda, *built.Verdict))
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"instructions": built.Instructions,
			"input":        built.Input,
			"agenda":       agenda,
		})
		return
	}
	// remote 路由:云分类同步跑在这里(classifyAgendaActionable ——
	// cerebellum 适配器或 legacy tracked OpenAI),失败退确定性回退;
	// finalize 与 /agenda/verdict 共用同一尾部保证字节同形。
	verdict := s.Sched.ClassifyAgendaActionable(ctx, persona, *companyID, agentID, agenda, time.Now().UnixMilli())
	httpx.WriteJSON(w, http.StatusOK, s.Sched.FinalizeAgendaVerdict(ctx, agenda, verdict))
}

// handleAgendaVerdict:daemon 本地 classify(或其确定性回退)后的判定
// 回传。重新收集议程(便宜的 DB 读;与载荷取用间的小陈旧窗口同
// /inbox-triage/payload 的容忍度),stall 认领与 brief 用当前数据,
// 并与 remote 路径共用同一尾部保证字节同形。
func (s *Service) handleAgendaVerdict(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	ctx := r.Context()
	if companyID == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"actionable": false})
		return
	}
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	focus := sliceUTF16(bodyStr(body, "focus"), 240)
	reason := sliceUTF16(bodyStr(body, "reason"), 240)
	verdict := sched.AgendaVerdict{Actionable: bodyBool(body, "actionable"), Focus: focus, Reason: reason}
	agenda, err := s.Sched.GatherAgentAgenda(ctx, agentID, *companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.Sched.FinalizeAgendaVerdict(ctx, agenda, verdict))
}

/* ───────── 读面(续) ───────── */

func (s *Service) handleMemoryQuery(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	// limits 逐键指针化(#94):TS `limits.semantic ?? 20`——缺键才补默认,
	// 显式 0 原样透传(只留 pinned 集;??:null/undefined 触发默认,0 不)。
	var semantic, recent, total *int
	if raw, isMap := body["limits"].(map[string]any); isMap {
		for k, v := range raw {
			if f, isNum := v.(float64); isNum {
				n := int(f)
				switch k {
				case "semantic":
					semantic = &n
				case "recent":
					recent = &n
				case "total":
					total = &n
				}
			}
		}
	}
	rows, err := s.LoadMemory(r.Context(), agentID, bodyStr(body, "queryText"),
		semantic, recent, total,
		bodyStrSlice(body, "projectIds"), bodyStrSlice(body, "conversationIds"))
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (s *Service) handleContext(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	ids := bodyStrSlice(body, "conversationIds")
	if ids == nil {
		ids = []string{}
	}
	// #130:放大闸门——每 id = 25 行 × (quoted 子查询 + reactions 聚合 +
	// human_last_read 相关子查询),无上限一把大数组就能打爆连接池。
	if len(ids) > 50 {
		httpx.WriteError(w, http.StatusBadRequest, "conversationIds: max 50 per request")
		return
	}
	rows, err := s.LoadContext(r.Context(), agentID, companyID, ids)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (s *Service) handleClimate(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	rows, err := s.LoadClimate(r.Context(), agentID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (s *Service) handleSkills(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	rows, err := s.LoadSkillsIndex(r.Context(), agentID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (s *Service) handleFaces(w http.ResponseWriter, r *http.Request, _ string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	ids := bodyStrSlice(body, "participantIds")
	if ids == nil {
		ids = []string{}
	}
	rows, err := s.LoadFaces(r.Context(), ids)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (s *Service) handleSystemPrompt(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	prompt, err := s.BuildSystemPrompt(r.Context(), agentID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	var out any
	if prompt != nil {
		out = *prompt
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"prompt": out})
}

// handleRoster:实时团队名册(名字+角色+id),按 agent 的租户。BYOA
// agent 每个真实轮取一次,才知道队友是谁、各干什么——云 agent 的系统
// 提示里已烘进同一份。
func (s *Service) handleRoster(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	persona, err := s.GetPersona(r.Context(), agentID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if persona == nil {
		httpx.WriteError(w, http.StatusNotFound, "agent not found")
		return
	}
	roster, err := s.BuildTeamRosterText(r.Context(), persona.CompanyID, agentID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"roster": roster})
}

/* ───────── 状态 + 在场 ───────── */

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	status := bodyStr(body, "status")
	// TS 只查非空——任意字符串直写(列无 CHECK),DB 失败也吞成 ok。
	if status == "" {
		httpx.WriteError(w, http.StatusBadRequest, "status required")
		return
	}
	// inproc 版吞掉 DB 失败只告警(inprocClient.setStatus try/catch)——
	// 状态胶囊非硬依赖,Go 同策略。
	if err := s.SetStatus(r.Context(), agentID, status); err != nil {
		slog.Warn("[runtime] setStatus failed — dropping", "status", status, "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleStatusHeartbeat(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	status := bodyStr(body, "status")
	// TS 同款:只查非空;status 不匹配当前值时 UPDATE 零行,照样 ok。
	if status == "" {
		httpx.WriteError(w, http.StatusBadRequest, "status required")
		return
	}
	if err := s.HeartbeatStatus(r.Context(), agentID, status); err != nil {
		slog.Warn("[runtime] heartbeatStatus failed — dropping", "status", status, "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleTyping(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	conversationID := bodyStr(body, "conversationId")
	if conversationID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "conversationId required")
		return
	}
	s.PublishTyping(r.Context(), conversationID, agentID, bodyBool(body, "done"), companyID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ───────── 观测面 ───────── */

func (s *Service) handleCreateRun(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	trigger, _ := body["trigger"].(map[string]any)
	var inputIDs []string
	if raw, ok := body["inputMessageIds"].([]any); ok {
		for _, x := range raw {
			if str, ok := x.(string); ok {
				inputIDs = append(inputIDs, str)
			}
		}
	}
	if inputIDs == nil {
		inputIDs = []string{}
	}
	var inboxCount int64
	if f, ok := bodyFloat(body, "inboxCount"); ok {
		inboxCount = int64(f)
	}
	// TS `fingerprint ?? null`:空串是有效值(保留),仅缺键为 null。
	var fingerprint *string
	if fpRaw, ok := body["fingerprint"].(string); ok {
		fingerprint = &fpRaw
	}
	runID, err := obs.CreateAgentRun(r.Context(), s.DB, struct {
		AgentID         string
		CompanyID       *string
		Trigger         map[string]any
		InputMessageIDs []string
		InboxCount      int64
		Fingerprint     *string
	}{agentID, companyID, trigger, inputIDs, inboxCount, fingerprint})
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"runId": runID})
}

func (s *Service) handleRecordEvent(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	runID, kind, title := bodyStr(body, "runId"), bodyStr(body, "kind"), bodyStr(body, "title")
	if runID == "" || kind == "" || title == "" {
		httpx.WriteError(w, http.StatusBadRequest, "runId, kind, title required")
		return
	}
	level := bodyStr(body, "level")
	data, _ := body["data"].(map[string]any)
	var stage *string
	if st := bodyStr(body, "stage"); st != "" {
		stage = &st
	}
	// 观测事件尽力而为:DB 打嗝不得打断轮(inproc recordEvent 同策略)。
	if err := obs.RecordAgentEvent(r.Context(), s.DB, struct {
		RunID     string
		AgentID   string
		CompanyID *string
		Kind      string
		Level     string
		Title     string
		Data      map[string]any
		Stage     *string
	}{runID, agentID, companyID, kind, level, title, data, stage}); err != nil {
		slog.Warn("[runtime] recordEvent failed — dropping", "kind", kind, "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// usageFromWire:RuntimeTokenUsage 宽松解析(缺字段按 0)。
func usageFromWire(v any) *costing.TokenUsage {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	get := func(k string) int64 {
		f, _ := m[k].(float64)
		return int64(f)
	}
	return &costing.TokenUsage{
		InputTokens:         get("inputTokens"),
		CachedInputTokens:   get("cachedInputTokens"),
		CacheCreationTokens: get("cacheCreationTokens"),
		OutputTokens:        get("outputTokens"),
	}
}

// handleRecordTriage:记一笔本地(BYOA)triage 进成本台账。daemon 在
// 操作者自己机器上跑 triage,服务器看不到其用量,除非 daemon 报上来。
// 身份来自 JWT;尽力而为(云侧 triage 在 classify 内联记,不走此路)。
func (s *Service) handleRecordTriage(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	source := bodyStr(body, "source")
	if source == "" {
		source = "byoa-claude"
	}
	model := strPtrOfRaw(body, "model")
	actionable := bodyBool(body, "actionable")
	reason := strPtrOfRaw(body, "reason")
	usage := usageFromWire(body["usage"])
	daemonVersion := normalizeDaemonVersion(bodyStr(body, "daemonVersion"))
	obs.RecordTriage(s.DB, agentID, companyID, &source, model, actionable, reason, usage)
	// 同时镜像进通用台账:BYOA 本地 triage 与云侧花费同页可见。
	// agent_triages 行保判定/理由(triage 经济学视图);llm_calls 只存
	// 原始调用形状。与 recordTriage 同样的 fire-and-forget 纪律。
	if usage != nil {
		reasonSlice := ""
		if reason != nil {
			rs := *reason
			if len(rs) > 200 {
				rs = rs[:200]
			}
			reasonSlice = rs
		}
		modelName := "<unknown>"
		if model != nil {
			modelName = *model // TS `body.model ?? '<unknown>'`:空串保留
		}
		obs.RecordLlmCall(s.DB, obs.LlmCallRecord{
			Purpose:       "inbox-triage",
			CompanyID:     companyID,
			AgentID:       &agentID,
			Source:        source,
			Model:         modelName,
			Usage:         usage,
			LatencyMS:     0,
			Status:        "ok",
			Extras:        map[string]any{"actionable": actionable, "reason": reasonSlice},
			DaemonVersion: daemonVersion,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func normalizeDaemonVersion(v string) *string {
	t := strings.TrimSpace(v)
	if t == "" {
		return nil
	}
	t = sliceUTF16(t, 32) // TS trim().slice(0,32) 按 UTF-16 码元
	return &t
}

// handleLlmCalls:BYOA agent 的逐跳轨迹。daemon 的 ClaudeSession/
// CodexSession 每条助手消息(Claude)或每个 turn-completed(Codex)产
// 一份 EngineHopReport,按 N 跳或 ~250ms 批量上送。每跳一行 llm_calls。
func (s *Service) handleLlmCalls(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	source := bodyStr(body, "source")
	switch source {
	case "byoa-claude", "byoa-codex", "byoa-grok", "byoa-cursor", "byoa-zcode":
	default:
		source = "byoa-claude"
	}
	daemonVersion := normalizeDaemonVersion(bodyStr(body, "daemonVersion"))
	hops, _ := body["hops"].([]any)
	if len(hops) == 0 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "inserted": 0})
		return
	}
	for _, rawHop := range hops {
		hop, ok := rawHop.(map[string]any)
		if !ok {
			continue
		}
		// purpose 白名单——未来 daemon 版本的新名字 coerce 成 agent-turn,
		// 不走私自由串进 rollup。
		purpose := "agent-turn"
		if p := bodyStr(hop, "purpose"); obs.KnownPurposes[p] {
			purpose = p
		}
		model := bodyStr(hop, "model")
		if model == "" {
			model = "<unknown>"
		}
		var latency int64
		if f, ok := bodyFloat(hop, "latencyMs"); ok {
			latency = int64(f)
		}
		status := bodyStr(hop, "status")
		if status == "" {
			status = "ok"
		}
		var runID, conversationID, errMsg *string
		if v := bodyStr(hop, "runId"); v != "" {
			runID = &v
		}
		if v := bodyStr(hop, "conversationId"); v != "" {
			conversationID = &v
		}
		if v := bodyStr(hop, "error"); v != "" {
			errMsg = &v
		}
		extras, _ := hop["extras"].(map[string]any)
		agent := agentID
		obs.RecordLlmCall(s.DB, obs.LlmCallRecord{
			Purpose:        purpose,
			CompanyID:      companyID,
			AgentID:        &agent,
			RunID:          runID,
			ConversationID: conversationID,
			Source:         source,
			Model:          model,
			Usage:          usageFromWire(hop["usage"]),
			LatencyMS:      latency,
			Status:         status,
			Error:          errMsg,
			Extras:         extras,
			DaemonVersion:  daemonVersion,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "inserted": len(hops)})
}

// handleRunHeartbeat:长引擎轮心跳,10 分钟陈旧清扫不误收。尽力而为。
func (s *Service) handleRunHeartbeat(w http.ResponseWriter, r *http.Request, _ string, _ *string) {
	runID := r.PathValue("runId")
	if err := obs.TouchAgentRun(r.Context(), s.DB, runID); err != nil {
		// 已被清扫/不存在都没关系。
		slog.Debug("[runtime] touchAgentRun no-op", "run", runID, "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleRunFinish(w http.ResponseWriter, r *http.Request, _ string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	status := bodyStr(body, "status")
	// TS 只查非空(不枚举校验)——镜像同语义。
	if status == "" {
		httpx.WriteError(w, http.StatusBadRequest, "status required")
		return
	}
	// TS `?? null` 语义:空串保留,仅缺键为 null。
	strPtrOf := func(key string) *string {
		if v, ok := body[key].(string); ok {
			return &v
		}
		return nil
	}
	summary, errMsg, model := strPtrOf("summary"), strPtrOf("error"), strPtrOf("model")
	var toolCallCount, tokenCount *int64
	if f, ok := bodyFloat(body, "toolCallCount"); ok {
		n := int64(f)
		toolCallCount = &n
	}
	if f, ok := bodyFloat(body, "tokenCount"); ok {
		n := int64(f)
		tokenCount = &n
	}
	// finishRun 尽力而为(inproc 同策略)。
	if err := obs.FinishAgentRun(r.Context(), s.DB, r.PathValue("runId"), status, summary, errMsg,
		toolCallCount, tokenCount, usageFromWire(body["usage"]), model); err != nil {
		slog.Warn("[runtime] finishRun failed — dropping", "run", r.PathValue("runId"), "status", status, "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ───────── steering busy 心跳 ───────── */

// handleBusyHeartbeat / handleBusyClear:轮中每 ~2s 一跳,消息路由据
// busy:<agentId> 判定 steer(轮中注入)还是常规 wake(轮后拾取)。
// agentId 来自 JWT,与所有端点同款防冒充。
func (s *Service) handleBusyHeartbeat(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	ttl := 5.0
	if f, isNum := bodyFloat(body, "ttlSec"); isNum && f > 0 && f <= 300 {
		ttl = f
	}
	s.RecordBusyHeartbeat(agentID, int(ttl))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleBusyClear(w http.ResponseWriter, _ *http.Request, agentID string, _ *string) {
	s.ClearBusyHeartbeat(agentID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ───────── thinking-claim ───────── */

// handleThinkingMark:同侪可见的"我在想"信号 + 在同刻给这些会话盖
// 新鲜度预检的 compose 锚(NX 语义保留——心跳不推迟锚,锚必须反映
// 轮始而非"最近一次心跳")。cli.cmdReply 预检靠锚发现 compose 期间
// 落地的同侪发帖,即便 agent 自己的 glance 已把 seen 基线推过去。
func (s *Service) handleThinkingMark(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	ids := bodyStrSlice(body, "conversationIds")
	ttl := 60.0
	if f, ok := bodyFloat(body, "ttlSec"); ok && f > 0 && f <= 600 {
		ttl = f
	}
	s.MarkThinking(agentID, ids, int(ttl))
	now := time.Now().UnixMilli()
	for _, cid := range ids {
		existing := s.GetComposeAnchor(agentID, cid)
		if existing > 0 && now-existing < 30*60_000 {
			continue // 本轮已盖过——保第一枚
		}
		s.RecordComposeAnchor(agentID, cid, now)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleThinkingUnmark(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	ids := bodyStrSlice(body, "conversationIds")
	s.UnmarkThinking(agentID, ids)
	// 轮结束——锚一并清,下一轮拿新锚。
	for _, cid := range ids {
		s.ClearComposeAnchor(agentID, cid)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleThinkingPeek(w http.ResponseWriter, r *http.Request, _ string, _ *string) {
	cid := r.URL.Query().Get("conversationId")
	if cid == "" {
		httpx.WriteError(w, http.StatusBadRequest, "conversationId required")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"agents": s.PeekThinking(cid)})
}

/* ───────── worklog ───────── */

func (s *Service) handleWorklogClaim(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	scopeKey, taskType, subject := bodyStr(body, "scopeKey"), bodyStr(body, "taskType"), bodyStr(body, "subject")
	if scopeKey == "" || taskType == "" || subject == "" {
		httpx.WriteError(w, http.StatusBadRequest, "scopeKey, taskType, subject required")
		return
	}
	ttl := 0.0
	if f, ok := bodyFloat(body, "ttlSec"); ok && f > 0 && f <= 3600 {
		ttl = f
	}
	result := s.ClaimWork(scopeKey, agentID, taskType, subject, int(ttl))
	out := map[string]any{"accepted": result.Accepted}
	if result.Existing != nil {
		out["existing"] = map[string]any{
			"agentId":   result.Existing.AgentID,
			"taskType":  result.Existing.TaskType,
			"subject":   result.Existing.Subject,
			"startedAt": result.Existing.StartedAt,
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) handleWorklogRelease(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	scopeKey, taskType, subject := bodyStr(body, "scopeKey"), bodyStr(body, "taskType"), bodyStr(body, "subject")
	if scopeKey == "" || taskType == "" || subject == "" {
		httpx.WriteError(w, http.StatusBadRequest, "scopeKey, taskType, subject required")
		return
	}
	s.ReleaseWork(scopeKey, agentID, taskType, subject)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleWorklogPeek(w http.ResponseWriter, r *http.Request, _ string, _ *string) {
	sk := r.URL.Query().Get("scopeKey")
	if sk == "" {
		httpx.WriteError(w, http.StatusBadRequest, "scopeKey required")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"entries": s.PeekWorklog(sk)})
}

/* ───────── 读游标推进 + 系统通知 ───────── */

// handleMarkRead:轮末告诉服务端哪些消息已经过轮中 steer 排水消费,
// 下次 wake 的 loadInbox 不再浮出。agentId 取 JWT。
func (s *Service) handleMarkRead(w http.ResponseWriter, r *http.Request, agentID string, _ *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	conversationID, upToMessageID := bodyStr(body, "conversationId"), bodyStr(body, "upToMessageId")
	if conversationID == "" || upToMessageID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "conversationId and upToMessageId required")
		return
	}
	if err := s.MarkConversationRead(r.Context(), agentID, conversationID, upToMessageID); err != nil {
		slog.Warn("[runtime] markConversationRead failed — dropping", "agent", agentID, "convo", conversationID, "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleNotices:身份恒取 JWT——notice 永远关于本 agent(如它自己的引擎
// 故障)。信任请求体的 agentId/companyId 会让任何有效 token 冒充他人
// 跨租户发通知。目标会话须属 token 租户且本 agent 是成员。
func (s *Service) handleNotices(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	conversationID := bodyStr(body, "conversationId")
	noticeKind := bodyStr(body, "noticeKind")
	text := bodyStr(body, "text")
	dedupeKey := bodyStr(body, "dedupeKey")
	if conversationID == "" || noticeKind == "" || text == "" || dedupeKey == "" {
		httpx.WriteError(w, http.StatusBadRequest, "conversationId, noticeKind, text, dedupeKey required")
		return
	}
	member, err := s.IsConversationMember(r.Context(), conversationID, agentID, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if !member {
		httpx.WriteError(w, http.StatusForbidden, "not a member of that conversation")
		return
	}
	ttl := 3600.0
	if f, ok := bodyFloat(body, "dedupeTtlSec"); ok {
		ttl = f
	}
	posted, err := s.PostSystemNotice(r.Context(), conversationID, companyID, agentID, noticeKind, text, dedupeKey, int(ttl))
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"posted": posted})
}
