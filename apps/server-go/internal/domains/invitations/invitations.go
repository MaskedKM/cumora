// domains/invitations —— 邀请域(#68 补齐):list/create/revoke/preview/
// accept 五路由 + convene 三路由。行为对齐 router.ts 1143–1660 与
// agents/convene.ts、onboardCompany.ts(joinAllHands/seedMemberDms)。
package invitations

import (
	"context"
	"crypto/md5"
	crand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/onboard"
)

const inviteTokenBytes = 32
const inviteMaxLinkUses = 100
const inviteTTL = 7 * 24 * time.Hour

/* ───────── 助手 ───────── */

func hashInviteToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func generateInviteToken() string {
	b := make([]byte, inviteTokenBytes)
	_, _ = crand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func buildInviteURL(token string) string {
	base := config.InviteSignInBase()
	base = strings.TrimRight(base, "/")
	if base == "" {
		base = "http://localhost:5173"
	}
	return base + "/invite/" + token
}

func randUUID() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func requireAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	return httpx.RequireAuth(w, r)
}

// requireCompanyAdmin:路径段公司上的 owner/admin 门(403 两段文案)。
func requireCompanyAdmin(w http.ResponseWriter, r *http.Request, db *sql.DB, companyID string) (string, bool) {
	uid, ok := requireAuth(w, r)
	if !ok {
		return "", false
	}
	var role string
	err := db.QueryRowContext(r.Context(),
		`SELECT role FROM company_members WHERE company_id = $1 AND user_id = $2`,
		companyID, uid).Scan(&role)
	if err == sql.ErrNoRows {
		httpx.WriteError(w, http.StatusForbidden, "not a member of this company")
		return "", false
	}
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return "", false
	}
	if role != "owner" && role != "admin" {
		httpx.WriteError(w, http.StatusForbidden, "only owners and admins can manage invitations")
		return "", false
	}
	return uid, true
}

// companyPlanTier:router.ts companyPlanTier 逐字——属主 users.tier,
// 回退最早加入的 owner 角色成员的 tier(评审 F1:不得取全体成员最优)。
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

func tierHumans(t string) int {
	switch t {
	case "pro":
		return 10
	case "max":
		return 25
	default:
		return 5
	}
}

func tierCompaniesPerUser(t string) int {
	switch t {
	case "pro":
		return 10
	case "max":
		return 25
	default:
		return 3
	}
}

func auditInvite(ctx context.Context, db *sql.DB, kind string, userID, companyID, ip, ua string, detail map[string]any) {
	detailJSON, _ := json.Marshal(detail)
	_, _ = db.ExecContext(ctx, `
		INSERT INTO audit_events (user_id, company_id, ip, user_agent, kind, detail)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)`, nilStr(userID), nilStr(companyID), nilStr(ip), nilStr(ua), kind, string(detailJSON))
}

type nullS = sql.NullString

func nilStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

/* ───────── list ───────── */

