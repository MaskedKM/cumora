// domains/admin/waitlist —— #124(#117-e):等待名单子面。GET 列表
// (status/q 过滤 +分页)、POST approve(建真号 = oauth Path C 同型:
// 事务内建号/身份/个人区,post-commit 尽力而为 all-hands + 欢迎邮件)、
// POST reject(记 note)。逐段对齐 api/admin-router.ts 330–363 与
// admin.ts 133–254、433–634。
package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	core "github.com/MaskedKM/cumora/apps/server-go/internal/domains/core"
	"github.com/MaskedKM/cumora/apps/server-go/internal/email"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/onboard"
)

type waitlistRowDb struct {
	id          string
	provider    string
	providerID  string
	email       string
	displayName string
	avatarURL   sql.NullString
	status      string
	note        sql.NullString
	requestedAt time.Time
	decidedAt   sql.NullTime
	decidedBy   sql.NullString
}

func (r *waitlistRowDb) toWire() map[string]any {
	return map[string]any{
		"id": r.id, "provider": r.provider, "providerId": r.providerID,
		"email": r.email, "displayName": r.displayName,
		"avatarUrl":   avatarOrGravatar(r.avatarURL, r.email),
		"status":      r.status,
		"note":        nullStrAny(r.note),
		"requestedAt": isoTime(r.requestedAt),
		"decidedAt":   nullTimeAny(r.decidedAt),
		"decidedBy":   nullStrAny(r.decidedBy),
	}
}

const waitlistCols = `id, provider, provider_id, email, display_name, avatar_url, status, note, requested_at, decided_at, decided_by`

func scanWaitlistRow(scanner interface{ Scan(...any) error }, row *waitlistRowDb) error {
	return scanner.Scan(&row.id, &row.provider, &row.providerID, &row.email, &row.displayName,
		&row.avatarURL, &row.status, &row.note, &row.requestedAt, &row.decidedAt, &row.decidedBy)
}

func waitlistList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		statusParam := r.URL.Query().Get("status")
		status := ""
		if statusParam == "pending" || statusParam == "approved" || statusParam == "rejected" {
			status = statusParam
		}
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		limit := int(min(500, max(1, numOrDefault(r.URL.Query().Get("limit"), 50))))
		offset := int(max(0, numOrDefault(r.URL.Query().Get("offset"), 0)))

		where := []string{}
		var params []any
		if status != "" {
			params = append(params, status)
			where = append(where, fmt.Sprintf(`status = $%d`, len(params)))
		}
		if q != "" {
			params = append(params, "%"+strings.ToLower(q)+"%")
			n := len(params)
			where = append(where, fmt.Sprintf(`(
	      LOWER(email) LIKE $%[1]d OR LOWER(display_name) LIKE $%[1]d
	      OR LOWER(provider) LIKE $%[1]d OR LOWER(provider_id) LIKE $%[1]d
	      OR LOWER(COALESCE(note, '')) LIKE $%[1]d
	    )`, n))
		}
		whereSql := ""
		if len(where) > 0 {
			whereSql = "WHERE " + strings.Join(where, " AND ")
		}
		var total int
		_ = db.QueryRowContext(r.Context(),
			`SELECT COUNT(*)::int FROM waitlist `+whereSql, params...).Scan(&total)
		params = append(params, limit, offset)
		rows, err := db.QueryContext(r.Context(), fmt.Sprintf(`
			SELECT %s FROM waitlist %s
			  ORDER BY requested_at DESC LIMIT $%d OFFSET $%d`,
			waitlistCols, whereSql, len(params)-1, len(params)), params...)
		if err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var row waitlistRowDb
			if scanWaitlistRow(rows, &row) == nil {
				items = append(items, row.toWire())
			}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"items": items, "total": total, "limit": limit, "offset": offset,
		})
	}
}

func waitlistApprove(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := requireAdmin(w, r, db)
		if !ok {
			return
		}
		userID, companyID, httpErr, err := approveWaitlistRow(r.Context(), db, r.PathValue("id"), adminID)
		if err != nil {
			if httpErr != 0 {
				// 受控域错(HttpError 语义:状态+文案由 fail() 显式给定)
				httpx.WriteError(w, httpErr, err.Error())
			} else {
				httpx.WriteInternalError(w, r, err)
			}
			return
		}
		var companyIDWire any
		if companyID != "" {
			companyIDWire = companyID
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"userId": userID, "companyId": companyIDWire})
	}
}

