// /runtime/cli 读命令组(#89):whoami / participants / conversations /
// groups / directs / members / messages / thread / convening / search /
// tools-log / participants-status。SQL 与文本输出逐项对齐 TS cli.ts 同名
// cmd*(mirror 测试为准);时间渲染镜像 Node 无 LANG 环境的 en-US 缺省
// (CI 容器行为;生产容器同环境,双跑等价以它为准)。
package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
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

/* ───────── Node 时间格式镜像 ───────── */

// nodeLocaleDate:en-US toLocaleDateString = "8/27/2026"。
func nodeLocaleDate(t time.Time) string {
	return fmt.Sprintf("%d/%d/%d", t.Month(), t.Day(), t.Year())
}

// nodeHM:`new Date(x).toLocaleTimeString([], {hour:'2-digit', minute:'2-digit'})`
// 在 en-US 下 = 12 小时制、两位补零 + AM/PM("09:04 AM")。
func nodeHM(t time.Time) string {
	h := t.Hour()
	ampm := "AM"
	switch {
	case h == 0:
		h = 12
	case h == 12:
		ampm = "PM"
	case h > 12:
		h -= 12
		ampm = "PM"
	}
	return fmt.Sprintf("%02d:%02d %s", h, t.Minute(), ampm)
}

// nodeLocaleString:`new Date(x).toLocaleString()` 在 en-US 下 =
// "8/27/2026, 9:04:09 AM"(M/D 不补零;12 小时制小时不补零)。
func nodeLocaleString(t time.Time) string {
	h := t.Hour()
	ampm := "AM"
	switch {
	case h == 0:
		h = 12
	case h == 12:
		ampm = "PM"
	case h > 12:
		h -= 12
		ampm = "PM"
	}
	return fmt.Sprintf("%d/%d/%d, %d:%02d:%02d %s",
		t.Month(), t.Day(), t.Year(), h, t.Minute(), t.Second(), ampm)
}

// nodeDateToString:`String(date)` = "Thu Aug 27 2026 09:04:09 GMT+0000
// (Coordinated Universal Time)"。UTC 的长名 Node 写全称,Go 只给 "UTC"
// —— 特判补齐;其它时区用 Go 的短名(生产与 CI 均跑 UTC,足够)。
func nodeDateToString(t time.Time) string {
	zone := t.Format("MST")
	switch zone {
	case "UTC":
		zone = "Coordinated Universal Time"
	}
	return t.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (" + zone + ")"
}

/* ───────── 附件 / 投票渲染(供 messages·inbox·glance 复用)───────── */

// cliRawJSON:jsonb 列的原始字节。PG 的 jsonb 按键长+字典序排键,
// node-pg 解析后 JS 对象保持这个顺序——--json 输出想逐字节等价就必须
// 原样透传,不能经 Go 的 struct/map(会按定义序/字母序重排)。
type cliRawJSON []byte

func (r *cliRawJSON) Scan(src any) error {
	switch t := src.(type) {
	case nil:
		*r = nil
		return nil
	case []byte:
		*r = t
		return nil
	case string:
		*r = []byte(t)
		return nil
	default:
		return fmt.Errorf("cliRawJSON: unsupported %T", src)
	}
}

func (r cliRawJSON) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	return r, nil
}

// cliAttachment:存储层原样的附件 jsonb(raw 透传 + 解析出的渲染字段)。
// R2 模式下 TS 会 freshen 重签名 URL;Go 侧存储抽象落地前按本地模式语义
// 原样返回(镜像测试即本地模式,等价)。
type cliAttachment struct {
	raw cliRawJSON
	m   map[string]any
}

func (a *cliAttachment) Scan(src any) error {
	if err := a.raw.Scan(src); err != nil {
		return err
	}
	if a.raw == nil || string(a.raw) == "null" {
		return nil
	}
	return jsonUnmarshal(a.raw, &a.m)
}

func (a cliAttachment) MarshalJSON() ([]byte, error) { return a.raw.MarshalJSON() }

