// Code generated from packages/contract/ws-events.json — DO NOT EDIT.
// 再生:npm run contract:gen(票 #221)。事件载荷/通道/作用域语义见契约;
// 手写面(internal/events/publish.go、wsx 网关)只组装此处类型,不再内联字面量。
package events

/* ── 事件名常量(载荷 type 判别值;发布方组帧时填入 Type 字段)── */
const (
	EventMessageNew           = "message.new"
	EventMessageDelta         = "message.delta"
	EventTyping               = "typing"
	EventStatus               = "participants.status"
	EventAvatar               = "participants.avatar"
	EventParticipantAdded     = "participants.added"
	EventComputerStatus       = "computers.status"
	EventReactions            = "message.reactions"
	EventPollUpdated          = "poll.updated"
	EventConversationUpdated  = "conversation.updated"
	EventGroupPulled          = "group.pulled"
	EventConvene              = "convene"
	EventBoard                = "board.changed"
	EventDocIndex             = "doc.changed"
	EventDocUpdate            = "doc.update"
	EventDocAwareness         = "doc.awareness"
	EventDocSync              = "doc.sync"
	EventDocError             = "doc.error"
	EventDocMention           = "doc.mention"
	EventCalendarReminder     = "calendar.reminder"
	EventCalendarEventChanged = "calendar.changed"
	EventHello                = "hello"
)

/* ── Redis 通道常量(x-channels;名字与退役前 publish.go 手写版逐一相同,
 *  全仓引用方零改动)── */
const (
	ChMessageNew       = "cumora:msg.new"
	ChMessageDelta     = "cumora:msg.delta"
	ChTyping           = "cumora:typing"
	ChStatus           = "cumora:status"
	ChReactions        = "cumora:reactions"
	ChPolls            = "cumora:polls"
	ChGroupPulled      = "cumora:group.pulled"
	ChConvoUpdated     = "cumora:convo.updated"
	ChConvene          = "cumora:convene"
	ChBoards           = "cumora:boards"
	ChDocs             = "cumora:docs"
	ChDocUpdate        = "cumora:doc.update"
	ChDocAware         = "cumora:doc.awareness"
	ChDocMention       = "cumora:doc.mention"
	ChCalendarReminder = "cumora:calendar.reminder"
	ChCalendarEvents   = "cumora:calendar.events"
)

// CompanyChannels:公司域事件通道全集 —— wsx 聊天桥订阅清单(#221 起由契约
// 推导:scope=company 事件的通道按首现去重;doc.update/doc.awareness 是房间
// 域(docrelay 管),不在其列。对齐退役前 publish.go 的手写清单(顺序为推导序)。
var CompanyChannels = []string{
	ChMessageNew, ChMessageDelta, ChTyping, ChStatus, ChReactions, ChPolls, ChConvoUpdated, ChGroupPulled, ChConvene, ChBoards, ChDocs, ChDocMention, ChCalendarReminder, ChCalendarEvents,
}

// MessageNewEvent —— message.new(通道 cumora:msg.new、scope=company)。
// 一条新消息落库后的广播。message 为消息 wire 全量(shape = OpenAPI Message:at/quoted/email/poll/pollTallies 等),clientId 用于前端乐观气泡与 WS 回显去重(tempId 键配对)。
type MessageNewEvent struct {
	Type           string         `json:"type"`
	CompanyID      string         `json:"companyId,omitempty"`
	ConversationID string         `json:"conversationId"`
	Message        map[string]any `json:"message"`
}

// MessageDeltaEvent —— message.delta(通道 cumora:msg.delta、scope=company)。
// 流式增量(代理边打边发,#210 已激活)。daemon 铸流 id(messageId)与终局消息 id 不配对——前端按 (conversationId, authorId) 收口,message.new / done / 陈旧兜底三条退场路径幂等;delta 只上屏不入库。
type MessageDeltaEvent struct {
	Type           string `json:"type"`
	CompanyID      string `json:"companyId,omitempty"`
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	AuthorID       string `json:"authorId"`
	Delta          string `json:"delta"`
	Sequence       int    `json:"sequence"`
	Done           bool   `json:"done"`
}

// TypingEvent —— typing(通道 cumora:typing、scope=company)。
// 代理输入态:done=false 开始 / true 停止。
type TypingEvent struct {
	Type           string `json:"type"`
	CompanyID      string `json:"companyId"`
	ConversationID string `json:"conversationId"`
	AgentID        string `json:"agentId"`
	Done           bool   `json:"done"`
}

// StatusEvent —— participants.status(通道 cumora:status、scope=company)。
// 参与者在场/工作态翻转。statusUpdatedAt 缺席 = 广播方未提供(前端按 undefined 容忍)。
type StatusEvent struct {
	Type            string `json:"type"`
	CompanyID       string `json:"companyId,omitempty"`
	ParticipantID   string `json:"participantId"`
	Status          string `json:"status"`
	StatusUpdatedAt string `json:"statusUpdatedAt,omitempty"`
}