// approveWaitlistRow:事务 = SELECT FOR UPDATE → 已存在同 email 拒 →
// 待决邀请嗅探(跳个人区)→ 建号+身份+(个人区 SAVEPOINT 重试 slug)
// → avatar 镜像 → waitlist 盖章。post-commit:all-hands + 邮件,尽力
// 而为(sub2api 未配置,门 no-op —— 同 oauth Path C)。
func approveWaitlistRow(ctx context.Context, db *sql.DB, waitlistID, decidedBy string) (string, string, int, error) {
	fail := func(status int, msg string) (string, string, int, error) {
		return "", "", status, errors.New(msg)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", 0, err
	}
	defer tx.Rollback()
	var row waitlistRowDb
	if err := scanWaitlistRow(tx.QueryRowContext(ctx,
		`SELECT `+waitlistCols+` FROM waitlist WHERE id = $1 FOR UPDATE`, waitlistID), &row); err != nil {
		return fail(http.StatusNotFound, "waitlist entry not found")
	}
	if row.status != "pending" {
		return fail(http.StatusConflict, "already "+row.status)
	}

	// 同 email 用户已冒出(等待期间走了别的 provider):不自动绑,提示
	// 管理员改 reject 让本人走既有路径。
	var existing string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE LOWER(email) = $1 LIMIT 1`, strings.ToLower(row.email)).
		Scan(&existing); err == nil {
		return fail(http.StatusConflict, "a user with email "+row.email+" already exists; reject this entry")
	}

	// 邀请先于账号:有活跃邀请则跳个人区(来加入别人的区,不自建)。
	var pendingInv string
	hasInvite := tx.QueryRowContext(ctx, `
		SELECT token_hash FROM company_invitations
		 WHERE LOWER(email) = $1 AND revoked_at IS NULL AND expires_at > NOW() AND use_count < max_uses
		 LIMIT 1`, strings.ToLower(row.email)).Scan(&pendingInv) == nil

	userID := "u-" + randHexID(12)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, password_hash, email_verified_at, is_admin)
		   VALUES ($1, $2, $3, NULL, NOW(), $4)`,
		userID, row.email, row.displayName, isAllowlistedAdmin(row.email)); err != nil {
		return "", "", 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_identities (provider, provider_id, user_id, email_lower)
		   VALUES ($1, $2, $3, $4)`,
		row.provider, row.providerID, userID, strings.ToLower(row.email)); err != nil {
		return "", "", 0, err
	}
	companyID := ""
	if !hasInvite {
		companyID = "co-" + randHexID(10)
		local := strings.SplitN(row.email, "@", 2)[0]
		seed := slugSeedFrom(local)
		slug := seed
		for attempt := 0; attempt < 5; attempt++ {
			// SAVEPOINT/ROLLBACK TO:重复键会中断整个事务,不落存点
			// 重试 INSERT 会撞 "transaction aborted"(短 localpart 撞名
			// 高发,bulk approve 热路径)。
			_, _ = tx.ExecContext(ctx, `SAVEPOINT company_ins`)
			_, err := tx.ExecContext(ctx,
				`INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
				companyID, row.displayName+"'s team", slug, userID)
			if err == nil {
				_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT company_ins`)
				break
			}
			_, _ = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT company_ins`)
			slug = seed + "-" + randHexID(4)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO company_members (company_id, user_id, role) VALUES ($1, $2, 'owner')`,
			companyID, userID); err != nil {
			return "", "", 0, err
		}
	}

	// provider 头像镜像 → users.avatar_url(后续受邀进第二区同一张脸)。
	avatar := core.MirrorAvatar(userID, row.avatarURL.String)
	if avatar == "" {
		avatar = avatarOrGravatar(sql.NullString{}, row.email).(string)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET avatar_url = $1 WHERE id = $2`, avatar, userID); err != nil {
		return "", "", 0, err
	}
	if companyID != "" {
		initial := strings.ToUpper(firstRune(row.displayName))
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO participants (id, kind, name, role, initial, avatar_bg, avatar_url, status, company_id)
			   VALUES ($1, 'human', $2, NULL, $3, '#FF8870', $4, 'avail', $5)
			   ON CONFLICT (id, company_id) DO NOTHING`,
			userID, row.displayName, initial, avatar, companyID); err != nil {
			return "", "", 0, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE waitlist SET status = 'approved', decided_at = NOW(), decided_by = $2 WHERE id = $1`,
		waitlistID, decidedBy); err != nil {
		return "", "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", "", 0, err
	}

	// post-commit 尽力而为(同 oauth Path C 姿态)。
	if companyID != "" {
		onboard.JoinAllHands(ctx, db, companyID, userID)
	}
	sendWaitlistApprovedEmail(ctx, row.email, row.displayName)
	return userID, companyID, 0, nil
}

