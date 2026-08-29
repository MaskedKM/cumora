// runtime 包 scanner —— 背景扫描(#62):scanner.ts 的 Go 等价。对带
// background.scan 能力的 agent 周期投递近 24h 群聊活动简报(默认不动,
// 只有人格与判断说"有具体、及时的理由"才打断人)。进程内指纹去重——
// 同一活动集合对同一 agent 只扫一次(重启后重扫一次,TS 同)。
package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
)

const scannerMinMessages = 8
const scannerWindowHours = 24

var scannerPrecedentMu sync.Mutex

// #135:key = company|agent,值 = 该 agent 最新指纹。指纹是 24h 窗口内
// 消息 id 全集——窗口只会进新 id、旧 id 出窗后不可回,历史指纹永不复发,
// 留最新一条即保留全部去重语义;旧形状(整指纹作 key)每天新消息即新
// 条目,只增不减(~2.3MB/agent/天)。
var scannerPrecedentScans = map[string]string{}

// envIntRaw 语义:SCANNER_INTERVAL_MS=0 原样生效(TS setInterval(fn,0)
// 热循环怪癖按平价复刻,非数回落 90s 默认)。
func scannerIntervalMS() int64 {
	if n, ok := envIntRaw("SCANNER_INTERVAL_MS"); ok {
		return n
	}
	return 90_000
}

type scanAgentRow struct {
	id        string
	name      string
	role      sql.NullString
	bio       sql.NullString
	companyID string
}

// loadBackgroundScanAgents: tools @> ['background.scan'] 的在册 agent。
func (s *Service) loadBackgroundScanAgents(ctx context.Context) []scanAgentRow {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, name, role, bio, company_id
		  FROM participants p
		 WHERE p.kind = 'agent'
		   AND p.departed_at IS NULL
		   AND EXISTS (
		     SELECT 1
		       FROM jsonb_array_elements_text(COALESCE(p.tools, '[]'::jsonb)) AS tool(value)
		      WHERE tool.value = ANY($1::text[])
		   )
		 ORDER BY p.company_id, p.name`, []string{"background.scan"})
	if err != nil {
		slog.Warn("[scanner] load agents failed", "err", err)
		return nil
	}
	defer rows.Close()
	var out []scanAgentRow
	for rows.Next() {
		var r scanAgentRow
		if err := rows.Scan(&r.id, &r.name, &r.role, &r.bio, &r.companyID); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// agentHasUnreadInbox: 有未读则跳过本轮(脑内有活,不叠背景扫描)。
func (s *Service) agentHasUnreadInbox(ctx context.Context, agentID string) bool {
	var exists bool
	err := s.DB.QueryRowContext(ctx, `
		SELECT EXISTS (
		   SELECT 1
		     FROM messages m
		     JOIN conversations c ON c.id = m.conversation_id
		    WHERE EXISTS (SELECT 1 FROM conversation_members cm WHERE cm.conversation_id = c.id AND cm.participant_id = $1)
		      AND m.author_id <> $1
		      AND ROW(m.created_at, m.id) > (
		        SELECT
		          COALESCE(cr.last_read_at, '1970-01-01T00:00:00Z'::timestamptz),
		          COALESCE(cr.last_read_message_id, '')
		          FROM (SELECT 1) AS _
		          LEFT JOIN conversation_reads cr
		            ON cr.user_id = $1 AND cr.conversation_id = c.id
		      )
		    LIMIT 1
		 )`, agentID).Scan(&exists)
	return err == nil && exists
}

type scanRecentMessage struct {
	messageID  string
	convoID    string
	convoTitle string
	authorName string
	body       string
}

func (s *Service) loadRecentActivity(ctx context.Context, companyID string) []scanRecentMessage {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT
		    m.id AS message_id,
		    m.conversation_id,
		    c.title AS conversation_title,
		    COALESCE(p.name, m.author_id) AS author_name,
		    m.body
		  FROM messages m
		  JOIN conversations c ON c.id = m.conversation_id
		  LEFT JOIN participants p ON p.id = m.author_id AND p.company_id = c.company_id
		 WHERE c.kind = 'group'
		   AND m.kind = 'text'
		   AND c.company_id = $1
		   AND m.created_at > NOW() - ($2 || ' hours')::interval
		 ORDER BY m.created_at DESC, m.id DESC
		 LIMIT 80`, companyID, "24")
	if err != nil {
		slog.Warn("[scanner] load activity failed", "err", err)
		return nil
	}
	defer rows.Close()
	var out []scanRecentMessage
	for rows.Next() {
		var r scanRecentMessage
		if err := rows.Scan(&r.messageID, &r.convoID, &r.convoTitle, &r.authorName, &r.body); err == nil {
			out = append(out, r)
		}
	}
	return out
}

