// /runtime/cli 读命令组(#89):whoami / participants / conversations /
// groups / directs / members。SQL 与文本输出逐项对齐 TS cli.ts 同名
// cmd*(mirror 测试为准)。
package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

/* ============== 共享小工具 ============== */

// cliErrThrow:TS runCli catch 分支等价——handler 内抛出的错误变成
// `error: <msg>`(exit 2)。
func cliErrThrow(e error) cliResult {
	return cliErrCode("error: "+e.Error(), 2)
}

// cliJSONStringify:TS `JSON.stringify(v, null, 2)` 等价(2 空格缩进、
// 不做 HTML 转义——TS 不转义 <>&)。
func cliJSONStringify(v any) (string, error) {
	var buf bytes.Buffer
	enc := newJSONEncoderNoEscape(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	s := buf.String()
	return s[:len(s)-1], nil // Encode 自带换行;JSON.stringify 不带尾换行
}

// cliJSONList:列表序列化 —— TS 的 rows 恒为数组(空查询给 [],不会是
// null);Go nil slice 序列化成 null,在此归一。
func cliJSONList[T any](xs []T) (string, error) {
	if xs == nil {
		xs = []T{}
	}
	return cliJSONStringify(xs)
}

// newJSONEncoderNoEscape:关掉 Go 缺省的 <>& HTML 转义(TS 不转义)。
func newJSONEncoderNoEscape(w *bytes.Buffer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc
}

// cliISOTime:node-postgres 把 timestamptz 解析成 JS Date,JSON.stringify
// 输出毫秒精度 ISO(始终 3 位小数 + Z/偏移)。--json 输出对齐到这个
// 形状。
type cliISOTime time.Time

func (t *cliISOTime) Scan(src any) error {
	if src == nil {
		return fmt.Errorf("cliISOTime: nil scan")
	}
	switch v := src.(type) {
	case time.Time:
		*t = cliISOTime(v)
		return nil
	default:
		return fmt.Errorf("cliISOTime: unsupported scan %T", src)
	}
}

func (t cliISOTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).UTC().Format("2006-01-02T15:04:05.000Z07:00") + `"`), nil
}

// —— UTF-16 码元口径的 padEnd/slice/len:JS 字符串长度按 UTF-16 码元
// 计(BMP 外的字符算 2)。id/kind 等枚举字段是 ASCII,正文标题可能携带
// CJK 或 emoji,mirror 输出的列宽对齐以 JS 口径为准 ———

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func utf16PadEnd(s string, n int) string {
	d := n - utf16Len(s)
	if d <= 0 {
		return s
	}
	return s + string(bytes.Repeat([]byte{' '}, d))
}