// AvatarEvent —— participants.avatar(通道 cumora:status、scope=company)。
// 代理头像(重)生成完毕;前端 participants store 原地补丁,不等 60s 刷新滴答。
type AvatarEvent struct {
	Type          string `json:"type"`
	CompanyID     string `json:"companyId,omitempty"`
	ParticipantID string `json:"participantId"`
	AvatarURL     string `json:"avatarUrl"`
}

// ParticipantAddedEvent —— participants.added(通道 cumora:status、scope=company)。
// 人类接受邀请(或以其他方式新镜像进公司 participants)。participant 为 ApiParticipant 的窄投影:前端 byId 原地 upsert 不再拉取;conversationId 在场时前端顺手补对应会话的成员表。
type ParticipantAddedEvent struct {
	Type           string         `json:"type"`
	CompanyID      string         `json:"companyId,omitempty"`
	ConversationID string         `json:"conversationId,omitempty"`
	Participant    map[string]any `json:"participant"`
}

// ComputerStatusEvent —— computers.status(通道 cumora:status、scope=company)。
// BYOA Computer(代理宿主)上线/离线/忙碌;Computers 面板与代理芯片即时反映宿主可用性。
type ComputerStatusEvent struct {
	Type       string `json:"type"`
	CompanyID  string `json:"companyId,omitempty"`
	ComputerID string `json:"computerId"`
	Status     string `json:"status"`
}

// ReactionsEvent —— message.reactions(通道 cumora:reactions、scope=company)。
// 某条消息的表情反应聚合(全量快照,非增量)。
type ReactionsEvent struct {
	Type           string           `json:"type"`
	CompanyID      string           `json:"companyId,omitempty"`
	ConversationID string           `json:"conversationId"`
	MessageID      string           `json:"messageId"`
	Reactions      []map[string]any `json:"reactions"`
}

// PollUpdatedEvent —— poll.updated(通道 cumora:polls、scope=company)。
// 投票状态变更(投票/改票/手动或到期关闭):携带全量反规范化快照,渲染端原地补丁免重拉。
type PollUpdatedEvent struct {
	Type           string           `json:"type"`
	CompanyID      string           `json:"companyId,omitempty"`
	ConversationID string           `json:"conversationId"`
	MessageID      string           `json:"messageId"`
	Poll           map[string]any   `json:"poll"`
	Tallies        []map[string]any `json:"tallies"`
	ActorID        *string          `json:"actorId"`
}

// ConversationUpdatedEvent —— conversation.updated(通道 cumora:convo.updated、scope=company)。
// 会话元数据补丁(主题/标题);客户端外科手术式补丁,不重拉。
type ConversationUpdatedEvent struct {
	Type           string         `json:"type"`
	CompanyID      string         `json:"companyId,omitempty"`
	ConversationID string         `json:"conversationId"`
	Patch          map[string]any `json:"patch"`
}

// GroupPulledEvent —— group.pulled(通道 cumora:group.pulled、scope=company)。
// 代理拉群完成通知;前端刷新会话列表见新群。
type GroupPulledEvent struct {
	Type           string `json:"type"`
	CompanyID      string `json:"companyId,omitempty"`
	ConversationID string `json:"conversationId"`
	PulledByID     string `json:"pulledById"`
}

// ConveneEvent —— convene(通道 cumora:convene、scope=company)。
// convene(临场圆桌)会话生命周期:轻载荷,前端收到后重拉全量。
type ConveneEvent struct {
	Type           string `json:"type"`
	CompanyID      string `json:"companyId,omitempty"`
	SessionID      string `json:"sessionId"`
	ConversationID string `json:"conversationId"`
	Kind           string `json:"kind"`
	Data           any    `json:"data,omitempty"`
}

// BoardEvent —— board.changed(通道 cumora:boards、scope=company)。
// 看板变更广播。刻意粗粒度:携带 boardId,前端按需重拉该板;卡级形状回显供乐观渲染免拉取;mentions 回显供通知 toaster 在用户没盯着看板时也能响。
type BoardEvent struct {
	Type      string   `json:"type"`
	CompanyID string   `json:"companyId,omitempty"`
	Kind      string   `json:"kind"`
	BoardID   string   `json:"boardId"`
	CardID    string   `json:"cardId,omitempty"`
	ColumnID  string   `json:"columnId,omitempty"`
	CommentID string   `json:"commentId,omitempty"`
	Mentions  []string `json:"mentions,omitempty"`
	ActorID   string   `json:"actorId,omitempty"`
}