type scanRosterRow struct {
	id   string
	name string
	role sql.NullString
	kind string
}

func (s *Service) loadScanRoster(ctx context.Context, companyID string) []scanRosterRow {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, name, role, kind
		  FROM participants
		 WHERE company_id = $1 AND departed_at IS NULL
		 ORDER BY kind DESC, name ASC`, companyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []scanRosterRow
	for rows.Next() {
		var r scanRosterRow
		if err := rows.Scan(&r.id, &r.name, &r.role, &r.kind); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// renderActivitySummary: 按会话分组,每会话取最近 12 条(时间正序)。
func renderActivitySummary(rows []scanRecentMessage) string {
	byConvo := map[string]*struct {
		title string
		lines []string
	}{}
	var order []string
	for _, r := range rows {
		v, ok := byConvo[r.convoID]
		if !ok {
			v = &struct {
				title string
				lines []string
			}{title: r.convoTitle}
			byConvo[r.convoID] = v
			order = append(order, r.convoID)
		}
		v.lines = append(v.lines, "["+r.messageID+"] "+r.authorName+": "+agent.TruncateRunesSimple(r.body, 240))
	}
	parts := make([]string, 0, len(order))
	for _, id := range order {
		v := byConvo[id]
		lines := v.lines
		if len(lines) > 12 {
			lines = lines[:12]
		}
		// rows 是 DESC 取的;组内逆序回时间正序(TS .reverse() 等价)。
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
		parts = append(parts, "# "+v.title+" ("+id+")\n"+strings.Join(lines, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// buildBackgroundScanBrief: 简报正文(逐字对齐 TS 模板)。
func buildBackgroundScanBrief(agent scanAgentRow, roster []scanRosterRow, recent []scanRecentMessage) string {
	var agentIDs, humanIDs []string
	for _, r := range roster {
		if r.kind == "agent" {
			entry := r.id
			if r.role.Valid && r.role.String != "" {
				entry += " (" + r.role.String + ")"
			}
			agentIDs = append(agentIDs, entry)
		}
	}
	for _, r := range roster {
		if r.kind == "human" {
			entry := r.id
			if r.role.Valid && r.role.String != "" {
				entry += " (" + r.role.String + ")"
			}
			humanIDs = append(humanIDs, entry)
		}
	}
	agentList, humanList := "(none)", "(none)"
	if len(agentIDs) > 0 {
		agentList = strings.Join(agentIDs, ", ")
	}
	if len(humanIDs) > 0 {
		humanList = strings.Join(humanIDs, ", ")
	}
	who := agent.name
	if agent.role.Valid && agent.role.String != "" {
		who += ", " + agent.role.String
	}
	return "You are " + who + ". You have the background.scan capability, so the runtime is giving you recent company activity to inspect.\n" +
		"\n" +
		"This is not a direct user request. Default to no action. Only interrupt people when your own persona and judgment say there is a concrete, timely reason.\n" +
		"\n" +
		"If you pull a group, use the normal tool yourself:\n" +
		"  bash(\"cumora pull-group '<title>' --members id1,id2,id3 --reason '<why now>' --say '<opening message with concrete evidence>'\")\n" +
		"\n" +
		"For brand / voice / cross-project collision scans, require specific evidence:\n" +
		"- quote at least two concrete message snippets or message ids from different parts of the activity\n" +
		"- explain the collision in plain language\n" +
		"- include only the people who can actually resolve it\n" +
		"\n" +
		"Available agents: " + agentList + "\n" +
		"Available humans: " + humanList + "\n" +
		"\n" +
		"Recent group activity from the last 24 hours:\n" +
		"\n" +
		renderActivitySummary(recent)
}

// 上抛错误:TS 的 await INSERT 在 per-agent try 内,失败即跳过唤醒。
func (s *Service) recordScanWake(ctx context.Context, agent scanAgentRow, fingerprint string) error {
	ref, _ := json.Marshal(map[string]string{
		"source":      "background_scanner",
		"capability":  "background.scan",
		"fingerprint": fingerprint,
	})
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO agent_log (id, agent_id, company_id, kind, body, ref)
		VALUES ($1, $2, $3, 'note', $4, $5::jsonb)`,
		"log-"+randHex12(), agent.id, agent.companyID,
		"background scan wake queued for "+agent.name, string(ref))
	return err
}

