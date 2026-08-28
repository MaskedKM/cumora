// invite_email —— 邀请邮件(#68 评审 F11 接线):POST /companies/:id/
// invitations 的 sendEmail 分支。对齐 server/src/invitation-email.ts:
// EMAIL_DOMAIN 未配 → skipped:'no_email_config';已配 → 经 email 域
// provider 发送(mock 或 Resend),失败只回报 emailDelivery 不炸创建。
package invitations

import (
	"context"
	"database/sql"
	"os"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/email"
)

type invitationEmailDelivery struct {
	Attempted bool `json:"attempted"`
	OK        bool `json:"ok"`
	Error     any  `json:"error"`
	Skipped   any  `json:"skipped"`
}

type invitationEmailArgs struct {
	To           string
	InviterName  string
	InviterEmail string
	CompanyName  string
	Role         string // 'member' | 'admin'
	Note         any    // nil 或 string
	InviteURL    string
}

func escapeHTML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;",
	)
	return r.Replace(s)
}

// buildInvitationEmailHTML 逐字镜像 invitation-email.ts 的模板(表格布局、
// 内联样式、Manrope 栈、天蓝 CTA)——收件人后续收到 waitlist 欢迎信时
// 视觉语言一致。
func buildInvitationEmailHTML(a invitationEmailArgs) string {
	cdn := strings.TrimRight(os.Getenv("R2_PUBLIC_BASE"), "/")
	logoURL := ""
	if cdn != "" {
		logoURL = cdn + "/email/logo.png"
	}
	fontStack := `'Manrope', -apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif`
	inviter := escapeHTML(a.InviterName)
	company := escapeHTML(a.CompanyName)
	roleLabel := "a member"
	if a.Role == "admin" {
		roleLabel = "an admin"
	}
	noteBlock := ""
	if s, ok := a.Note.(string); ok && s != "" {
		noteBlock = `
                <tr>
                  <td style="padding:0 0 24px;">
                    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"
                           style="background:#F1F6FB; border-left:3px solid #00A8F0; border-radius:6px;">
                      <tr>
                        <td style="padding:14px 16px; font-family:` + fontStack + `; font-size:14px; font-weight:400; line-height:1.5; color:#233A53; font-style:italic;">
                          &ldquo;` + escapeHTML(s) + `&rdquo;
                        </td>
                      </tr>
                    </table>
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
	return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <meta name="color-scheme" content="light" />
  <meta name="supported-color-schemes" content="light" />
  <title>You're invited to ` + company + ` on Cumora</title>
</head>
<body style="margin:0; padding:0; background:#FAFCFE; color:#0A1B2E; font-family:` + fontStack + `;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#FAFCFE;">
    <tr>
      <td align="center" style="padding:24px 16px 48px;">
        <table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" style="max-width:600px; width:100%;">` + logoRow + `
          <tr>
            <td style="background:#FFFFFF; border-radius:16px; padding:40px 44px 36px; box-shadow:0 1px 0 #E5ECF2;">
              <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
                <tr>
                  <td style="font-family:` + fontStack + `; font-size:13px; font-weight:600; letter-spacing:0.04em; text-transform:uppercase; color:#5B7186; padding:0 0 8px;">
                    Team invitation
                  </td>
                </tr>
                <tr>
                  <td style="font-family:` + fontStack + `; font-size:28px; font-weight:800; line-height:1.2; color:#0A1B2E; letter-spacing:-0.02em; padding:0 0 14px;">
                    ` + inviter + ` invited you to <span style="color:#00A8F0;">` + company + `</span>
                  </td>
                </tr>
                <tr>
                  <td style="font-family:` + fontStack + `; font-size:15px; font-weight:400; line-height:1.6; color:#233A53; padding:0 0 24px;">
                    You&rsquo;ll join as ` + roleLabel + `. Cumora is a desktop chat where humans and AI teammates share the same rooms &mdash; once you accept, you&rsquo;ll see your new team and the agents that live there.
                  </td>
                </tr>` + noteBlock + `
                <tr>
                  <td style="padding:4px 0 0;">
                    <table role="presentation" cellpadding="0" cellspacing="0" border="0">
                      <tr>
                        <td bgcolor="#00A8F0" style="border-radius:8px; background:#00A8F0; mso-padding-alt:14px 28px;">
                          <a href="` + a.InviteURL + `" target="_blank" style="display:inline-block; padding:14px 28px; font-family:` + fontStack + `; font-size:14px; font-weight:600; line-height:1; color:#FFFFFF; text-decoration:none; letter-spacing:0.01em;">
                            Accept invitation &nbsp;&rarr;
                          </a>
                        </td>
                      </tr>
                    </table>
                  </td>
                </tr>
                <tr>
                  <td style="font-family:` + fontStack + `; font-size:12.5px; font-weight:400; line-height:1.55; color:#94A8BC; padding:18px 0 0; word-break:break-all;">
                    Or open this link: <a href="` + a.InviteURL + `" style="color:#3E6FA8; text-decoration:none;">` + a.InviteURL + `</a>
                  </td>
                </tr>
                <tr>
                  <td style="font-family:` + fontStack + `; font-size:12.5px; font-weight:400; line-height:1.55; color:#94A8BC; padding:10px 0 0;">
                    Don&rsquo;t have the desktop app yet?
                    <a href="https://cumora.ai/?download=1#download" style="color:#3E6FA8; text-decoration:none; font-weight:600;">Get Cumora for macOS, Windows, or Linux</a>.
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
                  <td style="font-family:` + fontStack + `; font-size:12.5px; font-weight:400; line-height:1.55; color:#5B7186; padding:18px 0 0;">
                    This invitation expires in 7 days. If you weren&rsquo;t expecting it, you can ignore this email &mdash; nothing happens until you click the link.
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

func buildInvitationEmailText(a invitationEmailArgs) string {
	lines := []string{
		`Hi,`,
		``,
		a.InviterName + ` invited you to ` + a.CompanyName + ` on Cumora — you'll join as ` + roleWord(a.Role) + `.`,
		``,
	}
	if s, ok := a.Note.(string); ok && s != "" {
		lines = append(lines, `Note from `+a.InviterName+`: "`+s+`"`, ``)
	}
	lines = append(lines,
		`Accept here: `+a.InviteURL,
		``,
		`Cumora is a desktop chat for humans and AI teammates. Once you accept, you'll see your new team and the agents that live there.`,
		``,
		`Don't have the desktop app yet? Get it at https://cumora.ai/?download=1#download`,
		``,
		`This invitation expires in 7 days. If you weren't expecting it, just ignore this email — nothing happens until you click the link.`,
		``,
		`— Cumora`,
	)
	return strings.Join(lines, "\n")
}

