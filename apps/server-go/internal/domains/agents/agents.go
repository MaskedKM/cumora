// domains/agents —— agent CRUD(#68 补齐):create(全链种子)/update/
// offboard/rehire/autonomy 三端点(#123 补 GET 单读)。行为对齐
// router.ts 2236–2660、4712–4760 与 onboardCompany.ts joinAllHands。
// #187 批次 5:agents+participants 双 tag 走 ServerInterface;avatar/
// computer/runtime-token 三条跨包路由经导出面委托(devtools/computers)。
package agents

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"

	agentscontract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/agents"
	participantscontract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/participants"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/computers"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/devtools"
	emailpkg "github.com/MaskedKM/cumora/apps/server-go/internal/email"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/onboard"
	"golang.org/x/text/unicode/norm"
)

// AvatarGen:创建后的 fire-and-forget 头像生成钩子(runtime 注入;nil 安全)。
type AvatarGen func(agentID, tenant string)

// Server:agents tag(10 路由)与 participants tag(1 路由)的域实现。
// 方法体自原闭包工厂逐字上移(#187 批次 5);三条跨包路由一行委托。
type Server struct {
	DB         *sql.DB
	AvatarGen  AvatarGen          // create 后 fire-and-forget(nil 安全)
	SyncAvatar devtools.AvatarGen // {id}/avatar/generate 端点(同步产 URL)
	Computers  *computers.Server  // computer/runtime-token 两路由委托载体
}

var _ agentscontract.ServerInterface = (*Server)(nil)
var _ participantscontract.ServerInterface = (*Server)(nil)

func Mount(mux *http.ServeMux, db *sql.DB, avatarGen AvatarGen, syncAvatar devtools.AvatarGen) {
	s := &Server{DB: db, AvatarGen: avatarGen, SyncAvatar: syncAvatar, Computers: &computers.Server{DB: db}}
	_ = agentscontract.HandlerFromMux(s, mux)
	_ = participantscontract.HandlerFromMux(s, mux)
}

/* ───────── 跨包委托(agents tag 的三条路由) ───────── */

func (s *Server) GenerateAgentAvatar(w http.ResponseWriter, r *http.Request, id string) {
	devtools.AgentAvatarGenerate(s.DB, s.SyncAvatar, w, r, id)
}

func (s *Server) AssignAgentComputer(w http.ResponseWriter, r *http.Request, id string) {
	s.Computers.AssignAgentComputer(w, r, id)
}

func (s *Server) MintAgentRuntimeToken(w http.ResponseWriter, r *http.Request, id string) {
	s.Computers.MintAgentRuntimeToken(w, r, id)
}

func requireRole(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, bool) {
	uid, companyID, ok := httpx.RequireCompany(w, r, db)
	if !ok {
		return "", false
	}
	var role string
	if err := db.QueryRowContext(r.Context(),
		`SELECT role FROM company_members WHERE company_id = $1 AND user_id = $2 LIMIT 1`,
		companyID, uid).Scan(&role); err != nil {
		role = "member"
	}
	if role != "owner" && role != "admin" {
		httpx.WriteError(w, http.StatusForbidden, "this action requires an owner or admin of the team")
		return "", false
	}
	return companyID, true
}

/* ───────── 读体(readAgentBody 语义) ───────── */

type agentBody struct {
	name, role, systemPrompt, bio      *string
	initial, avatarBg                  *string
	avatarURL, model, fastModel        *string // nil 键=不动;存在但 null=显式清(用 hasNull 标)
	avatarURLNull, modelNull, fastNull bool
	chatRegister                       *bool // #24 聊天体语域开关(human-audience 说人话)
	tools                              []string
	hasTools                           bool
}

