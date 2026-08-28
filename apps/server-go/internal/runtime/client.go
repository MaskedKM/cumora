// runtime 包 client —— inproc-client.ts 的读/写数据面:未读收件箱、
// 上下文历史、climate、头像、成员判定、系统通知、读游标推进。
// SQL 与 TS 版逐字对齐(含 enable_seqscan=off 的 GIN 强制——规划器对
// members @> 误估价会全表扫约 2s 饿死连接池;会话级 GUC、无事务、出错
// 即销毁连接防 GUC 泄漏)。
package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/storage"
)

// GetConversationCompanyId:会话 → 租户(inbox 空时归 run 用)。
func (s *Service) GetConversationCompanyId(ctx context.Context, conversationID string) (*string, error) {
	var companyID sql.NullString
	err := s.DB.QueryRowContext(ctx,
		`SELECT company_id FROM conversations WHERE id = $1 LIMIT 1`, conversationID).Scan(&companyID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !companyID.Valid {
		return nil, nil
	}
	return &companyID.String, nil
}

// NextConversationSequence:counter upsert(种子 2,RETURNING next-1)。
func (s *Service) NextConversationSequence(ctx context.Context, conversationID string) (int64, error) {
	var seq int64
	err := s.DB.QueryRowContext(ctx, `
		INSERT INTO conversation_counters (conversation_id, next_sequence)
		VALUES ($1, 2)
		ON CONFLICT (conversation_id) DO UPDATE SET next_sequence = conversation_counters.next_sequence + 1
		RETURNING next_sequence - 1 AS seq`, conversationID).Scan(&seq)
	if err == sql.ErrNoRows {
		return 1, nil
	}
	return seq, err
}

// withSeqscanOff:独占连接上 SET enable_seqscan=off → 查询 → RESET。
// GUC 泄漏防护(TS release(true) 语义的池化等价物):database/sql 的
// Conn.Close 只会把连接归还池、无法销毁——而 pgx stdlib 的默认
// ResetSession 是 noop,带 GUC 残留的"健康"连接回池后会长期毒化后续
// 查询。因此错误路径必须用不受请求取消影响的独立 ctx 尽力 RESET:
// RESET 成功 → 连接干净归还;RESET 失败 → 连接本身已坏,池的下一次
// 使用/健康检查会将其丢弃。成功路径的 RESET 同样走独立 ctx——查询行
// 读完但请求 ctx 恰被取消(客户端中途断开)时,取消信号不该中断复位。
func (s *Service) withSeqscanOff(ctx context.Context, run func(conn *sql.Conn) error) error {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	failed := false
	defer func() {
		resetCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if failed {
			// 尽力复位;失败即连接已坏,归还后由池的下次使用淘汰。
			_, _ = conn.ExecContext(resetCtx, `RESET enable_seqscan`)
		}
		_ = conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, `SET enable_seqscan = off`); err != nil {
		return err
	}
	// SET 已生效:从此直到 RESET 成功,一律视为可能残留——run 闭包若
	// panic,defer 里的尽力 RESET 同样覆盖(评审复审 MINOR#1)。
	failed = true
	if err := run(conn); err != nil {
		return err
	}
	resetCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(resetCtx, `RESET enable_seqscan`); err != nil {
		return err
	}
	failed = false
	return nil
}

// jsonbCol → any:sql 里 jsonb 列扫为 []byte 后解成 any(nil 保持 nil)。
func jsonbCol(raw []byte) any {
	if raw == nil {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// loadInboxSQL:members GIN 定位会话 + LATERAL 沿 idx_messages_convo_created
// 取每会话未读尾(ROW(created_at,id) > 游标,last_read_message_id 作同瞬
// 消息的决胜);静音会话只放行 direct / 被点名 / 被引用的消息。上限 200。
const loadInboxSQL = `
WITH convos AS (
  SELECT c.id, c.company_id,
         c.title AS conversation_title, c.kind AS conversation_kind, c.topic AS conversation_topic,
         c.project_id, pr.name AS project_name,
         COALESCE(cr.last_read_at, '1970-01-01T00:00:00Z'::timestamptz) AS lr_at,
         COALESCE(cr.last_read_message_id, '') AS lr_id,
         EXISTS (
           SELECT 1 FROM conversation_mutes mu
            WHERE mu.user_id = $1 AND mu.conversation_id = c.id
              AND (mu.muted_until IS NULL OR mu.muted_until > NOW())
         ) AS muted
    FROM conversations c
    LEFT JOIN conversation_reads cr ON cr.user_id = $1 AND cr.conversation_id = c.id
    LEFT JOIN projects pr ON pr.id = c.project_id
   WHERE c.members @> to_jsonb(ARRAY[$1::text])
)
SELECT
  m.id, m.conversation_id, co.company_id,
  co.conversation_title, co.conversation_kind, co.conversation_topic,
  co.project_id, co.project_name,
  m.author_id, p.kind AS author_kind, COALESCE(p.name, m.author_id) AS author_name,
  m.body, m.kind, m.sequence, m.created_at, m.attachment, m.quoted_message_id,
  (
    SELECT jsonb_build_object(
      'id', qm.id, 'authorId', qm.author_id,
      'authorName', COALESCE(qp.name, qm.author_id),
      'kind', qm.kind, 'body', LEFT(qm.body, 240), 'sequence', qm.sequence
    )
      FROM messages qm
      LEFT JOIN participants qp ON qp.id = qm.author_id AND qp.company_id = co.company_id
     WHERE qm.id = m.quoted_message_id AND qm.conversation_id = m.conversation_id
  ) AS quoted
  FROM convos co
  JOIN LATERAL (
    SELECT * FROM messages mm
     WHERE mm.conversation_id = co.id
       AND mm.author_id <> $1
       AND ROW(mm.created_at, mm.id) > ROW(co.lr_at, co.lr_id)
       AND (
         NOT co.muted
         OR co.conversation_kind = 'direct'
         OR EXISTS (
           SELECT 1 FROM regexp_matches(mm.body, '@([[:alnum:]_-]+)', 'g') mention
            WHERE LOWER(mention[1]) = LOWER($1)
         )
         OR EXISTS (
           SELECT 1 FROM messages quoted
            WHERE quoted.id = mm.quoted_message_id
              AND quoted.conversation_id = mm.conversation_id
              AND quoted.author_id = $1
         )
       )
     ORDER BY mm.created_at ASC, mm.id ASC
     LIMIT 200
  ) m ON true
  LEFT JOIN participants p ON p.id = m.author_id AND p.company_id = co.company_id
 ORDER BY m.created_at ASC, m.id ASC
 LIMIT 200`

// LoadInbox:agent 全部会话的未读。注意:此处不推进"已见"边界——推进
// 由真正把行展示给大脑的端点决定(/runtime/inbox 默认推进、?probe=1
// 跳过),避免 maybeSteer 式探测污染基线放走重复碰撞(bram-a520 事故)。
func (s *Service) LoadInbox(ctx context.Context, agentID string) ([]map[string]any, error) {
	var out []map[string]any
	err := s.withSeqscanOff(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, loadInboxSQL, agentID)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanMessageRows(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []map[string]any{}
	}
	freshenAttachmentURLs(out)
	return out, nil
}

// scanMessageRows:inbox/context 共用的消息行扫描(列序与两条 SQL 的
// SELECT 一致;context SQL 在其外层再补 is_unread/is_self/reactions 等)。
func scanMessageRows(rows *sql.Rows) ([]map[string]any, error) {
	var out []map[string]any
	for rows.Next() {
		var (
			id, conversationID, authorID, body, kind                   string
			companyID, conversationTitle, conversationKind             sql.NullString
			conversationTopic, projectID, projectName, quotedMessageID sql.NullString
			authorKind, authorName                                     sql.NullString
			sequence                                                   int64
			createdAt                                                  time.Time
			attachment, quoted                                         []byte
		)
		if err := rows.Scan(
			&id, &conversationID, &companyID, &conversationTitle, &conversationKind, &conversationTopic,
			&projectID, &projectName, &authorID, &authorKind, &authorName,
			&body, &kind, &sequence, &createdAt, &attachment, &quotedMessageID, &quoted,
		); err != nil {
			return nil, err
		}
		m := map[string]any{
			"id":                 id,
			"conversation_id":    conversationID,
			"company_id":         nullStr(companyID),
			"conversation_title": conversationTitle.String,
			"conversation_kind":  conversationKind.String,
			"conversation_topic": nullStr(conversationTopic),
			"author_id":          authorID,
			"author_kind":        nullStr(authorKind),
			"author_name":        authorName.String,
			"body":               body,
			"kind":               kind,
			"sequence":           sequence,
			"created_at":         httpx.ISOms(createdAt),
			"attachment":         jsonbCol(attachment),
			"quoted_message_id":  nullStr(quotedMessageID),
			"quoted":             jsonbCol(quoted),
		}
		// project_id/project_name:TS 侧 pg 驱动对 NULL 列产出显式 null 键
		// (JSON 恒携带),Go 侧对齐——project_id 缺失时也置 null 而非省键。
		if projectID.Valid {
			m["project_id"] = projectID.String
		} else {
			m["project_id"] = nil
		}
		if projectName.Valid {
			m["project_name"] = projectName.String
		} else {
			m["project_name"] = nil
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func nullStr(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

// loadContextSQL:给定会话各取最近 25 条(含已读与自己的历史——真人
// 不从剥离切片推理);human_last_read_at/human_reacted_at 是参与者 kind
// 的 DB 事实(三楼回路的"有人在看"信号);reactions 预聚合。
const loadContextSQL = `
WITH last_reads AS (
  SELECT conversation_id, last_read_at
    FROM conversation_reads
   WHERE user_id = $1
),
recent AS (
  SELECT
    m.id, m.conversation_id, m.author_id, m.kind, m.body, m.sequence,
    m.created_at, m.attachment, m.quoted_message_id,
    c.company_id,
    c.title AS conversation_title, c.kind AS conversation_kind, c.topic AS conversation_topic,
    pr.name AS project_name,
    p.kind AS author_kind,
    COALESCE(p.name, m.author_id) AS author_name,
    COALESCE((SELECT last_read_at FROM last_reads lr WHERE lr.conversation_id = c.id),
             '1970-01-01T00:00:00Z'::timestamptz) AS last_read_at,
    (
      SELECT MAX(cr.last_read_at)
        FROM conversation_reads cr
        JOIN participants hp ON hp.id = cr.user_id AND hp.company_id = c.company_id
       WHERE cr.conversation_id = c.id AND hp.kind = 'human'
    ) AS human_last_read_at
   FROM conversations c
   LEFT JOIN projects pr ON pr.id = c.project_id
   JOIN LATERAL (
     SELECT * FROM messages mm
      WHERE mm.conversation_id = c.id
      ORDER BY mm.created_at DESC
      LIMIT 25
   ) m ON true
   LEFT JOIN participants p ON p.id = m.author_id AND p.company_id = c.company_id
  WHERE c.id = ANY($2::text[])
    AND c.members @> to_jsonb(ARRAY[$1::text])
    AND ($3::text IS NULL OR c.company_id = $3)
)
SELECT id, conversation_id, company_id, conversation_title, conversation_kind, conversation_topic,
       project_name,
       author_id, author_kind, author_name, body, kind, sequence, created_at, attachment,
       quoted_message_id, human_last_read_at,
       (
         SELECT jsonb_build_object(
           'id', qm.id,
           'authorId', qm.author_id,
           'authorName', COALESCE(qp.name, qm.author_id),
           'kind', qm.kind,
           'body', LEFT(qm.body, 240),
           'sequence', qm.sequence
         )
           FROM messages qm
           LEFT JOIN participants qp ON qp.id = qm.author_id AND qp.company_id = recent.company_id
          WHERE qm.id = recent.quoted_message_id
            AND qm.conversation_id = recent.conversation_id
         ) AS quoted,
       (created_at > last_read_at AND author_id <> $1) AS is_unread,
       (author_id = $1) AS is_self,
       COALESCE((
         SELECT jsonb_agg(jsonb_build_object('emoji', emoji, 'users', users) ORDER BY count DESC, emoji ASC)
           FROM (
             SELECT emoji, COUNT(*)::int AS count, array_agg(user_id ORDER BY user_id) AS users
               FROM message_reactions
              WHERE message_id = recent.id
              GROUP BY emoji
           ) r
       ), '[]'::jsonb) AS reactions,
       (
         SELECT MAX(mr.created_at)
           FROM message_reactions mr
           JOIN participants pp ON pp.id = mr.user_id AND pp.company_id = recent.company_id
          WHERE mr.message_id = recent.id AND pp.kind = 'human'
       ) AS human_reacted_at
  FROM recent
 ORDER BY conversation_id, created_at ASC`

// LoadContext:每会话最近历史 + 未读/自发标记 + 反应聚合。
// 租户隔离(#130):members containment 恒生效;companyID 为 nil(JWT
// 缺 claim)时退化为仅成员过滤,不放行跨公司。
func (s *Service) LoadContext(ctx context.Context, agentID string, companyID *string, conversationIDs []string) ([]map[string]any, error) {
	if len(conversationIDs) == 0 {
		return []map[string]any{}, nil
	}
	rows, err := s.DB.QueryContext(ctx, loadContextSQL, agentID, pqArray(conversationIDs), companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var (
			id, conversationID, authorID, body, kind        string
			companyID, conversationTitle, conversationKind  sql.NullString
			conversationTopic, projectName, quotedMessageID sql.NullString
			authorKind, authorName                          sql.NullString
			sequence                                        int64
			createdAt                                       time.Time
			attachment, quoted, reactions                   []byte
			humanLastReadAt, humanReactedAt                 sql.NullTime
			isUnread, isSelf                                bool
		)
		if err := rows.Scan(
			&id, &conversationID, &companyID, &conversationTitle, &conversationKind, &conversationTopic,
			&projectName, &authorID, &authorKind, &authorName, &body, &kind, &sequence, &createdAt,
			&attachment, &quotedMessageID, &humanLastReadAt, &quoted, &isUnread, &isSelf, &reactions, &humanReactedAt,
		); err != nil {
			return nil, err
		}
		m := map[string]any{
			"id":                 id,
			"conversation_id":    conversationID,
			"company_id":         nullStr(companyID),
			"conversation_title": conversationTitle.String,
			"conversation_kind":  conversationKind.String,
			"conversation_topic": nullStr(conversationTopic),
			"project_name":       nullStr(projectName),
			"author_id":          authorID,
			"author_kind":        nullStr(authorKind),
			"author_name":        authorName.String,
			"body":               body,
			"kind":               kind,
			"sequence":           sequence,
			"created_at":         httpx.ISOms(createdAt),
			"attachment":         jsonbCol(attachment),
			"quoted_message_id":  nullStr(quotedMessageID),
			"quoted":             jsonbCol(quoted),
			"is_unread":          isUnread,
			"is_self":            isSelf,
			"reactions":          jsonbCol(reactions),
		}
		if humanLastReadAt.Valid {
			m["human_last_read_at"] = httpx.ISOms(humanLastReadAt.Time)
		} else {
			m["human_last_read_at"] = nil
		}
		if humanReactedAt.Valid {
			m["human_reacted_at"] = httpx.ISOms(humanReactedAt.Time)
		} else {
			m["human_reacted_at"] = nil
		}
		out = append(out, m)
	}
	if out == nil {
		out = []map[string]any{}
	}
	freshenAttachmentURLs(out)
	return out, rows.Err()
}

// pqArray:text[] 参数(pgx v5 原生支持 []string,直接传即可)。
func pqArray(xs []string) []string { return xs }

// freshenAttachmentURLs:inproc-client 的 refreshAttachmentUrls 等价
// (#94 延至此票的 storage 面)——从存量 key(或 /uploads/ 短 URL 反解)
// 重算读 URL。本地模式 URL 稳定,几乎恒 no-op;R2 presign 部署下每个
// 消费者拿到新 TTL 的签名。best-effort:任何失败保持存量 URL。
func freshenAttachmentURLs(rows []map[string]any) {
	for _, row := range rows {
		att, ok := row["attachment"].(map[string]any)
		if !ok {
			continue
		}
		url, _ := att["url"].(string)
		if url == "" {
			continue
		}
		key, _ := att["key"].(string)
		if key == "" {
			// storageKeyFromPublicUrl 共享实现(#77 评审 MINOR2:含
			// percent-decode/trim/前缀白名单,与 TS freshen 同源)。
			key = storage.StorageKeyFromPublicUrl(url)
		}
		if key == "" {
			continue
		}
		att["url"] = storage.PublicUrl(key)
		att["key"] = key
	}
}

// LoadClimate:该 agent 对人们的感受——按更新时间倒序取前 24。
// climate 全局(不跟项目):跨群同一身份。
func (s *Service) LoadClimate(ctx context.Context, agentID string) ([]map[string]any, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT about_id, affinity::text AS affinity, trust, last_note, updated_at
		  FROM agent_climate
		 WHERE agent_id = $1
		 ORDER BY updated_at DESC LIMIT 24`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var aboutID, lastNote string
		var affinityText string
		var trust int64
		var updatedAt time.Time
		if err := rows.Scan(&aboutID, &affinityText, &trust, &lastNote, &updatedAt); err != nil {
			return nil, err
		}
		// affinity 是 float4:pgx 二进制解码走 float32 中转会丢精度
		// (0.7 → 0.6999999880…),TS 侧 node-pg 按文本解析无此问题。
		// ::text + ParseFloat 与 TS 数值语义对齐。
		affinity, _ := strconv.ParseFloat(affinityText, 64)
		out = append(out, map[string]any{
			"about_id":   aboutID,
			"affinity":   affinity,
			"trust":      trust,
			"last_note":  lastNote,
			"updated_at": httpx.ISOms(updatedAt),
		})
	}
	return out, rows.Err()
}

// LoadFaces:传入参与者 id 的头像行(顺序任意;调用方滤掉缺失/本地 URL)。
func (s *Service) LoadFaces(ctx context.Context, participantIDs []string) ([]map[string]any, error) {
	if len(participantIDs) == 0 {
		return []map[string]any{}, nil
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, role, avatar_url FROM participants WHERE id = ANY($1::text[])`,
		pqArray(participantIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name string
		var role, avatar sql.NullString
		if err := rows.Scan(&id, &name, &role, &avatar); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id":         id,
			"name":       name,
			"role":       nullStr(role),
			"avatar_url": nullStr(avatar),
		})
	}
	return out, rows.Err()
}

// HumanRecentlyActive:租户内有 human 的读游标在窗口内推进过(最强
// "有人在看"信号)。三楼回路的 SUPERVISION 信号:人可能在活动自身规则
// 排除他的侧室里旁观——在场抬高下限,从不豁免。
func (s *Service) HumanRecentlyActive(ctx context.Context, companyID string, windowMinutes int) (bool, error) {
	if windowMinutes <= 0 {
		windowMinutes = 10
	}
	var one int
	err := s.DB.QueryRowContext(ctx, `
		SELECT 1
		  FROM conversation_reads cr
		  JOIN participants hp ON hp.id = cr.user_id AND hp.company_id = $1
		 WHERE hp.kind = 'human'
		   AND cr.last_read_at > NOW() - ($2 || ' minutes')::interval
		 LIMIT 1`, companyID, windowMinutes).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// IsConversationMember:JWT 钉死写入的前置闸(如 postSystemNotice)。
func (s *Service) IsConversationMember(ctx context.Context, conversationID, agentID string, companyID *string) (bool, error) {
	var one int
	err := s.DB.QueryRowContext(ctx, `
		SELECT 1 AS ok FROM conversations
		 WHERE id = $1
		   AND ($3::text IS NULL OR company_id = $3)
		   AND members @> to_jsonb(ARRAY[$2::text])
		 LIMIT 1`, conversationID, agentID, companyID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// PostSystemNotice:往会话落一条 kind='system' 的 notice 消息,成员可见
// "agent 为何静默"(配额耗尽、供应商故障等)。NX/EX 去重——同室多 agent
// 同时 429 只发一条;未抢到锁返回 posted=false。Redis 操作错误必须上抛
// (TS 的 set 抛错 → 500,daemon 侧重试)——吞成 posted:false 会让引擎
// 故障通知被静默丢失且永不重试(评审 M3)。
func (s *Service) PostSystemNotice(ctx context.Context, conversationID string, companyID *string,
	agentID, noticeKind, text, dedupeKey string, dedupeTTLSec int) (bool, error) {
	rdb := s.redis()
	if rdb != nil {
		acquired, err := rdb.SetNX(ctxBG, "notice:"+dedupeKey, agentID,
			time.Duration(dedupeTTLSec)*time.Second).Result()
		if err != nil {
			return false, err
		}
		if !acquired {
			return false, nil
		}
	}
	// rdb == nil(单机降级)时跳过去重照发:该形态本就单进程,无跨进程
	// 重复可虑。
	messageID := "m-" + uuidHex()
	sequence, err := s.NextConversationSequence(ctx, conversationID)
	if err != nil {
		return false, err
	}
	body, _ := json.Marshal(map[string]any{
		"kind":       "notice",
		"noticeKind": noticeKind,
		"text":       text,
	})
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
		VALUES ($1,$2,$3,'system',$4,$5,$6)`,
		messageID, conversationID, agentID, string(body), sequence, companyID); err != nil {
		return false, err
	}
	msg := map[string]any{
		"id":             messageID,
		"conversationId": conversationID,
		"authorId":       agentID,
		"kind":           "system",
		"body":           string(body),
		"sequence":       sequence,
		"at":             httpx.ISOms(time.Now()),
	}
	events.MessageNew(ctx, deref(companyID), conversationID, msg)
	return true, nil
}

// MarkConversationRead:ROW 比较的单调推进——仅当新 (created_at, id)
// 字典序超过现存对才更新。幂等、乱序安全、同瞬碰撞安全。
func (s *Service) MarkConversationRead(ctx context.Context, agentID, conversationID, upToMessageID string) error {
	_, err := s.DB.ExecContext(ctx, `
		WITH msg AS (
		  SELECT created_at, id AS message_id FROM messages WHERE id = $1
		)
		INSERT INTO conversation_reads (user_id, conversation_id, last_read_at, last_read_message_id)
		SELECT $2, $3, msg.created_at, msg.message_id FROM msg
		ON CONFLICT (user_id, conversation_id)
		DO UPDATE SET
		  last_read_at = CASE
		    WHEN ROW(EXCLUDED.last_read_at, EXCLUDED.last_read_message_id)
		       > ROW(conversation_reads.last_read_at, conversation_reads.last_read_message_id)
		    THEN EXCLUDED.last_read_at ELSE conversation_reads.last_read_at END,
		  last_read_message_id = CASE
		    WHEN ROW(EXCLUDED.last_read_at, EXCLUDED.last_read_message_id)
		       > ROW(conversation_reads.last_read_at, conversation_reads.last_read_message_id)
		    THEN EXCLUDED.last_read_message_id ELSE conversation_reads.last_read_message_id END`,
		upToMessageID, agentID, conversationID)
	return err
}