// RunBackgroundScans: 一轮扫描(导出供测试)。
func (s *Service) RunBackgroundScans(ctx context.Context) {
	for _, agent := range s.loadBackgroundScanAgents(ctx) {
		func(a scanAgentRow) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Warn("[scanner] background scan failed", "agent", a.id, "recover", rec)
				}
			}()
			if s.agentHasUnreadInbox(ctx, a.id) {
				return
			}
			recent := s.loadRecentActivity(ctx, a.companyID)
			if len(recent) < scannerMinMessages {
				return
			}
			ids := make([]string, 0, len(recent))
			for _, r := range recent {
				ids = append(ids, r.messageID)
			}
			sort.Strings(ids)
			fingerprint := a.companyID + "|" + a.id + "|" + strings.Join(ids, "|")
			if scannerSeenOrMark(a.companyID+"|"+a.id, fingerprint) {
				return
			}

			if err := s.recordScanWake(ctx, a, fingerprint); err != nil {
				slog.Warn("[scanner] record wake failed", "agent", a.id, "err", err)
				return
			}
			roster := s.loadScanRoster(ctx, a.companyID)
			s.wakeOne(a.id, "background_scan", nil, nil, &WakeOpts{
				BackgroundBrief: &BackgroundBrief{
					Source: "background_scanner",
					Title:  "Recent company activity scan",
					Body:   buildBackgroundScanBrief(a, roster, recent),
				},
			})
		}(agent)
	}
}

// scannerSeenOrMark:指纹去重,该 agent 已扫过同一指纹返回 true,
// 未见则记下(只留最新)后返回 false。
func scannerSeenOrMark(agentKey, fingerprint string) bool {
	scannerPrecedentMu.Lock()
	defer scannerPrecedentMu.Unlock()
	if scannerPrecedentScans[agentKey] == fingerprint {
		return true
	}
	scannerPrecedentScans[agentKey] = fingerprint
	return false
}

// ResetBackgroundScannerForTests: 测试隔离入口(生产不调用)。
func ResetBackgroundScannerForTests() {
	scannerPrecedentMu.Lock()
	defer scannerPrecedentMu.Unlock()
	scannerPrecedentScans = map[string]string{}
}

// StartScanner: 周期 kick;ENABLE_SCANNER='false' 关闭(nil = 未启动)。
// TS 门控是字面 !== 'false'(仅精确串关闭)。
func (s *Service) StartScanner() (stop func()) {
	if getenv("ENABLE_SCANNER") == "false" {
		return nil
	}
	interval := scannerIntervalMS()
	// time.Ticker(0) 会 panic;钳 1ms = Node setInterval 对 <1 的钳制
	// 语义本身(评审注记)。唤醒仍由 20/min 低优先级预算 + 指纹去重封顶。
	if interval <= 0 {
		interval = 1
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Millisecond)
	go func() {
		for range ticker.C {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						slog.Error("[scanner] tick panicked", "recover", rec)
					}
				}()
				s.RunBackgroundScans(ctxBG)
			}()
		}
	}()
	slog.Info("[boot] background scanner running", "interval_ms", interval)
	return ticker.Stop
}