func decodeAgentBody(r *http.Request) agentBody {
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&raw)
	var b agentBody
	str := func(k string) *string {
		var v any
		if json.Unmarshal(raw[k], &v) != nil {
			return nil
		}
		s, ok := v.(string)
		if !ok {
			return nil
		}
		return &s
	}
	if s := str("name"); s != nil {
		t := strings.TrimSpace(*s)
		b.name = &t
	}
	if s := str("role"); s != nil {
		t := strings.TrimSpace(*s)
		b.role = &t
	}
	b.systemPrompt = str("systemPrompt")
	b.bio = str("bio")
	if s := str("initial"); s != nil {
		t := httpx.UTF16Cap(strings.TrimSpace(*s), 2)
		b.initial = &t
	}
	if s := str("avatarBg"); s != nil {
		t := strings.TrimSpace(*s)
		b.avatarBg = &t
	}
	if rawKey, ok := raw["avatarUrl"]; ok {
		var v any
		if json.Unmarshal(rawKey, &v) == nil {
			if v == nil {
				b.avatarURLNull = true
			} else if s, isStr := v.(string); isStr {
				t := strings.TrimSpace(s)
				b.avatarURL = &t
			}
		}
	}
	if rawKey, ok := raw["model"]; ok {
		var v any
		if json.Unmarshal(rawKey, &v) == nil {
			if v == nil {
				b.modelNull = true
			} else if s, isStr := v.(string); isStr {
				b.model = &s
			}
		}
	}
	if rawKey, ok := raw["fastModel"]; ok {
		var v any
		if json.Unmarshal(rawKey, &v) == nil {
			if v == nil {
				b.fastNull = true
			} else if s, isStr := v.(string); isStr {
				b.fastModel = &s
			}
		}
	}
	if rawKey, ok := raw["chatRegister"]; ok {
		var v any
		if json.Unmarshal(rawKey, &v) == nil {
			if bv, isBool := v.(bool); isBool {
				b.chatRegister = &bv
			}
		}
	}
	if rawKey, ok := raw["tools"]; ok {
		var v any
		if json.Unmarshal(rawKey, &v) == nil {
			if arr, isArr := v.([]any); isArr {
				b.hasTools = true
				for _, e := range arr {
					if s, isStr := e.(string); isStr {
						b.tools = append(b.tools, s)
					}
				}
			}
		}
	}
	return b
}

/* ───────── create ───────── */

var avatarPalette = []string{
	"#FFB088", "#FFD9D2", "#FFB7AF", "#F4B740",
	"#7C5CFF", "#A593FF", "#4FC2F4", "#41B5DC",
	"#4FC2A1", "#6EC56A", "#E9A0E9", "#FF7AB6",
}

func defaultAvatarBg(id string) string {
	h := uint32(0)
	for _, c := range []byte(id) {
		h = h*31 + uint32(c)
	}
	return avatarPalette[h%uint32(len(avatarPalette))]
}

var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)
var dashRunRe = regexp.MustCompile(`-{2,}`)
var combiningRe = regexp.MustCompile(`\p{M}`)

// slugifyAgentName:TS normalize('NFKD') + 组合记号剥离 + 小写(评审 F13:
// 'Ágent' → 'agent',非 NFKD 会得 'gent')。
func slugifyAgentName(name string) string {
	lowered := strings.ToLower(combiningRe.ReplaceAllString(norm.NFKD.String(name), ""))
	slug := nonAlnumRe.ReplaceAllString(lowered, "-")
	slug = strings.Trim(slug, "-")
	slug = dashRunRe.ReplaceAllString(slug, "-")
	slug = httpx.UTF16Cap(slug, 24)
	if !regexp.MustCompile(`^[a-z]`).MatchString(slug) {
		slug = httpx.UTF16Cap("a-"+slug, 24)
	}
	if slug == "" {
		slug = "agent"
	}
	return slug
}

// companyPlanTier:属主 tier,回退最早加入 owner 角色成员(评审 F2:
// 与 invitations 域 F1 同源;不得取调用者或全体成员最优)。
func companyPlanTier(ctx context.Context, db *sql.DB, companyID string) string {
	var tier sql.NullString
	_ = db.QueryRowContext(ctx, `
		SELECT COALESCE(owner_user.tier, owner_member.tier, 'free')
		  FROM companies c
		  LEFT JOIN users owner_user ON owner_user.id = c.owner_user_id
		  LEFT JOIN LATERAL (
		    SELECT u.tier
		      FROM company_members cm
		      JOIN users u ON u.id = cm.user_id
		     WHERE cm.company_id = c.id AND cm.role = 'owner' AND u.tier IS NOT NULL
		     ORDER BY cm.joined_at ASC
		     LIMIT 1
		  ) owner_member ON TRUE
		 WHERE c.id = $1`, companyID).Scan(&tier)
	if tier.Valid {
		return tier.String
	}
	return "free"
}

