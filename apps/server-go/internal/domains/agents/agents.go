// domains/agents —— agent CRUD(#68 补齐):create(全链种子)/update/
// offboard/rehire/autonomy 双端点。行为对齐 router.ts 2236–2660 与
// onboardCompany.ts joinAllHands。
package agents

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// AvatarGen:创建后的 fire-and-forget 头像生成钩子(runtime 注入;nil 安全)。
type AvatarGen func(agentID, tenant string)

func Mount(mux *http.ServeMux, db *sql.DB, avatarGen AvatarGen) {
	mux.HandleFunc("POST /api/agents", create(db, avatarGen))
	mux.HandleFunc("PUT /api/agents/{id}", update(db))
	mux.HandleFunc("DELETE /api/agents/{id}", offboard(db))
	mux.HandleFunc("POST /api/agents/{id}/rehire", rehire(db))
	mux.HandleFunc("PUT /api/agents/{id}/autonomy", putAutonomy(db))
	mux.HandleFunc("GET /api/agents/autonomy", getAutonomy(db))
}

func requireCompany(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, string, bool) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return "", "", false
	}
	companyID, ok := httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return "", "", false
	}
	return uid, companyID, true
}

func requireRole(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, bool) {
	uid, companyID, ok := requireCompany(w, r, db)
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
		t := utf16Cap(strings.TrimSpace(*s), 2)
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

func utf16Cap(s string, n int) string {
	count := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if count+w > n {
			return s[:i]
		}
		count += w
	}
	return s
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

func slugifyAgentName(name string) string {
	lowered := strings.ToLower(name)
	slug := nonAlnumRe.ReplaceAllString(lowered, "-")
	slug = strings.Trim(slug, "-")
	slug = dashRunRe.ReplaceAllString(slug, "-")
	slug = utf16Cap(slug, 24)
	if !regexp.MustCompile(`^[a-z]`).MatchString(slug) {
		slug = utf16Cap("a-"+slug, 24)
	}
	if slug == "" {
		slug = "agent"
	}
	return slug
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

func create(db *sql.DB, avatarGen AvatarGen) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, tenant, ok := requireCompany(w, r, db)
		if !ok {
			return
		}
		if _, ok := requireRole(w, r, db); !ok {
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
		// tier 限(free=10/pro=20/max=50)。
		var tier string
		_ = db.QueryRowContext(r.Context(),
			`SELECT COALESCE(tier, 'free') FROM users WHERE id = $1`, uid).Scan(&tier)
		var agentCount int
		_ = db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM participants WHERE company_id = $1 AND kind = 'agent' AND departed_at IS NULL`,
			tenant).Scan(&agentCount)
		if agentCount >= tierAgents(tier) {
			httpx.WriteError(w, http.StatusForbidden,
				fmt.Sprintf("%s tier teams can have at most %d active agents", tier, tierAgents(tier)))
			return
		}
		agentID, err := pickUniqueAgentID(r.Context(), db, *body.name)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
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
		_, err = db.ExecContext(r.Context(), `
			INSERT INTO participants (id, kind, name, role, initial, avatar_bg, status, bio, tools, system_prompt, model, fast_model, company_id)
			VALUES ($1, 'agent', $2, $3, $4, $5, 'avail', $6, $7::jsonb, $8, $9, $10, $11)`,
			agentID, *body.name, role, initial, avatarBg, bio, string(toolsJSON), *body.systemPrompt, modelArg, fastModelArg, tenant)
		if err != nil {
			if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "participants_agent_id_unique") {
				httpx.WriteError(w, http.StatusConflict, "agent id collision — please retry")
			} else {
				httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		joinAllHands(r.Context(), db, tenant, agentID)
		seedIdentitySoul(r.Context(), db, tenant, agentID, *body.name, role, bio, *body.systemPrompt)
		autoCreateDirect(r.Context(), db, uid, tenant, agentID, *body.name)
		if avatarGen != nil {
			go func() { avatarGen(agentID, tenant) }()
		}
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": agentID})
	}
}

func joinAllHands(ctx context.Context, db *sql.DB, companyID, participantID string) {
	var convID sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT all_hands_conversation_id FROM companies WHERE id = $1`, companyID).Scan(&convID); err != nil || !convID.Valid {
		return
	}
	res, err := db.ExecContext(ctx, `
		UPDATE conversations
		   SET members = members || to_jsonb(ARRAY[$2::text]), updated_at = NOW()
		 WHERE id = $1 AND NOT (members @> to_jsonb(ARRAY[$2::text]))`, convID.String, participantID)
	if err != nil {
		return
	}
	if ra, _ := res.RowsAffected(); ra == 0 {
		return
	}
	var seq int
	if err := db.QueryRowContext(ctx, `
		INSERT INTO conversation_counters (conversation_id, next_sequence)
		VALUES ($1, 2)
		ON CONFLICT (conversation_id) DO UPDATE SET next_sequence = conversation_counters.next_sequence + 1
		RETURNING next_sequence - 1`, convID.String).Scan(&seq); err != nil {
		seq = 1
	}
	body, _ := json.Marshal(map[string]string{"kind": "joined", "participantId": participantID})
	_, _ = db.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
		VALUES ($1, $2, $3, 'system', $4, $5, $6)`,
		"m-"+randUUID(), convID.String, participantID, string(body), seq, companyID)
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
		SELECT id, 1 FROM conversations
		 WHERE kind = 'direct' AND company_id = $2
		   AND members @> to_jsonb(ARRAY[$1::text]) AND members @> to_jsonb(ARRAY[$3::text])
		   AND jsonb_array_length(members) = 2
		 ON CONFLICT (conversation_id) DO NOTHING`, uid, tenant, agentID)
}