func utf16Slice(s string, n int) string {
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

// cliStrArr:jsonb 字符串数组列(members / tools)。NULL → nil(JSON 序列化
// 成 null,对齐 node-postgres 把 NULL 解析成 null)。
type cliStrArr []string

func (a *cliStrArr) Scan(src any) error {
	switch t := src.(type) {
	case nil:
		*a = nil
		return nil
	case []byte:
		return json.Unmarshal(t, a)
	case string:
		return json.Unmarshal([]byte(t), a)
	default:
		return fmt.Errorf("cliStrArr: unsupported %T", src)
	}
}

// cliAgentCompany:agent 全局唯一(kind='agent' 的部分唯一索引 + 服务端
// slug 化),id 直查返回唯一行的 tenant;用于一切按 agent 落库的操作。
func (s *Service) cliAgentCompany(ctx context.Context, agentID string) (string, error) {
	var companyID sql.NullString
	if err := s.DB.QueryRowContext(ctx,
		`SELECT company_id FROM participants WHERE id = $1 LIMIT 1`, agentID,
	).Scan(&companyID); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return companyID.String, nil
}

/* ============== whoami ============== */

func (s *Service) cliCmdWhoami(ctx context.Context, parsed cliParsed) cliResult {
	id, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	var p struct {
		ID     string    `json:"id"`
		Kind   string    `json:"kind"`
		Name   string    `json:"name"`
		Role   *string   `json:"role"`
		Status string    `json:"status"`
		Bio    *string   `json:"bio"`
		Tools  cliStrArr `json:"tools"`
	}
	err = s.DB.QueryRowContext(ctx,
		`SELECT id, kind, name, role, status, bio, tools FROM participants WHERE id = $1`, id,
	).Scan(&p.ID, &p.Kind, &p.Name, &p.Role, &p.Status, &p.Bio, &p.Tools)
	if err == sql.ErrNoRows {
		return cliErr("unknown participant: " + id)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	if parsed.flagTruey("json") {
		js, e := cliJSONStringify(p)
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT c.id, c.title, c.kind FROM conversations c
		  WHERE EXISTS (SELECT 1 FROM conversation_members cm WHERE cm.conversation_id = c.id AND cm.participant_id = $1)
		  ORDER BY c.updated_at DESC`, id)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type convo struct {
		id    string
		title string
		kind  string
	}
	var convos []convo
	for rows.Next() {
		var c convo
		if err := rows.Scan(&c.id, &c.title, &c.kind); err != nil {
			return cliErrThrow(err)
		}
		convos = append(convos, c)
	}
	if err := rows.Err(); err != nil {
		return cliErrThrow(err)
	}
	lines := []string{
		"id:        " + p.ID,
		"name:      " + p.Name,
		"kind:      " + p.Kind,
	}
	if p.Role != nil {
		lines = append(lines, "role:      "+*p.Role)
	}
	lines = append(lines, "status:    "+p.Status)
	if p.Bio != nil {
		lines = append(lines, "bio:       "+*p.Bio)
	}
	if len(p.Tools) > 0 {
		lines = append(lines, "tools:     "+strings.Join(p.Tools, ", "))
	}
	lines = append(lines, fmt.Sprintf("member of %d conversation(s):", len(convos)))
	for _, c := range convos {
		lines = append(lines, "  · ["+utf16PadEnd(c.kind, 7)+"] "+utf16PadEnd(c.id, 28)+" "+c.title)
	}
	return cliOK(strings.Join(lines, "\n"))
}

/* ============== participants ============== */

func (s *Service) cliCmdParticipants(ctx context.Context, parsed cliParsed) cliResult {
	// 租户隔离:只列本 agent 自己公司的成员;少了 company_id 过滤会把
	// 所有 Cumora 公司的成员全部列出(跨租户泄漏)。
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	companyID, err := s.cliAgentCompany(ctx, me)
	if err != nil {
		return cliErrThrow(err)
	}
	if companyID == "" {
		return cliErr("cannot resolve company for " + me)
	}
	args := []any{companyID}
	where := `WHERE company_id = $1 AND departed_at IS NULL`
	if v, ok := parsed.flags["kind"]; ok {
		// TS `parsed.flags.kind ? String(...) : null` — bool true 也会被
		// String() 成 "true"(查询自然无结果,行为照搬)。
		args = append(args, fmt.Sprint(v))
		where += ` AND kind = $2`
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, kind, name, role, status, avatar_url FROM participants `+where+` ORDER BY kind DESC, name ASC`,
		args...)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type row struct {
		ID     string  `json:"id"`
		Kind   string  `json:"kind"`
		Name   string  `json:"name"`
		Role   *string `json:"role"`
		Status string  `json:"status"`
		Avatar *string `json:"avatar_url"`
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Kind, &r.Name, &r.Role, &r.Status, &r.Avatar); err != nil {
			return cliErrThrow(err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return cliErrThrow(err)
	}
	if parsed.flagTruey("json") {
		js, e := cliJSONList(all)
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	lines := []string{
		"id              kind   status      role",
		"-----------------------------------------------------",
	}
	for _, r := range all {
		line := utf16PadEnd(r.ID, 15) + " " + utf16PadEnd(r.Kind, 6) + " " + utf16PadEnd(r.Status, 11) + " "
		if r.Role != nil {
			line += *r.Role
		}
		lines = append(lines, line)
		// 头像另起一行,agent 才能取回并查看(`cumora avatar show` 的便利
		// 包装)。
		if r.Avatar != nil {
			lines = append(lines, "  ↳ avatar: "+*r.Avatar)
		}
	}
	return cliOK(strings.Join(lines, "\n"))
}

/* ============== conversations / groups / directs ============== */

type cliConvoRow struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind"`
	Title     string           `json:"title"`
	Subtitle  *string          `json:"subtitle"`
	Members   cliStrArr        `json:"members"`
	Tag       *string          `json:"tag"`
	UpdatedAt cliISOTime       `json:"updated_at"`
	PulledBy  *cliPulledByJSON `json:"pulled_by"`
}

type cliPulledByJSON struct {
	AgentID *string `json:"agentId,omitempty"`
}

func (p *cliPulledByJSON) Scan(src any) error {
	return scanJSONB(src, p)
}

func (s *Service) cliCmdConversations(ctx context.Context, parsed cliParsed, kindFilter string) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	args := []any{me}
	kindWhere := ""
	if kindFilter != "" {
		args = append(args, kindFilter)
		kindWhere = `AND kind = $2`
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT c.id, c.kind, c.title, c.subtitle, c.members, c.tag, c.updated_at, c.pulled_by
		   FROM conversations c
		  WHERE EXISTS (SELECT 1 FROM conversation_members cm WHERE cm.conversation_id = c.id AND cm.participant_id = $1) `+kindWhere+`
		  ORDER BY c.updated_at DESC`, args...)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	var all []cliConvoRow
	for rows.Next() {
		var r cliConvoRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.Title, &r.Subtitle, &r.Members, &r.Tag, &r.UpdatedAt, &r.PulledBy); err != nil {
			return cliErrThrow(err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return cliErrThrow(err)
	}
	if parsed.flagTruey("json") {
		js, e := cliJSONList(all)
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	if len(all) == 0 {
		return cliOK("(no conversations for " + me + ")")
	}
	lines := []string{
		fmt.Sprintf("%s is in %d conversation(s):", me, len(all)),
		``,
		"id                          kind     title                                       members",
		"------------------------------------------------------------------------------------------------",
	}
	for _, r := range all {
		tag := ""
		if r.Tag != nil {
			tag = " [" + *r.Tag + "]"
		}
		pulled := ""
		if r.PulledBy != nil && r.PulledBy.AgentID != nil {
			pulled = " ← pulled by " + *r.PulledBy.AgentID
		}
		lines = append(lines,
			utf16PadEnd(r.ID, 28)+" "+utf16PadEnd(r.Kind, 8)+" "+
				utf16PadEnd(utf16Slice(r.Title, 42), 42)+" "+
				strings.Join(r.Members, ",")+tag+pulled)
	}
	return cliOK(strings.Join(lines, "\n"))
}

/* ============== members ============== */

func (s *Service) cliCmdMembers(ctx context.Context, parsed cliParsed) cliResult {
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr("usage: members <conversation_id>")
	}
	id := parsed.positional[0]
	var memberIDs cliStrArr
	if err := s.DB.QueryRowContext(ctx,
		`SELECT members FROM conversations WHERE id = $1`, id,
	).Scan(&memberIDs); err == sql.ErrNoRows {
		return cliErr("unknown conversation: " + id)
	} else if err != nil {
		return cliErrThrow(err)
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, kind, role, status, avatar_url FROM participants WHERE id = ANY($1::text[])`,
		memberIDs)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type row struct {
		ID     string  `json:"id"`
		Name   string  `json:"name"`
		Kind   string  `json:"kind"`
		Role   *string `json:"role"`
		Status string  `json:"status"`
		Avatar *string `json:"avatar_url"`
	}
	var peeps []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &r.Role, &r.Status, &r.Avatar); err != nil {
			return cliErrThrow(err)
		}
		peeps = append(peeps, r)
	}
	if err := rows.Err(); err != nil {
		return cliErrThrow(err)
	}
	if parsed.flagTruey("json") {
		js, e := cliJSONList(peeps)
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	var memberLines []string
	for _, p := range peeps {
		line := "  · " + utf16PadEnd(p.ID, 12) + " " + utf16PadEnd(p.Kind, 6) + " " + utf16PadEnd(p.Status, 10) + " " + p.Name
		if p.Role != nil {
			line += " (" + *p.Role + ")"
		}
		memberLines = append(memberLines, line)
		if p.Avatar != nil {
			memberLines = append(memberLines, "      ↳ avatar: "+*p.Avatar)
		}
	}
	lines := append([]string{
		fmt.Sprintf("%s has %d member(s):", id, len(peeps)),
		``,
	}, memberLines...)
	return cliOK(strings.Join(lines, "\n"))
}