func tierAgents(t string) int {
	switch t {
	case "pro":
		return 20
	case "max":
		return 50
	default:
		return 10
	}
}

func randSuffix6() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 4)
	for i := range b {
		b[i] = alphabet[rand.Intn(len(alphabet))]
	}
	return string(b)
}

func pickUniqueAgentID(ctx context.Context, db *sql.DB, baseName string) (string, error) {
	base := slugifyAgentName(baseName)
	candidates := []string{base}
	for i := 0; i < 8; i++ {
		candidates = append(candidates, base+"-"+randSuffix6())
	}
	for _, c := range candidates {
		var exists bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM participants WHERE id = $1)`, c).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return c, nil
		}
	}
	return "", fmt.Errorf("no unique id")
}

func (s *Server) CreateAgent(w http.ResponseWriter, r *http.Request) {
	uid, tenant, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	if _, ok := requireRole(w, r, s.DB); !ok {
		return
	}
	body := decodeAgentBody(r)
	if body.name == nil || *body.name == "" {
		httpx.WriteError(w, http.StatusBadRequest, "name required")
		return
	}
	if body.systemPrompt == nil || len(strings.TrimSpace(*body.systemPrompt)) < 10 {
		httpx.WriteError(w, http.StatusBadRequest,
			"systemPrompt required (at least 10 chars — describe the agent's style)")
		return
	}
	// tier 限(free=10/pro=20/max=50);tier 属公司不属调用者(评审 F2)。
	tier := companyPlanTier(r.Context(), s.DB, tenant)
	var agentCount int
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM participants WHERE company_id = $1 AND kind = 'agent' AND departed_at IS NULL`,
		tenant).Scan(&agentCount)
	if agentCount >= tierAgents(tier) {
		httpx.WriteError(w, http.StatusForbidden,
			fmt.Sprintf("%s tier teams can have at most %d active agents", tier, tierAgents(tier)))
		return
	}
	agentID, err := pickUniqueAgentID(r.Context(), s.DB, *body.name)
	if err != nil {
		// 9 候选全撞(TS 落到 INSERT duplicate)→ 同 409 语义(评审 F14)。
		httpx.WriteError(w, http.StatusConflict, "agent id collision — please retry")
		return
	}
	initial := ""
	if body.initial != nil && *body.initial != "" {
		initial = *body.initial
	} else {
		initial = strings.ToUpper(string([]rune(*body.name)[0:1]))
	}
	avatarBg := ""
	if body.avatarBg != nil && *body.avatarBg != "" {
		avatarBg = *body.avatarBg
	} else {
		avatarBg = defaultAvatarBg(agentID)
	}
	role := ""
	if body.role != nil {
		role = *body.role
	}
	bio := ""
	if body.bio != nil {
		bio = *body.bio
	}
	tools := body.tools
	if !body.hasTools || len(tools) == 0 {
		tools = []string{"bash"}
	}
	toolsJSON, _ := json.Marshal(tools)
	var modelArg, fastModelArg any
	if body.model != nil {
		modelArg = *body.model
	}
	if body.fastModel != nil {
		fastModelArg = *body.fastModel
	}
	_, err = s.DB.ExecContext(r.Context(), `
		INSERT INTO participants (id, kind, name, role, initial, avatar_bg, status, bio, tools, system_prompt, model, fast_model, company_id)
		VALUES ($1, 'agent', $2, $3, $4, $5, 'avail', $6, $7::jsonb, $8, $9, $10, $11)`,
		agentID, *body.name, role, initial, avatarBg, bio, string(toolsJSON), *body.systemPrompt, modelArg, fastModelArg, tenant)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "participants_agent_id_unique") {
			httpx.WriteError(w, http.StatusConflict, "agent id collision — please retry")
		} else {
			httpx.WriteInternalError(w, r, err)
		}
		return
	}
	onboard.JoinAllHands(r.Context(), s.DB, tenant, agentID)
	seedIdentitySoul(r.Context(), s.DB, tenant, agentID, *body.name, role, bio, *body.systemPrompt)
	autoCreateDirect(r.Context(), s.DB, uid, tenant, agentID, *body.name)
	if s.AvatarGen != nil {
		go func() { s.AvatarGen(agentID, tenant) }()
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": agentID})
}