// renderAttachment:TS cli.ts 的一行摘要(kind/name/size/url)。
func renderAttachment(att cliAttachment) string {
	if att.m == nil {
		return ""
	}
	size := ""
	if v, ok := att.m["size"].(float64); ok {
		size = fmt.Sprintf(" %dB", int64(v))
	}
	name, _ := att.m["name"].(string)
	kind, _ := att.m["kind"].(string)
	url, _ := att.m["url"].(string)
	return "    ↳ [" + kind + "] " + name + size + " → " + url
}

// cliPoll:消息投票载荷(jsonb,camelCase 键;raw 透传,字段供渲染)。
type cliPoll struct {
	raw     cliRawJSON
	parsed  pollPayload
	present bool
}

type pollPayload struct {
	Question     string          `json:"question"`
	Mode         string          `json:"mode"`
	Options      []cliPollOption `json:"options"`
	ExpiresAt    *string         `json:"expiresAt"`
	ClosedAt     *string         `json:"closedAt"`
	ClosedReason *string         `json:"closedReason"`
}

type cliPollOption struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

func (p *cliPoll) Scan(src any) error {
	if err := p.raw.Scan(src); err != nil {
		return err
	}
	if p.raw == nil || string(p.raw) == "null" {
		return nil
	}
	if err := jsonUnmarshal(p.raw, &p.parsed); err != nil {
		return err
	}
	p.present = true
	return nil
}

func (p cliPoll) MarshalJSON() ([]byte, error) { return p.raw.MarshalJSON() }

// renderPollBlock:多行投票块——问题、选项 id(免二次查库就能投)、模式、
// 开合状态、可复制的投票命令行。
func renderPollBlock(messageID string, poll pollPayload) []string {
	var lines []string
	state := "open · no expiration"
	if poll.ClosedAt != nil {
		state = "closed"
		if poll.ClosedReason != nil && *poll.ClosedReason != "" {
			state = "closed (" + *poll.ClosedReason + ")"
		}
	} else if poll.ExpiresAt != nil {
		state = "open · expires " + *poll.ExpiresAt
	}
	lines = append(lines, "    📊 POLL · "+poll.Mode+"-choice · "+state)
	lines = append(lines, "    question: "+poll.Question)
	for _, o := range poll.Options {
		lines = append(lines, "      • "+o.ID+" — "+o.Text)
	}
	if poll.ClosedAt == nil {
		multi := ""
		if poll.Mode == "multi" {
			multi = "[,<option_id>...]"
		}
		lines = append(lines, "    → to vote: cumora poll vote "+messageID+" <option_id>"+multi)
		lines = append(lines, "    → if the question doesn't apply to you or none of the options is your real answer, stay silent (no reply, no vote)")
	}
	return lines
}

// cliQuoted:cmdMessages 的 quoted 投影(authorName 即 author_id,TS 如此;
// raw 透传保证 jsonb 键序)。
type cliQuoted struct {
	raw    cliRawJSON
	parsed quotedPayload
}

type quotedPayload struct {
	ID         string `json:"id"`
	AuthorID   string `json:"authorId"`
	AuthorName string `json:"authorName"`
	Body       string `json:"body"`
}

func (q *cliQuoted) Scan(src any) error {
	if err := q.raw.Scan(src); err != nil {
		return err
	}
	if q.raw == nil || string(q.raw) == "null" {
		return nil
	}
	return jsonUnmarshal(q.raw, &q.parsed)
}

func (q cliQuoted) MarshalJSON() ([]byte, error) { return q.raw.MarshalJSON() }

/* ───────── messages / thread ───────── */