// DocIndexEvent —— doc.changed(通道 cumora:docs、scope=company)。
// 文档索引/元数据变更(内容同步走 doc.* 房间域通道;本事件只让客户端刷新文档列表,代理新文档即刻可见)。
type DocIndexEvent struct {
	Type       string `json:"type"`
	CompanyID  string `json:"companyId,omitempty"`
	Kind       string `json:"kind"`
	DocumentID string `json:"documentId"`
	ActorID    string `json:"actorId,omitempty"`
}

// DocUpdateEvent —— doc.update(通道 cumora:doc.update、scope=doc)。
// Yjs 增量已应用到文档房间。base64 承载(WS 路径 JSON-only);originId 供发送方自己的连接回声抑制。Redis 总线上的载荷(sidecar → docrelay)多带 companyId/authorId;wsx 网关下发帧不透出这两键 —— 契约按并集建模(均可选)。
type DocUpdateEvent struct {
	Type       string `json:"type"`
	CompanyID  string `json:"companyId,omitempty"`
	DocumentID string `json:"documentId"`
	UpdateB64  string `json:"updateB64"`
	OriginID   string `json:"originId"`
	AuthorID   string `json:"authorId,omitempty"`
}

// DocAwarenessEvent —— doc.awareness(通道 cumora:doc.awareness、scope=doc)。
// awareness(光标/选区/在场)—— 临时态不落库,与 doc.update 同扇出路径。
type DocAwarenessEvent struct {
	Type       string `json:"type"`
	CompanyID  string `json:"companyId,omitempty"`
	DocumentID string `json:"documentId"`
	UpdateB64  string `json:"updateB64"`
	OriginID   string `json:"originId"`
}

// DocSyncEvent —— doc.sync(scope=gateway)。
// 文档订阅握手回执:订阅登记成功即下发全量 state(客户端以收到此帧 = 订阅建立)。
type DocSyncEvent struct {
	Type       string `json:"type"`
	DocumentID string `json:"documentId"`
	StateB64   string `json:"stateB64"`
	OriginID   string `json:"originId"`
}

// DocErrorEvent —— doc.error(scope=gateway)。
// doc.* 帧服务端错误统一回执(订阅失败/未找到等);documentId 可能缺席(畸形帧无有效 id)。
type DocErrorEvent struct {
	Type       string `json:"type"`
	DocumentID string `json:"documentId"`
	Error      string `json:"error"`
}

// DocMentionEvent —— doc.mention(通道 cumora:doc.mention、scope=company)。
// 文档内 @mention:载荷足以让渲染端零往返弹 toast(标题/提及者显示名/目标 id 列表),走公司域通用桥 —— 收件人不必开着该文档。
type DocMentionEvent struct {
	Type          string   `json:"type"`
	CompanyID     string   `json:"companyId,omitempty"`
	DocumentID    string   `json:"documentId"`
	DocumentTitle string   `json:"documentTitle"`
	MentionerID   string   `json:"mentionerId"`
	MentionerName string   `json:"mentionerName"`
	MentionedIDs  []string `json:"mentionedIds"`
}

// CalendarReminderEvent —— calendar.reminder(通道 cumora:calendar.reminder、scope=company)。
// 「距此日历事件触发还有 N 分钟」提醒。桥扇出到全公司,渲染端按 recipientUserIds.includes(meId) 过滤;接收者可为空(仅代理的事件 —— 桥照发,无渲染端匹配)。
type CalendarReminderEvent struct {
	Type             string   `json:"type"`
	CompanyID        string   `json:"companyId,omitempty"`
	EventID          string   `json:"eventId"`
	Title            string   `json:"title"`
	OccurrenceAt     string   `json:"occurrenceAt"`
	LeadMinutes      int      `json:"leadMinutes"`
	RecipientUserIDs []string `json:"recipientUserIds"`
	Kind             string   `json:"kind"`
	AssigneeID       *string  `json:"assigneeId"`
}

// CalendarEventChangedEvent —— calendar.changed(通道 cumora:calendar.events、scope=company)。
// 日历行 CRUD / 派发器推进 last_fired_at。载荷刻意薄:客户端重拉受影响行(删除则丢弃),不做行内 diff —— 与 doc.changed 同思路,避免 router 与 CLI 两套编码器锁步。
type CalendarEventChangedEvent struct {
	Type      string  `json:"type"`
	CompanyID string  `json:"companyId,omitempty"`
	Kind      string  `json:"kind"`
	EventID   string  `json:"eventId"`
	ActorID   *string `json:"actorId"`
}

// HelloEvent —— hello(scope=gateway)。
// 握手帧:握手完成即发(重连同发)。客户端以收到 hello = 连接完成,据此重放 doc.subscribe、冲刷断线攒批、重引数据;它是每条连接的第一帧。
type HelloEvent struct {
	Type       string `json:"type"`
	InstanceID string `json:"instanceId"`
	Ts         int64  `json:"ts"`
}