/* ───────── update / offboard / rehire ───────── */

func update(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := requireRole(w, r, db)
		if !ok {
			return
		}
		id := r.PathValue("id")
		var kind string
		if err := db.QueryRowContext(r.Context(),
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
		if len(sets) == 0 {
			httpx.WriteError(w, http.StatusBadRequest, "nothing to update")
			return
		}
		params = append(params, id, tenant)
		if _, err := db.ExecContext(r.Context(),
			fmt.Sprintf(`UPDATE participants SET %s WHERE id = $%d AND company_id = $%d`,
				strings.Join(sets, ", "), len(params)-1, len(params)), params...); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func offboard(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := requireRole(w, r, db)
		if !ok {
			return
		}
		id := r.PathValue("id")
		var kind string
		var departedAt sql.NullTime
		if err := db.QueryRowContext(r.Context(),
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
		if _, err := db.ExecContext(r.Context(), `
			UPDATE participants SET departed_at = NOW(), status = 'resting', status_updated_at = NOW()
			  WHERE id = $1 AND company_id = $2`, id, tenant); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"ok": true, "departedAt": time.Now().UTC(),
		})
	}
}

func rehire(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := requireRole(w, r, db)
		if !ok {
			return
		}
		id := r.PathValue("id")
		var kind string
		var departedAt sql.NullTime
		if err := db.QueryRowContext(r.Context(),
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
		// tier 限:rehire 同 create 的闸。
		uid, _, _ := requireCompany(w, r, db)
		var tier string
		_ = db.QueryRowContext(r.Context(),
			`SELECT COALESCE(tier, 'free') FROM users WHERE id = $1`, uid).Scan(&tier)
		var agentCount int
		_ = db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM participants WHERE company_id = $1 AND kind = 'agent' AND departed_at IS NULL`,
			tenant).Scan(&agentCount)
		if agentCount >= tierAgents(tier) {
			httpx.WriteError(w, http.StatusForbidden,
				fmt.Sprintf("%s tier teams can have at most %d active agents", tier, tierAgents(tier)))
			return
		}
		if _, err := db.ExecContext(r.Context(), `
			UPDATE participants SET departed_at = NULL, status = 'avail', status_updated_at = NOW()
			  WHERE id = $1 AND company_id = $2`, id, tenant); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

/* ───────── autonomy ───────── */

func putAutonomy(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, tenant, ok := requireCompany(w, r, db)
		if !ok {
			return
		}
		var one int
		if err := db.QueryRowContext(r.Context(),
			`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 LIMIT 1`,
			r.PathValue("id"), tenant).Scan(&one); err != nil {
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
		if _, err := db.ExecContext(r.Context(), `
			INSERT INTO agent_autonomy (user_id, agent_id, threshold)
			VALUES ($1, $2, $3)
			ON CONFLICT (user_id, agent_id) DO UPDATE SET threshold = EXCLUDED.threshold`,
			uid, r.PathValue("id"), threshold); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "threshold": threshold})
	}
}

func getAutonomy(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, tenant, ok := requireCompany(w, r, db)
		if !ok {
			return
		}
		rows, err := db.QueryContext(r.Context(), `
			SELECT a.user_id, a.agent_id, a.threshold, a.pulled, a.led, a.dissolved
			  FROM agent_autonomy a
			  JOIN participants p ON p.id = a.agent_id
			 WHERE a.user_id = $1 AND p.company_id = $2`, uid, tenant)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
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
}

/* ───────── 小助手 ───────── */

func randUUID() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randHex6() string {
	b := make([]byte, 3)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}