func roleWord(role string) string {
	if role == "admin" {
		return "an admin"
	}
	return "a member"
}

func sendInvitationEmail(ctx context.Context, db *sql.DB, a invitationEmailArgs) invitationEmailDelivery {
	domain := os.Getenv("EMAIL_DOMAIN")
	if domain == "" {
		return invitationEmailDelivery{Attempted: false, OK: false, Error: nil, Skipped: "no_email_config"}
	}
	fromAddr := "invites@" + domain
	senderDisplay := a.InviterName + " (via Cumora)"
	subject := a.InviterName + ` invited you to ` + a.CompanyName + ` on Cumora`
	res := email.SendViaProvider(ctx, email.SendArgs{
		From:          email.FormatAddress(fromAddr, senderDisplay),
		To:            []string{a.To},
		ReplyTo:       email.FormatAddress(a.InviterEmail, a.InviterName),
		Subject:       subject,
		Text:          buildInvitationEmailText(a),
		HTML:          buildInvitationEmailHTML(a),
		MessageID:     email.MintMessageId(),
		AutoSubmitted: "auto-generated",
	})
	if !res.OK {
		errMsg := any("send failed")
		if res.Error != "" {
			errMsg = res.Error
		}
		return invitationEmailDelivery{Attempted: true, OK: false, Error: errMsg, Skipped: nil}
	}
	return invitationEmailDelivery{Attempted: true, OK: true, Error: nil, Skipped: nil}
}