func seedIdentitySoul(ctx context.Context, db *sql.DB, tenant, agentID, name, role, bio, systemPrompt string) {
	roleStr := role
	if roleStr == "" {
		roleStr = "agent"
	}
	bioBlock := ""
	if bio != "" {
		bioBlock = "**Bio:**\n" + bio + "\n\n"
	}
	identity := "# " + name + "\n\n**Role:** " + roleStr + "\n\n" + bioBlock +
		"_This file is your identity. Edit it as you grow — what you write here_\n" +
		"_loads into your system prompt on every wake._\n"
	soul := "# Soul of " + name + "\n\n## Voice\n\n" + systemPrompt + "\n\n## Principles\n\n" +
		"- Speak like a real person, not like a tech blog.\n" +
		"- Match the user's language.\n" +
		"- Save things worth remembering — they outlive any single conversation.\n\n" +
		"_This file is your voice + values. Edit it freely to evolve who you are._\n"
	_, _ = db.ExecContext(ctx, `
		INSERT INTO agent_workspace (agent_id, path, body, company_id, updated_at)
		VALUES ($1, 'IDENTITY.md', $2, $3, NOW()), ($1, 'SOUL.md', $4, $3, NOW())
		ON CONFLICT (agent_id, path) DO NOTHING`, agentID, identity, tenant, soul)
}

func autoCreateDirect(ctx context.Context, db *sql.DB, uid, tenant, agentID, name string) {
	_, _ = db.ExecContext(ctx, `
		INSERT INTO conversations (id, kind, title, subtitle, members, pinned, tag, company_id)
		VALUES ($1, 'direct', $2, NULL, $3::jsonb, FALSE, NULL, $4)
		ON CONFLICT (id) DO NOTHING`,
		"direct-"+agentID+"-"+randHex6(), name, `["`+uid+`","`+agentID+`"]`, tenant)
	_, _ = db.ExecContext(ctx, `
		INSERT INTO conversation_counters (conversation_id, next_sequence)
		SELECT c.id, 1
		  FROM conversation_members ca
		  JOIN conversation_members cb ON cb.conversation_id = ca.conversation_id
		  JOIN conversations c ON c.id = ca.conversation_id
		 WHERE ca.participant_id = $1 AND cb.participant_id = $3
		   AND c.kind = 'direct' AND c.company_id = $2
		   AND jsonb_array_length(c.members) = 2
		 ON CONFLICT (conversation_id) DO NOTHING`, uid, tenant, agentID)
}

/* ───────── update / offboard / rehire ───────── */

func (s *Server) UpdateAgent(w http.ResponseWriter, r *http.Request, id string) {
	tenant, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	var kind string
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT kind FROM participants WHERE id = $1 AND company_id = $2`, id, tenant).Scan(&kind); err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	if kind != "agent" {
		httpx.WriteError(w, http.StatusBadRequest, "cannot edit non-agent participant")
		return
	}
	body := decodeAgentBody(r)
	sets := []string{}
	params := []any{}
	push := func(col string, v any) {
		params = append(params, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(params)))
	}
	if body.name != nil {
		push("name", *body.name)
	}
	if body.role != nil {
		push("role", *body.role)
	}
	if body.systemPrompt != nil {
		push("system_prompt", *body.systemPrompt)
	}
	if body.bio != nil {
		push("bio", *body.bio)
	}
	if body.initial != nil {
		push("initial", *body.initial)
	}
	if body.avatarBg != nil {
		push("avatar_bg", *body.avatarBg)
	}
	if body.avatarURL != nil {
		push("avatar_url", *body.avatarURL)
	} else if body.avatarURLNull {
		push("avatar_url", nil)
	}
	if body.model != nil {
		push("model", *body.model)
	} else if body.modelNull {
		push("model", nil)
	}
	if body.fastModel != nil {
		push("fast_model", *body.fastModel)
	} else if body.fastNull {
		push("fast_model", nil)
	}
	if body.hasTools {
		toolsJSON, _ := json.Marshal(body.tools)
		params = append(params, string(toolsJSON))
		sets = append(sets, fmt.Sprintf("tools = $%d::jsonb", len(params)))
	}
	if body.chatRegister != nil {
		push("chat_register", *body.chatRegister)
	}
	if len(sets) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	params = append(params, id, tenant)
	if _, err := s.DB.ExecContext(r.Context(),
		fmt.Sprintf(`UPDATE participants SET %s WHERE id = $%d AND company_id = $%d`,
			strings.Join(sets, ", "), len(params)-1, len(params)), params...); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) OffboardAgent(w http.ResponseWriter, r *http.Request, id string) {
	tenant, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	var kind string
	var departedAt sql.NullTime
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT kind, departed_at FROM participants WHERE id = $1 AND company_id = $2`, id, tenant).
		Scan(&kind, &departedAt); err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	if kind != "agent" {
		httpx.WriteError(w, http.StatusBadRequest, "cannot off-board non-agent participant")
		return
	}
	if departedAt.Valid {
		httpx.WriteError(w, http.StatusConflict, "already off-boarded")
		return
	}
	if _, err := s.DB.ExecContext(r.Context(), `
		UPDATE participants SET departed_at = NOW(), status = 'resting', status_updated_at = NOW()
		  WHERE id = $1 AND company_id = $2`, id, tenant); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok": true, "departedAt": httpx.ISOms(time.Now()),
	})
}