func waitlistReject(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := requireAdmin(w, r, db)
		if !ok {
			return
		}
		// TS:note 是字符串就原样入库(空串也是 ''),非字符串 → null。
		var note sql.NullString
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			if raw, has := body["note"]; has {
				var s string
				if json.Unmarshal(raw, &s) == nil {
					note = sql.NullString{String: s, Valid: true}
				}
			}
		}
		res, err := db.ExecContext(r.Context(), `
			UPDATE waitlist SET status = 'rejected', decided_at = NOW(), decided_by = $2, note = $3
			 WHERE id = $1 AND status = 'pending'`, r.PathValue("id"), adminID, note)
		if err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			httpx.WriteError(w, http.StatusNotFound, "no pending waitlist entry")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// slugSeedFrom:TS localpart.replace(/[^a-z0-9]+/g,'-').slice(0,30)
// || 'workspace'(与 core oauthSlugSeed 同型;core 未导出,就地同型)。
func slugSeedFrom(local string) string {
	low := strings.ToLower(local)
	var b strings.Builder
	prevDash := false
	for _, c := range low {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := b.String()
	if len(out) > 30 {
		out = out[:30]
	}
	if out == "" {
		return "workspace"
	}
	return out
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return "?"
}

/* ───────── 欢迎邮件(admin.ts buildWelcomeEmailHtml/sendWaitlistApprovedEmail) ───────── */

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")

// approvedEntryURL:给入口 URL 标 approved=1(web 兜底可放行下载);
// 非 http(s) 或解析失败 → 空。
func approvedEntryURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	q := u.Query()
	q.Set("approved", "1")
	u.RawQuery = q.Encode()
	return u.String()
}

// sendWaitlistApprovedEmail:尽力而为;EMAIL_DOMAIN 未配 → 跳过;
// mock(RESEND 空)由 provider 层吞发。HTML 模板对齐 TS 的无 CDN 形态
// (R2_PUBLIC_BASE 未配时 TS 同样省略图行;本部署即此形态)。
func sendWaitlistApprovedEmail(ctx context.Context, to, displayName string) {
	if os.Getenv("EMAIL_DOMAIN") == "" {
		return
	}
	signInBase := os.Getenv("INVITE_BASE_URL")
	if signInBase == "" {
		signInBase = os.Getenv("AUTH_DONE_URL")
	}
	httpURL := approvedEntryURL(signInBase)
	parts := strings.Fields(displayName)
	firstName := "there"
	if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
		firstName = strings.TrimSpace(parts[0])
	}
	var cta string
	if httpURL != "" {
		cta = "Sign in here: " + httpURL
	} else {
		cta = "Open the Cumora app and sign in with the same account you used to join the waitlist."
	}
	text := strings.Join([]string{
		"Hi " + firstName + ",", "",
		"You're in — welcome to Cumora.", "",
		"Your team is set up — your starter teammates (Atlas, Iris, Bram, Nova) are already gathered and ready to meet you.", "",
		cta,
		"Use the same Google or GitHub account you used to join the waitlist.", "",
		"Don't have the desktop app yet? Get it at https://cumora.ai/?download=1#download", "",
		"Reply to this email if anything trips you up — a real person reads it.", "",
		"— Cumora",
	}, "\n")
	_ = email.SendViaProvider(ctx, email.SendArgs{
		From:          email.FormatAddress("welcome@"+os.Getenv("EMAIL_DOMAIN"), "Cumora"),
		To:            []string{to},
		Subject:       "You're in — welcome to Cumora",
		Text:          text,
		HTML:          welcomeEmailHTML(firstName, httpURL),
		AutoSubmitted: "auto-generated",
	})
}

