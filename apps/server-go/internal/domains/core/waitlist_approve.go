// waitlist_approve —— 管理面审批的入伙机器(#124):POST /api/admin/
// waitlist/:id/approve 的域原语。逐段对齐 server/src/admin.ts
// approveWaitlist(sendWaitlistApprovedEmail/buildWelcomeEmailHtml 同文件):
// FOR UPDATE 锁行 → 邮箱查重 → 待处理邀请嗅探(跳过个人工作区)→
// users+user_identities+companies(slug SAVEPOINT 重试)+company_members+
// participants → waitlist 置 approved,单事务;提交后 #all-hands 尽力、
// 欢迎信尽力、sub2api 供给按 #109 延后为 no-op。
// 与 oauthFindOrCreate Path C 同构但不可复用:那边假定 exchange+profile
// 前奏刚跑完,这边的一切都从 waitlist 行取。
package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/email"
	"github.com/MaskedKM/cumora/apps/server-go/internal/onboard"
)

// 哨兵错误:admin 域 HTTP 层据此映射 404/409(TS HttpError 语义)。
var ErrWaitlistNotFound = errors.New("waitlist entry not found")

type WaitlistConflictError struct{ Status string }

func (e *WaitlistConflictError) Error() string { return "already " + e.Status }

type WaitlistEmailExistsError struct{ Email string }

func (e *WaitlistEmailExistsError) Error() string {
	return fmt.Sprintf("a user with email %s already exists; reject this entry", e.Email)
}

type waitlistRow struct {
	id          string
	provider    string
	providerID  string
	email       string
	displayName string
	avatarURL   string // NULL → ""
	status      string
	requestedAt string
}