// cliMsgFlagNum:TS `Number(flags.x ?? def)` + clamp(1..max)。不可解析值
// 在 TS 会变 NaN 并让 SQL 报错走 error: 路径;Go 直接给同级的 error: 出口。
func cliMsgFlagNum(p cliParsed, key string, def, max int) (int, error) {
	v, ok := p.flags[key]
	if !ok {
		return def, nil
	}
	switch t := v.(type) {
	case string:
		n, err := strconv.Atoi(t)
		if err != nil {
			return 0, fmt.Errorf("invalid integer %q for --%s", t, key)
		}
		return clampInt(n, 1, max), nil
	case bool:
		n := 0
		if t {
			n = 1
		}
		return clampInt(n, 1, max), nil
	default:
		return def, nil
	}
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func (s *Service) cliCmdMessages(ctx context.Context, parsed cliParsed) cliResult {
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr("usage: messages <conversation_id> [--tail N] [--thread <root_id>]")
	}
	id := parsed.positional[0]
	tail, err := cliMsgFlagNum(parsed, "tail", 20, 200)
	if err != nil {
		return cliErrThrow(err)
	}
	threadRootID := ""
	if v, ok := parsed.flags["thread"]; ok {
		threadRootID = fmt.Sprint(v)
	}
	args := []any{id}
	whereExtra := ""
	if threadRootID != "" {
		args = append(args, threadRootID)
		whereExtra = `AND quoted_message_id = $2`
	}
	args = append(args, tail)
	limitParam := fmt.Sprintf("$%d", len(args))
	rows, err := s.DB.QueryContext(ctx, `SELECT
        id, author_id, kind, body, sequence, created_at, attachment, poll,
        quoted_message_id,
        (
          SELECT jsonb_build_object(
            'id', qm.id,
            'authorId', qm.author_id,
            'authorName', qm.author_id,
            'body', LEFT(qm.body, 240)
          )
            FROM messages qm
           WHERE qm.id = messages.quoted_message_id
             AND qm.conversation_id = messages.conversation_id
        ) AS quoted
       FROM messages WHERE conversation_id = $1
       `+whereExtra+`
       ORDER BY sequence DESC LIMIT `+limitParam, args...)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type msgRow struct {
		ID              string        `json:"id"`
		AuthorID        string        `json:"author_id"`
		Kind            string        `json:"kind"`
		Body            string        `json:"body"`
		Sequence        int64         `json:"sequence"`
		CreatedAt       cliISOTime    `json:"created_at"`
		Attachment      cliAttachment `json:"attachment"`
		Poll            cliPoll       `json:"poll"`
		QuotedMessageID *string       `json:"quoted_message_id"`
		Quoted          *cliQuoted    `json:"quoted"`
	}
	var listed []msgRow
	for rows.Next() {
		var m msgRow
		if err := rows.Scan(&m.ID, &m.AuthorID, &m.Kind, &m.Body, &m.Sequence, &m.CreatedAt, &m.Attachment, &m.Poll, &m.QuotedMessageID, &m.Quoted); err != nil {
			return cliErrThrow(err)
		}
		listed = append(listed, m)
	}
	if err := rows.Err(); err != nil {
		return cliErrThrow(err)
	}
	// 倒序取回 → 正序展示
	inOrder := make([]msgRow, len(listed))
	for i, m := range listed {
		inOrder[len(listed)-1-i] = m
	}
	// 推进 Redis seen 边界:messages 刚把这些行展示给 agent,最高 seq 即
	// "已看到"。没有它,`messages → reply` 的典型流会在自己刚读过的尾巴
	// 上 HOLD。Redis-only、失败开放、单调。
	if len(inOrder) > 0 {
		me, rerr := cliResolveAs(parsed)
		if rerr != nil {
			return cliErrThrow(rerr)
		}
		s.RecordSeen(me, id, inOrder[len(inOrder)-1].Sequence)
	}
	if parsed.flagTruey("json") {
		js, e := cliJSONList(inOrder)
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	if len(inOrder) == 0 {
		if threadRootID != "" {
			return cliOK("(no replies in thread " + threadRootID + ")")
		}
		return cliOK("(no messages in " + id + ")")
	}
	header := fmt.Sprintf("last %d message(s) in %s:", len(inOrder), id)
	if threadRootID != "" {
		header = fmt.Sprintf("%d reply(ies) in thread %s:", len(inOrder), threadRootID)
	}
	lines := []string{header, ""}
	for _, m := range inOrder {
		t := nodeHM(time.Time(m.CreatedAt))
		body := strings.ReplaceAll(utf16Slice(m.Body, 280), "\n", " \\n ")
		if m.Kind == "tool" {
			body = "[tool call]"
		}
		lines = append(lines, "  ["+m.ID+"] ["+t+"] "+utf16PadEnd(m.AuthorID, 8)+
			" #"+padStartUTF16(strconv.FormatInt(m.Sequence, 10), 3, ' ')+
			"  "+body)
		if m.QuotedMessageID != nil {
			if m.Quoted != nil && m.Quoted.parsed.ID != "" {
				qBody := strings.ReplaceAll(utf16Slice(m.Quoted.parsed.Body, 180), "\n", " \\n ")
				lines = append(lines, "    ↩ quoting ["+m.Quoted.parsed.ID+"] "+m.Quoted.parsed.AuthorName+": "+qBody)
			} else {
				lines = append(lines, "    ↩ quoting ["+*m.QuotedMessageID+"] (original deleted)")
			}
		}
		if m.Kind == "poll" && m.Poll.present {
			lines = append(lines, renderPollBlock(m.ID, m.Poll.parsed)...)
		}
		if att := renderAttachment(m.Attachment); att != "" {
			lines = append(lines, att)
		}
	}
	return cliOK(strings.Join(lines, "\n"))
}

func padStartUTF16(s string, n int, ch byte) string {
	d := n - utf16Len(s)
	if d <= 0 {
		return s
	}
	return strings.Repeat(string(ch), d) + s
}

// cmdThread:messages --thread <root> 的糖衣。
func (s *Service) cliCmdThread(ctx context.Context, parsed cliParsed) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[0] == "" || parsed.positional[1] == "" {
		return cliErr("usage: thread <conversation_id> <root_msg_id> [--tail N]")
	}
	proxied := cliParsed{
		positional: []string{parsed.positional[0]},
		flags:      map[string]any{},
	}
	for k, v := range parsed.flags {
		proxied.flags[k] = v
	}
	proxied.flags["thread"] = parsed.positional[1]
	return s.cliCmdMessages(ctx, proxied)
}