func welcomeEmailHTML(firstName, signInURL string) string {
	fontStack := `'Manrope', -apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif`
	name := htmlEscaper.Replace(firstName)
	var ctaBlock string
	if signInURL != "" {
		ctaBlock = `
                  <tr>
                    <td style="padding:8px 0 0;">
                      <table role="presentation" cellpadding="0" cellspacing="0" border="0">
                        <tr>
                          <td bgcolor="#00A8F0" style="border-radius:8px; background:#00A8F0; mso-padding-alt:14px 28px;">
                            <a href="` + signInURL + `" target="_blank" style="display:inline-block; padding:14px 28px; font-family:` + fontStack + `; font-size:14px; font-weight:600; line-height:1; color:#FFFFFF; text-decoration:none; letter-spacing:0.01em;">
                              Open Cumora &nbsp;→
                            </a>
                          </td>
                        </tr>
                      </table>
                    </td>
                  </tr>`
	} else {
		ctaBlock = `
                  <tr>
                    <td style="padding:4px 0 0; font-family:` + fontStack + `; font-size:15px; font-weight:600; color:#4E3F8C;">
                      Open the Cumora app and sign in with the account you used to join the waitlist.
                    </td>
                  </tr>`
	}
	return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <meta name="color-scheme" content="light" />
  <meta name="supported-color-schemes" content="light" />
  <title>Welcome to Cumora</title>
</head>
<body style="margin:0; padding:0; background:#FAFCFE; color:#0A1B2E; font-family:` + fontStack + `;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#FAFCFE;">
    <tr>
      <td align="center" style="padding:24px 16px 48px;">
        <table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" style="max-width:600px; width:100%;">
          <tr>
            <td align="center" style="padding:36px 0 24px; font-family:` + fontStack + `; font-size:18px; font-weight:700; color:#0A1B2E; letter-spacing:-0.01em;">
              Cumora
            </td>
          </tr>
          <tr>
            <td style="background:#FFFFFF; border-radius:16px; padding:44px 44px 40px; box-shadow:0 1px 0 #E5ECF2;">
              <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
                <tr>
                  <td style="font-family:` + fontStack + `; font-size:36px; font-weight:800; line-height:1.1; color:#0A1B2E; letter-spacing:-0.02em; padding:0 0 8px;">
                    You&rsquo;re in.
                  </td>
                </tr>
                <tr>
                  <td style="font-family:` + fontStack + `; font-size:18px; font-weight:500; line-height:1.45; color:#233A53; padding:0 0 24px;">
                    Welcome to Cumora, ` + name + `.
                  </td>
                </tr>
                <tr>
                  <td style="font-family:` + fontStack + `; font-size:15px; font-weight:400; line-height:1.65; color:#233A53; padding:0 0 28px;">
                    Your team is set up &mdash; your starter teammates Atlas, Iris, Bram, and Nova are already gathered and ready to meet you. Sign in with the same Google or GitHub account you used to join the waitlist, and you&rsquo;ll land right in.
                  </td>
                </tr>` + ctaBlock + `
                <tr>
                  <td style="font-family:` + fontStack + `; font-size:12.5px; font-weight:400; line-height:1.55; color:#94A8BC; padding:14px 0 0;">
                    Don&rsquo;t have the desktop app yet?
                    <a href="https://cumora.ai/?download=1#download" style="color:#3E6FA8; text-decoration:none; font-weight:600;">Get it for macOS, Windows, or Linux</a>.
                  </td>
                </tr>
                <tr>
                  <td style="padding:32px 0 0;">
                    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
                      <tr><td style="border-top:1px solid #E5ECF2; line-height:0; font-size:0;">&nbsp;</td></tr>
                    </table>
                  </td>
                </tr>
                <tr>
                  <td style="font-family:` + fontStack + `; font-size:13px; font-weight:400; line-height:1.55; color:#5B7186; padding:20px 0 0;">
                    Cumora is where small teams gather with AI agents as first-class teammates. We&rsquo;re early; expect rough edges and steady polish. Reply to this email if anything trips you up &mdash; a real person reads it.
                  </td>
                </tr>
              </table>
            </td>
          </tr>
          <tr>
            <td align="center" style="font-family:` + fontStack + `; font-size:12px; font-weight:400; line-height:1.5; color:#94A8BC; padding:24px 0 0;">
              &copy; Cumora &middot; Where teams gather
            </td>
          </tr>
        </table>
      </td>
    </tr>
  </table>
</body>
</html>`
}