func List(db *sql.DB, w http.ResponseWriter, r *http.Request, id string) {
	companyID := id
	if _, ok := requireCompanyAdmin(w, r, db, companyID); !ok {
		return
	}
	rows, err := db.QueryContext(r.Context(), `
		SELECT i.token_hash, i.email, i.role, i.note, i.max_uses, i.use_count,
		       i.created_at, i.expires_at, i.revoked_at,
		       i.last_accepted_at, i.last_accepted_by, i.invited_by,
		       u.display_name
		  FROM company_invitations i
		  LEFT JOIN users u ON u.id = i.invited_by
		 WHERE i.company_id = $1
		 ORDER BY i.created_at DESC
		 LIMIT 200`, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	now := time.Now()
	out := []map[string]any{}
	for rows.Next() {
		var tokenHash, role string
		var email, note, invitedBy sql.NullString
		var maxUses, useCount int
		var createdAt, expiresAt time.Time
		var revokedAt, lastAcceptedAt sql.NullTime
		var lastAcceptedBy, inviterName sql.NullString
		if err := rows.Scan(&tokenHash, &email, &role, &note, &maxUses, &useCount,
			&createdAt, &expiresAt, &revokedAt,
			&lastAcceptedAt, &lastAcceptedBy, &invitedBy, &inviterName); err == nil {
			status := "active"
			switch {
			case revokedAt.Valid:
				status = "revoked"
			case expiresAt.Before(now):
				status = "expired"
			case useCount >= maxUses:
				status = "consumed"
			}
			out = append(out, map[string]any{
				"id": tokenHash, "email": nullTime2Any(email), "role": role,
				"note":    nullTime2Any(note),
				"maxUses": maxUses, "useCount": useCount,
				"createdAt":      httpx.ISOms(createdAt),
				"expiresAt":      httpx.ISOms(expiresAt),
				"revokedAt":      nullTimeUTC(revokedAt),
				"lastAcceptedAt": nullTimeUTC(lastAcceptedAt),
				"lastAcceptedBy": nullTime2Any(lastAcceptedBy),
				"invitedBy":      nullTime2Any(invitedBy),
				"inviterName":    nullTime2Any(inviterName),
				"status":         status,
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func nullTime2Any(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

func nullTimeUTC(nt sql.NullTime) any {
	if !nt.Valid {
		return nil
	}
	return httpx.ISOms(nt.Time)
}

/* ───────── create ───────── */

// validEmail:TS /^[^\s@]+@[^\s@]+\.[^\s@]+$/ —— 单 @ 且无空白,
// 域部含点(评审 F10:旧手写版收 a@b.c@d)。
var emailValidRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

func validEmail(s string) bool {
	return emailValidRe.MatchString(s)
}

func Create(db *sql.DB, w http.ResponseWriter, r *http.Request, id string) {
	companyID := id
	me, ok := requireCompanyAdmin(w, r, db, companyID)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	strOf := func(k string) (string, bool) {
		var v any
		if json.Unmarshal(body[k], &v) != nil {
			return "", false
		}
		s, isStr := v.(string)
		if !isStr {
			return "", false
		}
		return s, true
	}
	rawEmail, _ := strOf("email")
	rawEmail = strings.TrimSpace(rawEmail)
	var email any
	if rawEmail != "" {
		lower := strings.ToLower(rawEmail)
		if !validEmail(lower) {
			httpx.WriteError(w, http.StatusBadRequest, "invalid email")
			return
		}
		email = lower
	}
	role := "member"
	if s, ok := strOf("role"); ok && (s == "member" || s == "admin") {
		role = s
	}
	var note any
	if s, ok := strOf("note"); ok {
		t := strings.TrimSpace(httpx.UTF16Cap(s, 280))
		if t != "" {
			note = t
		}
	}
	emailStr := ""
	if s, ok := email.(string); ok {
		emailStr = s
	}
	maxUses := inviteMaxLinkUses
	if emailStr != "" {
		maxUses = 1
	} else if raw, ok := body["maxUses"]; ok {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			if f, isNum := v.(float64); isNum {
				n := int(f)
				if n < 1 {
					n = 1
				}
				if n > inviteMaxLinkUses {
					n = inviteMaxLinkUses
				}
				maxUses = n
			}
		}
	}
	if emailStr != "" {
		var one int
		if err := db.QueryRowContext(r.Context(), `
			SELECT 1 FROM company_members cm
			  JOIN users u ON u.id = cm.user_id
			 WHERE cm.company_id = $1 AND LOWER(u.email) = $2 LIMIT 1`,
			companyID, emailStr).Scan(&one); err == nil {
			httpx.WriteError(w, http.StatusConflict, "that email is already a member of this team")
			return
		}
		_, _ = db.ExecContext(r.Context(), `
			UPDATE company_invitations SET revoked_at = NOW()
			 WHERE company_id = $1 AND email = $2
			   AND revoked_at IS NULL AND expires_at > NOW()
			   AND use_count < max_uses`, companyID, emailStr)
	}
	tier := companyPlanTier(r.Context(), db, companyID)
	var used int
	_ = db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM company_members WHERE company_id = $1`, companyID).Scan(&used)
	limit := tierHumans(tier)
	remaining := limit - used
	if remaining <= 0 {
		httpx.WriteError(w, http.StatusForbidden,
			fmt.Sprintf("%s tier teams can have at most %d human members", tier, limit))
		return
	}
	if maxUses > remaining {
		maxUses = remaining
	}
	token := generateInviteToken()
	tokenHash := hashInviteToken(token)
	expiresAt := time.Now().Add(inviteTTL)
	if _, err := db.ExecContext(r.Context(), `
		INSERT INTO company_invitations
		  (token_hash, company_id, invited_by, email, role, note, max_uses, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		tokenHash, companyID, me, email, role, note, maxUses, expiresAt); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	noteDetail := any(nil)
	if s, ok := note.(string); ok {
		noteDetail = s
	}
	auditInvite(r.Context(), db, "invitation_create", me, companyID,
		r.RemoteAddr, r.UserAgent(), map[string]any{"email": email, "role": role, "maxUses": maxUses, "note": noteDetail})
	// F11:sendEmail 分支——email 锁定的邀请可代发信;RESEND_API_KEY
	// 空时 provider 走 mock;失败只进 emailDelivery,不炸创建(邀请
	// 行已落库,邀请人手上有 URL)。
	sendEmail := false
	if raw, has := body["sendEmail"]; has {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			if b, isBool := v.(bool); isBool {
				sendEmail = b
			}
		}
	}
	var emailDelivery any
	if sendEmail && emailStr != "" {
		var inviterEmail, displayName string
		inviterOK := db.QueryRowContext(r.Context(),
			`SELECT email, display_name FROM users WHERE id = $1`, me).
			Scan(&inviterEmail, &displayName) == nil
		var companyName string
		companyOK := db.QueryRowContext(r.Context(),
			`SELECT name FROM companies WHERE id = $1`, companyID).Scan(&companyName) == nil
		if inviterOK && companyOK {
			inviterName := displayName
			if inviterName == "" {
				inviterName = inviterEmail
			}
			emailDelivery = sendInvitationEmail(r.Context(), db, invitationEmailArgs{
				To: emailStr, InviterName: inviterName, InviterEmail: inviterEmail,
				CompanyName: companyName, Role: role, Note: note, InviteURL: buildInviteURL(token),
			})
		} else {
			emailDelivery = invitationEmailDelivery{Attempted: false, OK: false, Error: "inviter or company row missing", Skipped: nil}
		}
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id": tokenHash, "token": token, "url": buildInviteURL(token),
		"email": email, "role": role, "note": note,
		"maxUses": maxUses, "useCount": 0,
		"createdAt": httpx.ISOms(time.Now()), "expiresAt": httpx.ISOms(expiresAt),
		"status": "active", "emailDelivery": emailDelivery,
	})
}

/* ───────── revoke ───────── */

func Revoke(db *sql.DB, w http.ResponseWriter, r *http.Request, id string, inviteId string) {
	companyID := id
	inviteID := inviteId
	me, ok := requireCompanyAdmin(w, r, db, companyID)
	if !ok {
		return
	}
	res, err := db.ExecContext(r.Context(), `
		UPDATE company_invitations SET revoked_at = NOW()
		 WHERE token_hash = $1 AND company_id = $2 AND revoked_at IS NULL`, inviteID, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	revoked := false
	if n, _ := res.RowsAffected(); n > 0 {
		revoked = true
		auditInvite(r.Context(), db, "invitation_revoke", me, companyID,
			r.RemoteAddr, r.UserAgent(), map[string]any{"inviteId": inviteID})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": revoked})
}

/* ───────── preview ───────── */

func Preview(db *sql.DB, w http.ResponseWriter, r *http.Request, token string) {
	if len(token) < 8 {
		httpx.WriteError(w, http.StatusBadRequest, "bad token")
		return
	}
	viewerEmail := ""
	if uid, ok := httpx.UserID(r); ok && uid != "" {
		var email string
		if db.QueryRowContext(r.Context(), `SELECT email FROM users WHERE id = $1`, uid).Scan(&email) == nil {
			viewerEmail = strings.ToLower(email)
		}
	}
	invite, status := loadInvitation(r, db, token, viewerEmail)
	// not_found 不带 invitation 键(TS 形状;评审 F9)。
	if invite == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": status})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": status, "invitation": invite})
}

// loadInvitation:状态机 not_found/revoked/expired/consumed/already_member/
// wrong_email/valid;(invite, status) 双返回。
func loadInvitation(r *http.Request, db *sql.DB, token, viewerEmail string) (map[string]any, string) {
	hash := hashInviteToken(token)
	var companyID, role string
	var invEmail, note sql.NullString
	var maxUses, useCount int
	var createdAt, expiresAt time.Time
	var revokedAt sql.NullTime
	var companyName, companySlug string
	var inviterName sql.NullString
	err := db.QueryRowContext(r.Context(), `
		SELECT i.company_id, i.email, i.role, i.note, i.max_uses, i.use_count,
		       i.created_at, i.expires_at, i.revoked_at,
		       c.name, c.slug, u.display_name
		  FROM company_invitations i
		  JOIN companies c ON c.id = i.company_id
		  LEFT JOIN users u ON u.id = i.invited_by
		 WHERE i.token_hash = $1`, hash).Scan(
		&companyID, &invEmail, &role, &note, &maxUses, &useCount,
		&createdAt, &expiresAt, &revokedAt, &companyName, &companySlug, &inviterName)
	if err == sql.ErrNoRows {
		return nil, "not_found"
	}
	if err != nil {
		return nil, "not_found"
	}
	base := map[string]any{
		"role": role, "email": nullTime2Any(invEmail), "note": nullTime2Any(note),
		"createdAt": httpx.ISOms(createdAt), "expiresAt": httpx.ISOms(expiresAt),
		"inviterName": nullTime2Any(inviterName),
		"company":     map[string]any{"id": companyID, "name": companyName, "slug": companySlug},
		"multiUse":    maxUses > 1,
	}
	if revokedAt.Valid {
		return base, "revoked"
	}
	if expiresAt.Before(time.Now()) {
		return base, "expired"
	}
	if useCount >= maxUses {
		return base, "consumed"
	}
	if uid, ok := httpx.UserID(r); ok && uid != "" {
		var one int
		if db.QueryRowContext(r.Context(),
			`SELECT 1 FROM company_members WHERE company_id = $1 AND user_id = $2 LIMIT 1`,
			companyID, uid).Scan(&one) == nil {
			return base, "already_member"
		}
	}
	if invEmail.Valid && viewerEmail != "" && strings.ToLower(invEmail.String) != viewerEmail {
		return base, "wrong_email"
	}
	return base, "valid"
}

/* ───────── accept ───────── */

func Accept(db *sql.DB, w http.ResponseWriter, r *http.Request, token string) {
	me, ok := requireAuth(w, r)
	if !ok {
		return
	}
	if len(token) < 8 {
		httpx.WriteError(w, http.StatusBadRequest, "bad token")
		return
	}
	tokenHash := hashInviteToken(token)
	var email, displayName string
	var avatarURL sql.NullString
	if err := db.QueryRowContext(r.Context(),
		`SELECT email, display_name, avatar_url FROM users WHERE id = $1`, me).
		Scan(&email, &displayName, &avatarURL); err != nil {
		httpx.WriteError(w, http.StatusUnauthorized, "session points to missing user")
		return
	}
	viewerEmail := strings.ToLower(email)
	userAvatar := gravatarURL(email)
	if avatarURL.Valid && avatarURL.String != "" {
		userAvatar = avatarURL.String
	}
	// 事务:F OR UPDATE 防两单用邀请并发双赢。
	// 事务豁免(#213,#235 复审仍留):函数体内 9 处 rollback()+写响应
	// 分支(4xx 文案各异、isMember 回滚后写 200、tier 403 动态文案)——
	// 非"500 文案各异"类,#214 未触及;哨兵错误集无法在合理复杂度下
	// 保持响应字节等价。
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	rollback := func() { _ = tx.Rollback() }
	var companyID, role string
	var invEmail sql.NullString
	var maxUses, useCount int
	var expiresAt time.Time
	var revokedAt sql.NullTime
	err = tx.QueryRowContext(r.Context(), `
		SELECT company_id, email, role, max_uses, use_count, expires_at, revoked_at
		  FROM company_invitations WHERE token_hash = $1 FOR UPDATE`, tokenHash).
		Scan(&companyID, &invEmail, &role, &maxUses, &useCount, &expiresAt, &revokedAt)
	_ = revokedAt
	if err == sql.ErrNoRows {
		rollback()
		httpx.WriteError(w, http.StatusNotFound, "invitation not found")
		return
	}
	if err != nil {
		rollback()
		httpx.WriteInternalError(w, r, err)
		return
	}
	if revokedAt.Valid {
		rollback()
		httpx.WriteError(w, http.StatusGone, "invitation revoked")
		return
	}
	if expiresAt.Before(time.Now()) {
		rollback()
		httpx.WriteError(w, http.StatusGone, "invitation expired")
		return
	}
	if useCount >= maxUses {
		rollback()
		httpx.WriteError(w, http.StatusGone, "invitation already used")
		return
	}
	if invEmail.Valid && strings.ToLower(invEmail.String) != viewerEmail {
		rollback()
		httpx.WriteError(w, http.StatusForbidden,
			"this invitation is reserved for "+invEmail.String+" — sign in with that email to accept")
		return
	}
	var one int
	isMember := tx.QueryRowContext(r.Context(),
		`SELECT 1 FROM company_members WHERE company_id = $1 AND user_id = $2 LIMIT 1`,
		companyID, me).Scan(&one) == nil
	if isMember {
		rollback()
		writeAcceptOK(w, r, db, companyID, me, true)
		return
	}
	// 用户公司数 tier 限。
	var tier string
	_ = tx.QueryRowContext(r.Context(), `SELECT COALESCE(tier,'free') FROM users WHERE id=$1`, me).Scan(&tier)
	var nCompanies int
	_ = tx.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM company_members WHERE user_id = $1`, me).Scan(&nCompanies)
	if nCompanies >= tierCompaniesPerUser(tier) {
		rollback()
		httpx.WriteError(w, http.StatusForbidden,
			fmt.Sprintf("%s tier users can belong to at most %d companies", tier, tierCompaniesPerUser(tier)))
		return
	}
	coTier := companyPlanTier(r.Context(), db, companyID)
	var used int
	_ = tx.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM company_members WHERE company_id = $1`, companyID).Scan(&used)
	if used >= tierHumans(coTier) {
		rollback()
		httpx.WriteError(w, http.StatusForbidden,
			fmt.Sprintf("%s tier teams can have at most %d human members", coTier, tierHumans(coTier)))
		return
	}
	_, _ = tx.ExecContext(r.Context(), `
		INSERT INTO company_members (company_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (company_id, user_id) DO NOTHING`, companyID, me, role)
	initial := strings.ToUpper(firstRune(displayName))
	_, _ = tx.ExecContext(r.Context(), `
		INSERT INTO participants (id, kind, name, role, initial, avatar_bg, avatar_url, status, company_id)
		VALUES ($1, 'human', $2, NULL, $3, '#FF8870', $4, 'avail', $5)
		ON CONFLICT (id, company_id) DO NOTHING`, me, displayName, initial, userAvatar, companyID)
	_, _ = tx.ExecContext(r.Context(), `
		UPDATE company_invitations
		   SET use_count = use_count + 1, last_accepted_at = NOW(), last_accepted_by = $2
		 WHERE token_hash = $1`, tokenHash, me)
	if err := tx.Commit(); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	onboard.JoinAllHands(r.Context(), db, companyID, me)
	seedMemberDms(r.Context(), db, companyID, me)
	var invitedBy sql.NullString
	_ = db.QueryRowContext(r.Context(),
		`SELECT invited_by FROM company_invitations WHERE token_hash = $1`, tokenHash).Scan(&invitedBy)
	auditInvite(r.Context(), db, "invitation_accept", me, companyID,
		r.RemoteAddr, r.UserAgent(), map[string]any{"role": role, "invitedBy": nullTime2Any(invitedBy)})
	writeAcceptOK(w, r, db, companyID, me, false)
}

func writeAcceptOK(w http.ResponseWriter, r *http.Request, db *sql.DB, companyID, me string, already bool) {
	var name, slug string
	var role sql.NullString
	_ = db.QueryRowContext(r.Context(), `
		SELECT c.name, c.slug, cm.role
		  FROM companies c JOIN company_members cm ON cm.company_id = c.id AND cm.user_id = $2
		 WHERE c.id = $1`, companyID, me).Scan(&name, &slug, &role)
	roleStr := "member"
	if role.Valid {
		roleStr = role.String
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok": true, "alreadyMember": already,
		"company": map[string]any{"id": companyID, "name": name, "slug": slug, "role": roleStr},
	})
}

// gravatarURL:TS gravatarUrlForEmail = MD5(评审 F4;SHA-256 是走眼)。
func gravatarURL(email string) string {
	sum := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
	return fmt.Sprintf("https://www.gravatar.com/avatar/%x?d=identicon&s=256", sum)
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return "?"
}

/* ───────── convene ───────── */

func requireConvoMember(w http.ResponseWriter, r *http.Request, db *sql.DB, convoID string) (string, string, bool) {
	uid, companyID, ok := httpx.RequireCompany(w, r, db)
	if !ok {
		return "", "", false
	}
	var membersJSON, kind string
	err := db.QueryRowContext(r.Context(),
		`SELECT members::text, kind FROM conversations WHERE id = $1 AND company_id = $2 LIMIT 1`,
		convoID, companyID).Scan(&membersJSON, &kind)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return "", "", false
	}
	var members []string
	_ = json.Unmarshal([]byte(membersJSON), &members)
	for _, m := range members {
		if m == uid {
			return uid, companyID, true
		}
	}
	httpx.WriteError(w, http.StatusNotFound, "not found")
	return "", "", false
}

func StartConvene(db *sql.DB, w http.ResponseWriter, r *http.Request, id string) {
	me, _, ok := requireConvoMember(w, r, db, id)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	// F16:TS 是 String(body.topic ?? 'live work session')——非串值
	// 强转(123→"123"),仅 null/缺省落默认。
	topic := "live work session"
	if raw, has := body["topic"]; has {
		var v any
		if json.Unmarshal(raw, &v) == nil && v != nil {
			topic = httpx.JSToString(v)
		}
	}
	var convoTitle string
	var membersJSON string
	if err := db.QueryRowContext(r.Context(),
		`SELECT title, members::text FROM conversations WHERE id = $1`, id).
		Scan(&convoTitle, &membersJSON); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	// F6:Go 侧无服务端编排,会话无人终结——新会话开场即了结本会话
	// 既有 live(被新会话取代),恢复 TS 编排自然结束时的终态。
	sweepStaleConvene(r.Context(), db, id, true)
	sessionID := "cs-" + randUUID()
	flair := httpx.UTF16Cap(topic, 80)
	startedAt := time.Now().UTC()
	_, _ = db.ExecContext(r.Context(), `
		INSERT INTO convene_sessions (id, conversation_id, title, flair, started_by, started_at, state)
		VALUES ($1, $2, $3, $4, $5, NOW(), 'live')`,
		sessionID, id, convoTitle+" · live", flair, me)
	session := map[string]any{
		"id": sessionID, "conversation_id": id,
		"title": convoTitle + " · live", "flair": flair,
		"started_by": me, "started_at": httpx.ISOms(startedAt), "ended_at": nil, "state": "live",
	}
	var companyID sql.NullString
	_ = db.QueryRowContext(r.Context(),
		`SELECT company_id FROM conversations WHERE id = $1`, id).Scan(&companyID)
	payload := map[string]any{
		"type": "convene", "sessionId": sessionID, "conversationId": id,
		"kind": "started", "data": session,
	}
	if companyID.Valid {
		payload["companyId"] = companyID.String
	}
	_ = events.PublishRaw(r.Context(), events.ChConvene, mustMJSON(payload))
	// 编排(orchestrate)是服务端 agent-turn 引擎——BYOA 化后由成员
	// daemon 经常规唤醒参与;此处不移植服务端编排,transcript 在测试
	// 形态两侧同为空(测试只断言列表形状)。
	httpx.WriteJSON(w, http.StatusOK, session)
}

/* ───────── convene(F6 清扫 + 端点) ───────── */

// sweepStaleConvene 了结指定会话的 live convene 会话:supersede=true 时
// 全部(Go 无服务端编排,新会话开场即取代旧 live);否则仅超时僵尸
// (started_at < NOW() - 30min)。每个被了结的会话补发 TS orchestrate
// 结束时的 convene ended 事件,保持订阅端状态一致。
func sweepStaleConvene(ctx context.Context, db *sql.DB, conversationID string, supersede bool) {
	stale := `
		SELECT s.id, s.conversation_id, c.company_id
		  FROM convene_sessions s
		  JOIN conversations c ON c.id = s.conversation_id
		 WHERE s.conversation_id = $1 AND s.state = 'live'
		   AND ($2::bool OR s.started_at < NOW() - INTERVAL '30 minutes')`
	rows, err := db.QueryContext(ctx, stale, conversationID, supersede)
	if err != nil {
		return
	}
	type swept struct {
		id, convID string
		companyID  sql.NullString
	}
	var all []swept
	for rows.Next() {
		var s swept
		if rows.Scan(&s.id, &s.convID, &s.companyID) == nil {
			all = append(all, s)
		}
	}
	rows.Close()
	for _, s := range all {
		if _, err := db.ExecContext(ctx,
			`UPDATE convene_sessions SET state = 'ended', ended_at = NOW() WHERE id = $1`, s.id); err != nil {
			continue
		}
		payload := map[string]any{
			"type": "convene", "sessionId": s.id, "conversationId": s.convID,
			"kind": "ended",
		}
		if s.companyID.Valid {
			payload["companyId"] = s.companyID.String
		}
		_ = events.PublishRaw(ctx, events.ChConvene, mustMJSON(payload))
	}
}

func ActiveConvene(db *sql.DB, w http.ResponseWriter, r *http.Request, id string) {
	if _, _, ok := requireConvoMember(w, r, db, id); !ok {
		return
	}
	// F6:读侧兜底——超时(30min)live 僵尸就地了结(TS 编排崩溃同样
	// 会留 live 僵尸,30min 远超编排时长,不影响真进行中的会话)。
	sweepStaleConvene(r.Context(), db, id, false)
	var sessionID, title, flair, startedBy, state string
	var startedAt time.Time
	var endedAt sql.NullTime
	err := db.QueryRowContext(r.Context(), `
		SELECT id, title, flair, started_by, started_at, ended_at, state
		  FROM convene_sessions
		 WHERE conversation_id = $1 AND state = 'live'
		 ORDER BY started_at DESC LIMIT 1`, id).
		Scan(&sessionID, &title, &flair, &startedBy, &startedAt, &endedAt, &state)
	if err == sql.ErrNoRows {
		httpx.WriteJSON(w, http.StatusOK, nil)
		return
	}
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": sessionID, "conversation_id": id,
		"title": title, "flair": flair, "started_by": startedBy,
		"started_at": httpx.ISOms(startedAt), "ended_at": nullTimeUTC(endedAt), "state": state,
	})
}