/* ───────── convening ───────── */

func (s *Service) cliCmdConvening(ctx context.Context, parsed cliParsed) cliResult {
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr("usage: convening <conversation_id>")
	}
	id := parsed.positional[0]
	var (
		pulledByID, headlineLead, headlineTail, subhead, status string
		pulledAt                                                cliISOTime
		whoAndWhy, reasoning                                    []any
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT pulled_by_id, pulled_at, headline_lead, headline_tail, subhead,
		        who_and_why, reasoning, status
		   FROM convening_info WHERE conversation_id = $1`, id,
	).Scan(&pulledByID, &pulledAt, &headlineLead, &headlineTail, &subhead, jsonArrayScan{&whoAndWhy}, jsonArrayScan{&reasoning}, &status)
	if err == sql.ErrNoRows {
		return cliErr("no convening info for " + id)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	if parsed.flagTruey("json") {
		js, e := cliJSONStringify(map[string]any{
			"pulled_by_id":  pulledByID,
			"pulled_at":     pulledAt,
			"headline_lead": headlineLead,
			"headline_tail": headlineTail,
			"subhead":       subhead,
			"who_and_why":   whoAndWhy,
			"reasoning":     reasoning,
			"status":        status,
		})
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	reasoningLines := ""
	if reasoning != nil {
		parts := make([]string, len(reasoning))
		for i, r := range reasoning {
			if s, ok := r.(string); ok {
				parts[i] = "  " + strconv.Itoa(i+1) + ". " + s
			}
		}
		reasoningLines = strings.Join(parts, "\n")
	}
	whoLines := ""
	if whoAndWhy != nil {
		parts := make([]string, len(whoAndWhy))
		for i, w := range whoAndWhy {
			line := ""
			if m, ok := w.(map[string]any); ok {
				pid, _ := m["pid"].(string)
				line = "  · " + pid
				if reason, _ := m["reason"].(string); reason != "" {
					line += " — " + reason
				}
			}
			parts[i] = line
		}
		whoLines = strings.Join(parts, "\n")
	}
	headline := headlineLead
	if headlineTail != "" {
		headline += " " + headlineTail
	}
	return cliOK(strings.Join([]string{
		"headline:    " + headline,
		"subhead:     " + subhead,
		"pulled by:   " + pulledByID,
		"pulled at:   " + nodeDateToString(time.Time(pulledAt)),
		"status:      " + status,
		"",
		"who & why:",
		whoLines,
		"",
		"reasoning:",
		reasoningLines,
	}, "\n"))
}

// jsonArrayScan:jsonb 列按 JSON 数组读(空/NULL → nil)。
type jsonArrayScan struct{ dst *[]any }

func (j jsonArrayScan) Scan(src any) error {
	switch t := src.(type) {
	case nil:
		*j.dst = nil
		return nil
	case []byte:
		if string(t) == "null" {
			*j.dst = nil
			return nil
		}
		return jsonUnmarshal(t, j.dst)
	case string:
		return jsonUnmarshal([]byte(t), j.dst)
	default:
		return fmt.Errorf("jsonArrayScan: unsupported %T", src)
	}
}

/* ───────── search ───────── */

func (s *Service) cliCmdSearch(ctx context.Context, parsed cliParsed) cliResult {
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr("usage: search <query> [--in <convo_id>] [--limit N]")
	}
	query := parsed.positional[0]
	// 租户隔离(#129):与 HTTP search 同形——本 agent 公司 + 成员会话内
	// 搜;裸 ILIKE 全库扫既把别租户正文回给发起者,又是最大单点负载。
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
	inConvo := ""
	if v, ok := parsed.flags["in"]; ok {
		inConvo = fmt.Sprint(v)
	}
	limit, err := cliMsgFlagNum(parsed, "limit", 10, 50)
	if err != nil {
		return cliErrThrow(err)
	}
	args := []any{"%" + query + "%", companyID, me}
	whereExtra := ""
	if inConvo != "" {
		args = append(args, inConvo)
		whereExtra = `AND m.conversation_id = $4`
	}
	args = append(args, limit)
	limitParam := fmt.Sprintf("$%d", len(args))
	rows, err := s.DB.QueryContext(ctx,
		`SELECT m.id, m.conversation_id, m.author_id, m.body, m.created_at, m.attachment
		   FROM messages m
		   JOIN conversations c ON c.id = m.conversation_id
		  WHERE m.body ILIKE $1
		    AND c.company_id = $2
		    AND EXISTS (SELECT 1 FROM conversation_members cm WHERE cm.conversation_id = c.id AND cm.participant_id = $3) `+whereExtra+`
		  ORDER BY m.created_at DESC LIMIT `+limitParam, args...)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type row struct {
		ID             string        `json:"id"`
		ConversationID string        `json:"conversation_id"`
		AuthorID       string        `json:"author_id"`
		Body           string        `json:"body"`
		CreatedAt      cliISOTime    `json:"created_at"`
		Attachment     cliAttachment `json:"attachment"`
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.ConversationID, &r.AuthorID, &r.Body, &r.CreatedAt, &r.Attachment); err != nil {
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
		suffix := ""
		if inConvo != "" {
			suffix = " in " + inConvo
		}
		return cliOK("(no matches for \"" + query + "\"" + suffix + ")")
	}
	lines := []string{fmt.Sprintf("%d match(es) for \"%s\":", len(all), query), ""}
	lowerQuery := strings.ToLower(query)
	for _, m := range all {
		t := nodeLocaleString(time.Time(m.CreatedAt))
		idx := strings.Index(strings.ToLower(m.Body), lowerQuery)
		from := idx - 20
		if from < 0 {
			from = 0
		}
		to := idx + 100
		if to > len(m.Body) {
			to = len(m.Body)
		}
		slice := strings.ReplaceAll(m.Body[from:to], "\n", " \\n ")
		lines = append(lines, "  · ["+t+"] "+m.ConversationID+" "+m.AuthorID+": …"+slice+"…")
		if att := renderAttachment(m.Attachment); att != "" {
			lines = append(lines, att)
		}
	}
	return cliOK(strings.Join(lines, "\n"))
}

/* ───────── tools-log ───────── */

func (s *Service) cliCmdToolsLog(ctx context.Context, parsed cliParsed) cliResult {
	agent := ""
	if v, ok := parsed.flags["agent"]; ok {
		agent = fmt.Sprint(v)
	}
	limit, err := cliMsgFlagNum(parsed, "limit", 15, 50)
	if err != nil {
		return cliErrThrow(err)
	}
	args := []any{}
	where := ""
	if agent != "" {
		args = append(args, agent)
		where = `WHERE agent_id = $1`
	}
	args = append(args, limit)
	limitParam := fmt.Sprintf("$%d", len(args))
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, agent_id, name, status, duration_ms, args, created_at
		   FROM tool_calls `+where+`
		   ORDER BY created_at DESC LIMIT `+limitParam, args...)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type row struct {
		ID         string         `json:"id"`
		AgentID    string         `json:"agent_id"`
		Name       string         `json:"name"`
		Status     string         `json:"status"`
		DurationMS *int64         `json:"duration_ms"`
		Args       map[string]any `json:"args"`
		CreatedAt  cliISOTime     `json:"created_at"`
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.AgentID, &r.Name, &r.Status, &r.DurationMS, jsonMapScan{&r.Args}, &r.CreatedAt); err != nil {
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
		return cliOK("(no tool calls)")
	}
	lines := []string{fmt.Sprintf("last %d tool call(s):", len(all)), ""}
	for _, r := range all {
		t := nodeHM(time.Time(r.CreatedAt))
		argsBrief := utf16Slice(compactJSON(r.Args), 100)
		dur := "-"
		if r.DurationMS != nil {
			dur = strconv.FormatInt(*r.DurationMS, 10)
		}
		lines = append(lines, "  ["+t+"] "+utf16PadEnd(r.AgentID, 8)+" "+utf16PadEnd(r.Name, 22)+" "+
			utf16PadEnd(r.Status, 7)+" "+dur+"ms  "+argsBrief)
	}
	return cliOK(strings.Join(lines, "\n"))
}