func (s *Server) RehireAgent(w http.ResponseWriter, r *http.Request, id string) {
	tenant, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	var kind string
	var departedAt sql.NullTime
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT kind, departed_at FROM participants WHERE id = $1 AND company_id = $2`, id, tenant).
		Scan(&kind, &departedAt); err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	if kind != "agent" {
		httpx.WriteError(w, http.StatusBadRequest, "cannot rehire non-agent participant")
		return
	}
	if !departedAt.Valid {
		httpx.WriteError(w, http.StatusConflict, "agent is not off-boarded")
		return
	}
	// tier 限:rehire 同 create 的闸(tier 属公司,评审 F2)。
	tier := companyPlanTier(r.Context(), s.DB, tenant)
	var agentCount int
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM participants WHERE company_id = $1 AND kind = 'agent' AND departed_at IS NULL`,
		tenant).Scan(&agentCount)
	if agentCount >= tierAgents(tier) {
		httpx.WriteError(w, http.StatusForbidden,
			fmt.Sprintf("%s tier teams can have at most %d active agents", tier, tierAgents(tier)))
		return
	}
	if _, err := s.DB.ExecContext(r.Context(), `
		UPDATE participants SET departed_at = NULL, status = 'avail', status_updated_at = NOW()
		  WHERE id = $1 AND company_id = $2`, id, tenant); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ───────── autonomy ───────── */

// GetAutonomy:TS rows[0] ?? 默认门 —— 已存行原样回,未见行回
// threshold 0.6 起步(未见 ≠ 错误,前端首访即拿默认值)。
func (s *Server) GetAutonomy(w http.ResponseWriter, r *http.Request, id string) {
	uid, tenant, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	var one int
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 LIMIT 1`,
		id, tenant).Scan(&one); err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	var threshold float64
	var pulled, led, dissolved int
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT threshold, pulled, led, dissolved FROM agent_autonomy
		 WHERE user_id = $1 AND agent_id = $2`, uid, id).
		Scan(&threshold, &pulled, &led, &dissolved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if err != nil {
		threshold = 0.6
		pulled, led, dissolved = 0, 0, 0
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"userId": uid, "agentId": id, "threshold": threshold,
		"pulled": pulled, "led": led, "dissolved": dissolved,
	})
}

func (s *Server) PutAutonomy(w http.ResponseWriter, r *http.Request, id string) {
	uid, tenant, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	var one int
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 LIMIT 1`,
		id, tenant).Scan(&one); err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	var v any
	_ = json.Unmarshal(body["threshold"], &v)
	threshold := 0.6
	if f, isNum := v.(float64); isNum {
		threshold = f
	}
	if threshold < 0 {
		threshold = 0
	}
	if threshold > 1 {
		threshold = 1
	}
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO agent_autonomy (user_id, agent_id, threshold)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, agent_id) DO UPDATE SET threshold = EXCLUDED.threshold`,
		uid, id, threshold); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "threshold": threshold})
}