func ConveneTranscript(db *sql.DB, w http.ResponseWriter, r *http.Request, sessionId string) {
	me, tenant, ok := httpx.RequireCompany(w, r, db)
	if !ok {
		return
	}
	sessionID := sessionId
	var membersJSON string
	err := db.QueryRowContext(r.Context(), `
		SELECT c.members::text
		  FROM convene_sessions s
		  JOIN conversations c ON c.id = s.conversation_id
		 WHERE s.id = $1 AND c.company_id = $2 LIMIT 1`, sessionID, tenant).Scan(&membersJSON)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	var members []string
	_ = json.Unmarshal([]byte(membersJSON), &members)
	isMember := false
	for _, m := range members {
		if m == me {
			isMember = true
			break
		}
	}
	if !isMember {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	rows, err := db.QueryContext(r.Context(), `
		SELECT id, session_id, author_id, kind, body, sequence, decision, created_at
		  FROM convene_transcript WHERE session_id = $1
		 ORDER BY sequence ASC`, sessionID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, sessID, authorID, kind, body string
		var sequence int
		var decision []byte
		var createdAt time.Time
		if rows.Scan(&id, &sessID, &authorID, &kind, &body, &sequence, &decision, &createdAt) == nil {
			var decisionAny any
			_ = json.Unmarshal(decision, &decisionAny)
			out = append(out, map[string]any{
				"id": id, "sessionId": sessID, "authorId": authorID,
				"kind": kind, "body": body, "sequence": sequence,
				"decision": decisionAny, "createdAt": httpx.ISOms(createdAt),
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

/* ───────── all-hands / DM 种子(onboardCompany.ts 同语义;
   joinAllHands 已合一到 onboard.JoinAllHands —— F7) ───────── */

func seedMemberDms(ctx context.Context, db *sql.DB, companyID, memberID string) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, kind FROM participants
		 WHERE company_id = $1 AND id <> $2 AND departed_at IS NULL`, companyID, memberID)
	if err != nil {
		return
	}
	type other struct{ id, name, kind string }
	var others []other
	for rows.Next() {
		var o other
		if rows.Scan(&o.id, &o.name, &o.kind) == nil {
			others = append(others, o)
		}
	}
	rows.Close()
	for _, o := range others {
		var one int
		if db.QueryRowContext(ctx, `
			SELECT 1
			   FROM conversation_members ca
			   JOIN conversation_members cb ON cb.conversation_id = ca.conversation_id
			   JOIN conversations c ON c.id = ca.conversation_id
			  WHERE ca.participant_id = $2 AND cb.participant_id = $3
			    AND c.company_id = $1 AND c.kind = 'direct'
			    AND jsonb_array_length(c.members) = 2 LIMIT 1`, companyID, memberID, o.id).Scan(&one) == nil {
			continue
		}
		dmID := "direct-" + o.id + "-" + randHex6()
		var tag any
		if o.kind == "human" {
			tag = "human"
		}
		membersJSON, _ := json.Marshal([]string{memberID, o.id})
		_, _ = db.ExecContext(ctx, `
			INSERT INTO conversations (id, kind, title, subtitle, members, pinned, tag, company_id)
			VALUES ($1, 'direct', $2, NULL, $3::jsonb, FALSE, $4, $5)
			ON CONFLICT (id) DO NOTHING`, dmID, o.name, string(membersJSON), tag, companyID)
		_, _ = db.ExecContext(ctx, `
			INSERT INTO conversation_counters (conversation_id, next_sequence) VALUES ($1, 1)
			ON CONFLICT (conversation_id) DO NOTHING`, dmID)
	}
}

func randHex6() string {
	b := make([]byte, 3)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}

func mustMJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