// ApproveWaitlist:审批主流程;返回新 mint 的 userId 与个人工作区 id
// (有待处理邀请时为 nil —— 该用户来加入别人的区,不自建)。
func ApproveWaitlist(ctx context.Context, db *sql.DB, waitlistID, decidedBy string) (string, *string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()

	var row waitlistRow
	var avatarNull sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT id, provider, provider_id, email, display_name, avatar_url, status
		   FROM waitlist WHERE id = $1 FOR UPDATE`, waitlistID).
		Scan(&row.id, &row.provider, &row.providerID, &row.email, &row.displayName, &avatarNull, &row.status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrWaitlistNotFound
	}
	if err != nil {
		return "", nil, err
	}
	if avatarNull.Valid {
		row.avatarURL = avatarNull.String
	}
	if row.status != "pending" {
		return "", nil, &WaitlistConflictError{row.status}
	}

	// 邮箱已有用户(等待期被另一 provider 落地):不自动绑,把冲突亮给
	// 管理员——删行让人家走既有登录即可。
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE LOWER(email) = $1 LIMIT 1`, strings.ToLower(row.email)).Scan(&existing)
	if err == nil {
		return "", nil, &WaitlistEmailExistsError{row.email}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}

	// 待处理邀请嗅探:先点了邀请链、后撞 waitlist 门的人,审批后直奔
	// 目标工作区,不自建个人区(镜像 oauth Path C 的 inviteToken 门)。
	var pendingInv string
	_ = tx.QueryRowContext(ctx,
		`SELECT token_hash FROM company_invitations
		  WHERE LOWER(email) = $1
		    AND revoked_at IS NULL
		    AND expires_at > NOW()
		    AND use_count < max_uses
		  LIMIT 1`, strings.ToLower(row.email)).Scan(&pendingInv)
	skipPersonalWorkspace := pendingInv != ""

	userID := "u-" + oauthRandHex(12)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, password_hash, email_verified_at, is_admin, tier)
		   VALUES ($1, $2, $3, NULL, NOW(), $4, 'free')`,
		userID, row.email, row.displayName, oauthIsAllowlistedAdmin(row.email)); err != nil {
		return "", nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_identities (provider, provider_id, user_id, email_lower)
		   VALUES ($1, $2, $3, $4)`,
		row.provider, row.providerID, userID, strings.ToLower(row.email)); err != nil {
		return "", nil, err
	}

	var companyID *string
	if !skipPersonalWorkspace {
		co := "co-" + oauthRandHex(10)
		slugSeed := oauthSlugSeed(strings.SplitN(row.email, "@", 2)[0])
		finalSlug := slugSeed
		// TS 5 次重试;每次 SAVEPOINT —— PG 事务内语句失败即 aborted,
		// 不回滚到存点的话重试 INSERT 会 25P02(短 localpart 批量审批
		// 时撞 slug 是常态,不是理论)。
		insErr := error(nil)
		for attempt := 0; attempt < 5; attempt++ {
			_, _ = tx.ExecContext(ctx, "SAVEPOINT company_ins")
			_, insErr = tx.ExecContext(ctx,
				`INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
				co, row.displayName+"'s team", finalSlug, userID)
			if insErr == nil {
				_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT company_ins")
				break
			}
			if !oauthIsDupKey(insErr) {
				return "", nil, insErr
			}
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT company_ins")
			finalSlug = fmt.Sprintf("%s-%s", slugSeed, oauthRandHex(4))
		}
		if insErr != nil {
			return "", nil, insErr
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO company_members (company_id, user_id, role) VALUES ($1, $2, 'owner')`,
			co, userID); err != nil {
			return "", nil, err
		}
		companyID = &co
	}

	// provider 头像转存本地;TS 审批语义 = mirror 失败(或空)落
	// gravatar——oauthMirrorAvatar 失败时回退原 URL,据此可区分:返回值
	// 仍等于输入即未成功转存(成功路径是本地 /uploads URL)。
	avatar := oauthMirrorAvatar(userID, row.avatarURL)
	if avatar == row.avatarURL {
		avatar = gravatarURL(row.email)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET avatar_url = $1 WHERE id = $2`, avatar, userID); err != nil {
		return "", nil, err
	}

	if companyID != nil {
		initial := strings.ToUpper(oauthFirstRune(row.displayName))
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO participants (id, kind, name, role, initial, avatar_bg, avatar_url, status, company_id)
			   VALUES ($1, 'human', $2, NULL, $3, '#FF8870', $4, 'avail', $5)
			   ON CONFLICT (id, company_id) DO NOTHING`,
			userID, row.displayName, initial, avatar, *companyID); err != nil {
			return "", nil, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE waitlist SET status = 'approved', decided_at = NOW(), decided_by = $2 WHERE id = $1`,
		waitlistID, decidedBy); err != nil {
		return "", nil, err
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}

	// 提交后副作用:全部尽力而为,绝不倒滚审批本身。
	if companyID != nil {
		onboard.JoinAllHands(ctx, db, *companyID, userID)
	}
	sendWaitlistApprovedEmail(row.email, row.displayName)
	// sub2api 供给(#109 延后):本部署 SUB2API_* 未配置,门保持 no-op,
	// 与 oauthFindOrCreate 同一决定。
	return userID, companyID, nil
}

/* ───────────── 欢迎信(尽力而为) ───────────── */

// approvedEntryUrl:仅 http(s) 入口 URL 才盖 approved=1(TS 同名函数)。
func approvedEntryUrl(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	q := u.Query()
	q.Set("approved", "1")
	u.RawQuery = q.Encode()
	return u.String()
}

func escapeHtmlAttr(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

// buildWelcomeEmailHTML 逐字镜像 admin.ts buildWelcomeEmailHtml(表格布局、
// 内联样式、Manrope 栈、object-fit 裁 hero;R2_PUBLIC_BASE 未配则省图)。
func buildWelcomeEmailHTML(firstName, signInURL string) string {
	cdn := strings.TrimRight(os.Getenv("R2_PUBLIC_BASE"), "/")
	heroURL, logoURL := "", ""
	if cdn != "" {
		heroURL = cdn + "/email/welcome-hero.png"
		logoURL = cdn + "/email/logo.png"
	}
	name := escapeHtmlAttr(firstName)
	fontStack := `'Manrope', -apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif`

	heroRow := ""
	if heroURL != "" {
		heroRow = `
        <tr>
          <td style="padding:0; line-height:0; font-size:0;">
            <img src="` + heroURL + `" alt="" width="600" height="220" style="display:block; width:100%; max-width:600px; height:220px; object-fit:cover; object-position:center 55%; border-radius:16px 16px 0 0;" />
          </td>
        </tr>`
	}
	logoRow := `
        <tr>
          <td align="center" style="padding:36px 0 24px; font-family:` + fontStack + `; font-size:18px; font-weight:700; color:#0A1B2E; letter-spacing:-0.01em;">
            Cumora
          </td>
        </tr>`
	if logoURL != "" {
		logoRow = `
        <tr>
          <td align="center" style="padding:36px 0 24px;">
            <table role="presentation" cellpadding="0" cellspacing="0" border="0">
              <tr>
                <td style="vertical-align:middle; padding-right:10px; line-height:0;">
                  <img src="` + logoURL + `" alt="" width="32" height="32" style="display:block; width:32px; height:32px;" />
                </td>
                <td style="vertical-align:middle; font-family:` + fontStack + `; font-size:18px; font-weight:700; color:#0A1B2E; letter-spacing:-0.01em;">
                  Cumora
                </td>
              </tr>
            </table>
          </td>
        </tr>`
	}
	ctaBlock := `
                  <tr>
                    <td style="padding:4px 0 0; font-family:` + fontStack + `; font-size:15px; font-weight:600; color:#4E3F8C;">
                      Open the Cumora app and sign in with the account you used to join the waitlist.
                    </td>
                  </tr>`
	if signInURL != "" {
		ctaBlock = `
                  <tr>
                    <td style="padding:8px 0 0;">
                      <table role="presentation" cellpadding="0" cellspacing="0" border="0">
                        <tr>
                          <td bgcolor="#00A8F0" style="border-radius:8px; background:#00A8F0; mso-padding-alt:14px 28px;">
                            <a href="` + signInURL + `" target="_blank" style="display:inline-block; padding:14px 28px; font-family:` + fontStack + `; font-size:14px; font-weight:600; line-height:1; color:#FFFFFF; text-decoration:none; letter-spacing:0.01em;">
                              Open Cumora &nbsp;&rarr;
                            </a>
                          </td>
                        </tr>
                      </table>
                    </td>
                  </tr>`
	}
	heroRadius := "border-radius:16px;"
	if heroURL != "" {
		heroRadius = "border-radius:0 0 16px 16px;"
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
        <table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" style="max-width:600px; width:100%;">` + logoRow + heroRow + `
          <tr>
            <td style="background:#FFFFFF; ` + heroRadius + ` padding:44px 44px 40px; box-shadow:0 1px 0 #E5ECF2;">
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

// sendWaitlistApprovedEmail:审批通过通知(尽力)。EMAIL_DOMAIN 未配 →
// skip 警告;失败只警告——库里的 approved 已成事实,最坏情况是用户下次
// 登录才知到。mock 模式(RESEND_API_KEY 空)由 SendViaProvider 自理。
func sendWaitlistApprovedEmail(to, displayName string) {
	if email.RootDomain() == "" {
		slog.Warn("[admin] skip waitlist-approved email: EMAIL_DOMAIN unset")
		return
	}
	signInRaw := os.Getenv("INVITE_BASE_URL")
	if signInRaw == "" {
		signInRaw = os.Getenv("AUTH_DONE_URL")
	}
	httpURL := approvedEntryUrl(signInRaw)
	firstName := strings.TrimSpace(strings.SplitN(displayName, " ", 2)[0])
	if firstName == "" {
		firstName = displayName
	}
	if firstName == "" {
		firstName = "there"
	}
	ctaLine := "Open the Cumora app and sign in with the same account you used to join the waitlist."
	if httpURL != "" {
		ctaLine = "Sign in here: " + httpURL
	}
	text := strings.Join([]string{
		"Hi " + firstName + ",",
		"",
		"You're in — welcome to Cumora.",
		"",
		"Your team is set up — your starter teammates (Atlas, Iris, Bram, Nova) are already gathered and ready to meet you.",
		"",
		ctaLine,
		"Use the same Google or GitHub account you used to join the waitlist.",
		"",
		"Don't have the desktop app yet? Get it at https://cumora.ai/?download=1",
		"",
		"Reply to this email if anything trips you up — a real person reads it.",
		"",
		"— Cumora",
	}, "\n")
	res := email.SendViaProvider(context.Background(), email.SendArgs{
		From:          email.FormatAddress("welcome@"+email.RootDomain(), "Cumora"),
		To:            []string{to},
		Subject:       "You're in — welcome to Cumora",
		Text:          text,
		HTML:          buildWelcomeEmailHTML(firstName, httpURL),
		AutoSubmitted: "auto-generated",
	})
	if !res.OK {
		slog.Warn("[admin] waitlist-approved email failed", "to", to, "err", res.Error)
	}
}