func (s *Server) GetAllAutonomy(w http.ResponseWriter, r *http.Request) {
	uid, tenant, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT a.user_id, a.agent_id, a.threshold, a.pulled, a.led, a.dissolved
		  FROM agent_autonomy a
		  JOIN participants p ON p.id = a.agent_id
		 WHERE a.user_id = $1 AND p.company_id = $2`, uid, tenant)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var userID, agentID string
		var threshold float64
		var pulled, led, dissolved int
		if rows.Scan(&userID, &agentID, &threshold, &pulled, &led, &dissolved) == nil {
			out = append(out, map[string]any{
				"userId": userID, "agentId": agentID, "threshold": threshold,
				"pulled": pulled, "led": led, "dissolved": dissolved,
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

/* ───────── 小助手 ───────── */

func randHex6() string {
	b := make([]byte, 3)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}

/* ───────── GET /participants(#68 补齐) ───────── */

// GetParticipants:过期 busy 状态回落 + 名册(含 agent 惰铸地址回显)。
func (s *Server) GetParticipants(w http.ResponseWriter, r *http.Request) {
	_, tenant, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	_, _ = s.DB.ExecContext(r.Context(), `
		UPDATE participants
		   SET status = 'avail', status_updated_at = NOW()
		 WHERE company_id = $1
		   AND kind = 'agent'
		   AND departed_at IS NULL
		   AND status IN ('thinking', 'working', 'waiting')
		   AND status_updated_at < NOW() - ($2::int * INTERVAL '1 millisecond')`, tenant, 90_000)
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT p.id, p.kind, p.name, p.role, p.initial,
		       p.avatar_bg, p.avatar_url,
		       p.status, p.status_updated_at,
		       p.bio, p.tools, p.system_prompt, p.model,
		       p.computer_id, p.engine, p.fast_model,
		       COALESCE(p.email, CASE WHEN p.kind = 'human' AND cm.user_id IS NOT NULL THEN u.email END),
		       comp.slug, p.departed_at, p.chat_register
		  FROM participants p
		  JOIN companies comp ON comp.id = p.company_id
		  LEFT JOIN company_members cm ON cm.user_id = p.id AND cm.company_id = p.company_id
		  LEFT JOIN users u ON u.id = cm.user_id
		 WHERE p.company_id = $1
		 ORDER BY p.kind DESC, p.name ASC`, tenant)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, kind, name, initial, avatarBg, status string
		var role, avatarUrl, bio, systemPrompt, model sql.NullString
		var tools []byte
		var statusUpdatedAt time.Time
		var computerID, engine, fastModel, emailCol, companySlug sql.NullString
		var departedAt sql.NullTime
		var chatRegister sql.NullBool
		if err := rows.Scan(&id, &kind, &name, &role, &initial,
			&avatarBg, &avatarUrl, &status, &statusUpdatedAt,
			&bio, &tools, &systemPrompt, &model,
			&computerID, &engine, &fastModel, &emailCol, &companySlug, &departedAt, &chatRegister); err == nil {
			var toolsAny any
			_ = json.Unmarshal(tools, &toolsAny)
			emailVal := any(nil)
			if emailCol.Valid {
				emailVal = emailCol.String
			} else if kind == "agent" && companySlug.Valid {
				emailVal = emailpkg.ComputeAgentAddress(id, companySlug.String)
			}
			// #24:开关读回——P0 修复,缺失时按默认开(旧列空档)。
			chatRegisterVal := true
			if chatRegister.Valid {
				chatRegisterVal = chatRegister.Bool
			}
			out = append(out, map[string]any{
				"id": id, "kind": kind, "name": name, "role": nullStr(role),
				"initial": initial, "avatarBg": avatarBg, "avatarUrl": nullStr(avatarUrl),
				"status": status, "statusUpdatedAt": httpx.ISOms(statusUpdatedAt),
				"bio": nullStr(bio), "tools": toolsAny, "systemPrompt": nullStr(systemPrompt),
				"model": nullStr(model), "computerId": nullStr(computerID),
				"engine": nullStr(engine), "fastModel": nullStr(fastModel),
				"email": emailVal, "departedAt": nullTimeUTC(departedAt),
				"chatRegister": chatRegisterVal,
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func nullTimeUTC(nt sql.NullTime) any {
	if !nt.Valid {
		return nil
	}
	return httpx.ISOms(nt.Time)
}

func nullStr(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}
