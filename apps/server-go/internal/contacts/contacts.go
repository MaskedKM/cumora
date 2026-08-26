// contacts —— 联系人查找核心(#57):`cumora contacts` CLI 背后的三支
// 查询(同租户 agent / 人类成员 / 外部 email_contacts)+ 统一模糊过滤。
// 对齐 server/src/agents/cli.ts 的 listEmailContacts。CLI 表面在 daemon
// 波(#63+)挂载;本包即其可测核心。
package contacts

import (
	"context"
	"database/sql"
	"os"
	"strings"
)

// Contact 对齐 EmailContact 形状。
type Contact struct {
	ParticipantID any    `json:"participantId"`
	Name          string `json:"name"`
	Address       string `json:"address"`
	Kind          string `json:"kind"`
	Role          any    `json:"role"`
}

// rootDomain 对齐 env.ts:EMAIL_DOMAIN 小写化、去首尾点;空 = 未配置。
func rootDomain() string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(os.Getenv("EMAIL_DOMAIN")), "."))
}

// safeLocalPart:id 小写,非 [a-z0-9_-] 转 '-',去首尾 -_,截 64。
func safeLocalPart(id string) string {
	s := strings.ToLower(id)
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b = append(b, c)
		} else {
			b = append(b, '-')
		}
	}
	out := strings.Trim(string(b), "-_")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// safeSlugPart:slug 小写,非 [a-z0-9-] 转 '-',去首尾 -,截 63。
func safeSlugPart(slug string) string {
	s := strings.ToLower(slug)
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b = append(b, c)
		} else {
			b = append(b, '-')
		}
	}
	out := strings.Trim(string(b), "-")
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

// computeAgentAddress 对齐 email.ts 的确定性 agent 地址
// <safeLocalPart(id)>.<safeSlugPart(slug)>@<EMAIL_DOMAIN>;任一段不可得
// 返回 ""(调用方回退 "(chat only)")。
func computeAgentAddress(agentID, companySlug string) string {
	dom := rootDomain()
	local := safeLocalPart(agentID)
	slug := safeSlugPart(companySlug)
	if dom == "" || local == "" || slug == "" {
		return ""
	}
	return local + "." + slug + "@" + dom
}

func matches(q string, c Contact) bool {
	if q == "" {
		return true
	}
	lower := func(s string) string { return strings.ToLower(s) }
	if strings.Contains(lower(c.Name), q) || strings.Contains(lower(c.Address), q) {
		return true
	}
	if pid, ok := c.ParticipantID.(string); ok && strings.Contains(lower(pid), q) {
		return true
	}
	if role, ok := c.Role.(string); ok && strings.Contains(lower(role), q) {
		return true
	}
	return false
}

// List 对齐 listEmailContacts:①同租户 agent(排除 viewer;email 未铸的
// 用确定性地址,「(chat only)」兜底);②人类成员(auth email);③外部
// 往来地址(无过滤 30 条/有过滤 200 条)。过滤在 Go 侧统一执行。
func List(ctx context.Context, db *sql.DB, companyID, viewerID, query string) ([]Contact, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	out := []Contact{}

	agentRows, err := db.QueryContext(ctx, `
		SELECT p.id, p.name, p.email, p.role, c.slug
		  FROM participants p
		  JOIN companies c ON c.id = p.company_id
		 WHERE p.company_id = $1 AND p.kind = 'agent' AND p.departed_at IS NULL
		   AND p.id <> $2
		 ORDER BY p.name ASC`, companyID, viewerID)
	if err != nil {
		return nil, err
	}
	defer agentRows.Close()
	for agentRows.Next() {
		var id, name string
		var email, role sql.NullString
		var slug string
		if agentRows.Scan(&id, &name, &email, &role, &slug) != nil {
			continue
		}
		address := ""
		if email.Valid {
			address = email.String
		} else {
			address = computeAgentAddress(id, slug)
			if address == "" {
				address = "(chat only)"
			}
		}
		c := Contact{ParticipantID: id, Name: name, Address: address, Kind: "agent"}
		if role.Valid {
			c.Role = role.String
		}
		if matches(q, c) {
			out = append(out, c)
		}
	}
	agentRows.Close()

	humanRows, err := db.QueryContext(ctx, `
		SELECT u.id, u.display_name, u.email
		  FROM users u
		  JOIN company_members cm ON cm.user_id = u.id
		 WHERE cm.company_id = $1 AND u.email IS NOT NULL
		 ORDER BY u.display_name ASC`, companyID)
	if err != nil {
		return nil, err
	}
	defer humanRows.Close()
	for humanRows.Next() {
		var id, name, email string
		if humanRows.Scan(&id, &name, &email) != nil {
			continue
		}
		c := Contact{ParticipantID: id, Name: name, Address: email, Kind: "human"}
		if matches(q, c) {
			out = append(out, c)
		}
	}
	humanRows.Close()

	limit := 30
	if q != "" {
		limit = 200
	}
	extRows, err := db.QueryContext(ctx, `
		SELECT address, display_name FROM email_contacts
		 WHERE company_id = $1
		 ORDER BY last_seen_at DESC LIMIT $2`, companyID, limit)
	if err != nil {
		return nil, err
	}
	defer extRows.Close()
	for extRows.Next() {
		var address string
		var displayName sql.NullString
		if extRows.Scan(&address, &displayName) != nil {
			continue
		}
		name := address
		if displayName.Valid {
			name = displayName.String
		}
		c := Contact{ParticipantID: nil, Name: name, Address: address, Kind: "external"}
		if matches(q, c) {
			out = append(out, c)
		}
	}
	return out, nil
}