// jsonMapScan:jsonb → map(NULL → nil)。
type jsonMapScan struct{ dst *map[string]any }

func (j jsonMapScan) Scan(src any) error {
	switch t := src.(type) {
	case nil:
		*j.dst = nil
		return nil
	case []byte:
		if string(t) == "null" {
			*j.dst = nil
			return nil
		}
		return jsonUnmarshal(t, j.dst)
	case string:
		return jsonUnmarshal([]byte(t), j.dst)
	default:
		return fmt.Errorf("jsonMapScan: unsupported %T", src)
	}
}

// compactJSON:TS `JSON.stringify(x)`(无缩进、不转义 HTML)。
func compactJSON(v any) string {
	if v == nil {
		return "null"
	}
	var buf bytes.Buffer
	enc := newJSONEncoderNoEscape(&buf)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

/* ───────── participants-status ───────── */

func (s *Service) cliCmdStatusList(ctx context.Context, parsed cliParsed) cliResult {
	// 租户隔离:同 cmdParticipants 的跨租户补丁。
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
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, status, kind FROM participants
		  WHERE company_id = $1 AND kind = 'agent' AND departed_at IS NULL ORDER BY name ASC`,
		companyID)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type row struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Kind   string `json:"kind"`
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &r.Kind); err != nil {
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
		"agent              status",
		"-----------------------------",
	}
	for _, r := range all {
		lines = append(lines, utf16PadEnd(r.Name, 8)+" ("+utf16PadEnd(r.ID, 6)+")  "+r.Status)
	}
	return cliOK(strings.Join(lines, "\n"))
}
