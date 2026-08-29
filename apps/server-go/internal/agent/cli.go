// /runtime/cli 世界动作命令面(#89):TS server/src/agents/cli.ts 的 runCli
// 等价物。daemon 的引擎经 cumora shim 把 argv POST 到本路由,服务端解析
// 子命令、执行 DB+广播行为、返回 CliResult。文本输出与 TS 逐字节对齐
// (mirror 测试为准)。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// scanJSONB:pgx 把 jsonb 列交给 Scan 成 []byte;这里解到目标结构。
func scanJSONB(src any, v any) error {
	switch t := src.(type) {
	case nil:
		return nil
	case []byte:
		return json.Unmarshal(t, v)
	case string:
		return json.Unmarshal([]byte(t), v)
	default:
		return fmt.Errorf("scanJSONB: unsupported %T", src)
	}
}

// without:去掉首个等于 s 的成员(不动原切片)。
func (a cliStrArr) without(s string) cliStrArr {
	out := make(cliStrArr, 0, len(a))
	dropped := false
	for _, v := range a {
		if !dropped && v == s {
			dropped = true
			continue
		}
		out = append(out, v)
	}
	return out
}

func jsonMarshalStrings(xs []string) (string, error) {
	if xs == nil {
		xs = []string{}
	}
	b, err := json.Marshal(xs)
	return string(b), err
}

// eventsPublishMessageNew:CH_MESSAGE_NEW 广播(companyId 空则省键)。
func eventsPublishMessageNew(ctx context.Context, companyID *string, convID string, msg map[string]any) {
	company := ""
	if companyID != nil {
		company = *companyID
	}
	events.MessageNew(ctx, company, convID, msg)
}

func isoNowMs() string { return httpx.ISOms(time.Now()) }

// uuidHex:httpx.UUIDHex 的本包别名(#140 拆包后 cli 面调用点零改动)。
func uuidHex() string { return httpx.UUIDHex() }

// jsonUnmarshal:自 runtime/agenda.go 同名包装平移的副本(#140 拆包;
// #141 横切统一票再议合并)。
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// mustJSON:自 runtime/presence.go 同名助手平移的副本(#140 拆包)。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

/* ============== argv parsing(cli-parse.ts 等价) ============== */

// cliParsed 是 TS ParsedArgs 的等价物:flags 值为 string 或 bool
// (TS 的 string | boolean 联合)。literalFrom 为 -1 表示没有 `--` 标记。
type cliParsed struct {
	positional  []string
	flags       map[string]any
	literalFrom int
}

func cliParseArgs(args []string) cliParsed {
	positional := []string{}
	flags := map[string]any{}
	literalFrom := -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			literalFrom = len(positional)
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "--") {
			key := a[2:]
			if eq := strings.IndexByte(key, '='); eq >= 0 {
				flags[key[:eq]] = key[eq+1:]
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				flags[key] = args[i+1]
				i++
			} else {
				flags[key] = true
			}
		} else {
			positional = append(positional, a)
		}
	}
	return cliParsed{positional: positional, flags: flags, literalFrom: literalFrom}
}

// flagStr 返回 flags[key] 且值为 string 时的值(TS `typeof x === 'string'`
// 检查等价);否则空串+false。
func (p cliParsed) flagStr(key string) (string, bool) {
	v, ok := p.flags[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// flagStrOr:flag 存在且为 string 时返回它,否则返回缺省。
func (p cliParsed) flagStrOr(key, def string) string {
	if s, ok := p.flagStr(key); ok {
		return s
	}
	return def
}

// flagTruey:TS 里 `parsed.flags.x` 直接作真值判断(bool true 或非空
// string 都为真;空串与 false 为假)。
func (p cliParsed) flagTruey(key string) bool {
	v, ok := p.flags[key]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != ""
	}
	return false
}

// joinBodyArgs:把 positional[start:] 拼成一段正文。经过 shell 的参数要
// unescapeChat(bash 工具用单引号包正文,`\n` 以两个字符到达);`--` 之后
// 的字面参数不展开——shim 的 --file/--stdin 内容经 JSON 原样到达,再展开
// 会恰好毁掉它要逐字携带的代码片段。
func (p cliParsed) joinBodyArgs(start int) string {
	literalFrom := p.literalFrom
	if literalFrom < 0 {
		literalFrom = len(p.positional)
	}
	parts := make([]string, 0, len(p.positional)-start)
	for i, part := range p.positional[start:] {
		if start+i >= literalFrom {
			parts = append(parts, part)
		} else {
			parts = append(parts, cliUnescapeChat(part))
		}
	}
	return strings.Join(parts, " ")
}

// cliTokenize:把单条命令行按 bash 风格拆词(空白分词;"…" 与 '…' 成对
// 引号内不分词;引号内不处理转义)。
func cliTokenize(line string) []string {
	out := []string{}
	var buf strings.Builder
	var q rune
	for _, c := range line {
		if q != 0 {
			if c == q {
				q = 0
			} else {
				buf.WriteRune(c)
			}
		} else if c == '"' || c == '\'' {
			q = c
		} else if c == ' ' || c == '\t' {
			if buf.Len() > 0 {
				out = append(out, buf.String())
				buf.Reset()
			}
		} else {
			buf.WriteRune(c)
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

// cliUnescapeChat:把字面转义(\n \t \r \\ \' \")换成真实字符。未知转义
// 原样保留,避免悄悄弄坏模型可能在正文里引用的 Windows 路径。
func cliUnescapeChat(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '\\':
			b.WriteByte('\\')
		case '\'':
			b.WriteByte('\'')
		case '"':
			b.WriteByte('"')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

/* ============== 结果形状(cli-result.ts / cli.ts ok()·err()) ============== */

// CliSideEffect:TS CliSideEffect —— 必有非空字符串 event,其余字段自由。
type CliSideEffect map[string]any

// cliResult:TS CliResult。sideEffects 为 nil 时 JSON 序列化按路由层补
// 空数组(TS 路由 `result.sideEffects ?? []`)。
type cliResult struct {
	ok          bool
	text        string
	exitCode    int
	sideEffects []CliSideEffect
}

// HTTPShape:/runtime/cli HTTP 路由的响应渲染面(runtime 包专用;
// sideEffects nil 补空数组,TS `result.sideEffects ?? []` 平价)。
func (r cliResult) HTTPShape() (ok bool, text string, exitCode int, sideEffects []CliSideEffect) {
	if r.sideEffects == nil {
		r.sideEffects = []CliSideEffect{}
	}
	return r.ok, r.text, r.exitCode, r.sideEffects
}

// 每个结果都经 cliOK/cliErr 出口;在这里剥离 UTF-16 孤代理,正文截断
// (body.slice(0,N) 切半个 emoji)产生的坏序列就到不了模型转录。
func cliOK(text string, effects ...CliSideEffect) cliResult {
	var sides []CliSideEffect
	if len(effects) > 0 {
		sides = effects
	}
	return cliResult{ok: true, text: cliStripLoneSurrogates(text), exitCode: 0, sideEffects: sides}
}

func cliErr(text string) cliResult {
	return cliResult{ok: false, text: cliStripLoneSurrogates(text), exitCode: 1}
}

func cliErrCode(text string, code int) cliResult {
	return cliResult{ok: false, text: cliStripLoneSurrogates(text), exitCode: code}
}

func cliStripLoneSurrogates(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == utf8.RuneError {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

/* ============== 身份(cli-identity.ts / runtime/cli-argv.ts) ============== */

// cliResolveAs:CLI 代理身份解析,优先级与 TS 完全一致:
//  1. 环境运行时身份(CUMORA_AGENT_ID,仅在 http/agent-bash 可信来源下)
//  2. 显式 --as <id>(运行时路由用 JWT 注入)
//  3. CUMORA_DEFAULT_AS(开发便利)
//
// 都没有 → 报错:静默缺省会重新打开本函数要关的冒充漏洞。
func cliResolveAs(p cliParsed) (string, error) {
	if os.Getenv("CUMORA_RUNTIME_CLIENT") == "http" || os.Getenv("CUMORA_CLI_IDENTITY_SOURCE") == "agent-bash" {
		if pinned := os.Getenv("CUMORA_AGENT_ID"); pinned != "" {
			return pinned, nil
		}
	}
	if explicit, ok := p.flagStr("as"); ok && explicit != "" {
		return explicit, nil
	}
	if fromEnv := os.Getenv("CUMORA_DEFAULT_AS"); fromEnv != "" {
		return fromEnv, nil
	}
	return "", fmt.Errorf("--as <participant_id> is required (or set CUMORA_DEFAULT_AS in your env for dev)")
}

// cliStripAsArgs:剥掉 argv 里所有 `--as <id>` / `--as=<id>`。JWT 钉死身
// 份后必须剥离调用方自带的 --as(parseArgs 取最后一次出现,不剥就会被
// 冒充);其余 token 保持原序。
func cliStripAsArgs(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if tok == "--as" {
			i++ // 连值一起跳过;缺值也仍丢掉 --as 本身
			continue
		}
		if strings.HasPrefix(tok, "--as=") {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// BuildRuntimeArgv:最终交给 RunCli 的 argv——头部注入 `--as <sub>`,
// 尾部剥净调用方 --as(防御纵深:注入的头一个在最左,就算有漏网的也赢
// 不过它)。
func BuildRuntimeArgv(jwtSub string, clientArgv []string) []string {
	return append([]string{"--as", jwtSub}, cliStripAsArgs(clientArgv)...)
}

/* ============== 分发(cli.ts runCli) ============== */

// RunCli:与 TS runCli 等价的命令分发。argv 首部的全局 `--as`(可能多个)
// 摘下后重新挂到子命令参数上,让各命令的解析器照常看到 flags.as。
func (s *Service) RunCli(ctx context.Context, argv []string) (res cliResult) {
	var asFlag *string
	for len(argv) > 0 && (argv[0] == "--as" || strings.HasPrefix(argv[0], "--as=")) {
		if argv[0] == "--as" {
			if len(argv) > 1 {
				v := argv[1]
				asFlag = &v
			}
			argv = argv[2:]
		} else {
			v := strings.TrimPrefix(argv[0], "--as=")
			asFlag = &v
			argv = argv[1:]
		}
	}
	if len(argv) == 0 {
		return s.cliCmdHelp()
	}
	sub, rest := argv[0], argv[1:]
	parsed := cliParseArgs(rest)
	if asFlag != nil {
		if _, exists := parsed.flags["as"]; !exists {
			parsed.flags["as"] = *asFlag
		}
	}
	// TS runCli 的 try/catch:handler panic/解析错误 → "error: …" exit 2。
	defer func() {
		if r := recover(); r != nil {
			res = cliErrCode(fmt.Sprintf("error: %v", r), 2)
		}
	}()
	switch sub {
	case "help", "--help", "-h":
		return s.cliCmdHelp()
	case "whoami":
		return s.cliCmdWhoami(ctx, parsed)
	case "participants":
		return s.cliCmdParticipants(ctx, parsed)
	case "conversations":
		return s.cliCmdConversations(ctx, parsed, "")
	case "groups":
		return s.cliCmdConversations(ctx, parsed, "group")
	case "directs":
		return s.cliCmdConversations(ctx, parsed, "direct")
	case "members":
		return s.cliCmdMembers(ctx, parsed)
	case "messages":
		return s.cliCmdMessages(ctx, parsed)
	case "thread":
		return s.cliCmdThread(ctx, parsed)
	case "convening":
		return s.cliCmdConvening(ctx, parsed)
	case "search":
		return s.cliCmdSearch(ctx, parsed)
	case "tools-log":
		return s.cliCmdToolsLog(ctx, parsed)
	case "participants-status":
		return s.cliCmdStatusList(ctx, parsed)
	case "reply":
		return s.cliCmdReply(ctx, parsed)
	case "leave":
		return s.cliCmdLeave(ctx, parsed)
	case "invite":
		return s.cliCmdInvite(ctx, parsed)
	case "kick":
		return s.cliCmdKick(ctx, parsed)
	case "topic":
		return s.cliCmdTopicRead(ctx, parsed)
	case "topic-set":
		return s.cliCmdTopicSet(ctx, parsed)
	case "rename":
		return s.cliCmdRename(ctx, parsed)
	case "memory":
		return s.cliCmdMemory(ctx, parsed)
	case "climate":
		return s.cliCmdClimate(ctx, parsed)
	case "log":
		return s.cliCmdLog(ctx, parsed)
	case "workspace":
		return s.cliCmdTeamWorkspace(ctx, parsed)
	case "ws":
		return s.cliCmdWorkspace(ctx, parsed)
	case "tasks":
		return s.cliCmdTasks(ctx, parsed)
	case "avatar":
		return s.cliCmdAvatar(ctx, parsed)
	case "skills":
		return s.cliCmdSkills(ctx, parsed)
	case "react":
		return s.cliRunTool(ctx, "react", parsed)
	case "dm":
		return s.cliRunTool(ctx, "dm_with", parsed)
	case "pull-group":
		return s.cliRunTool(ctx, "pull_group", parsed)
	case "palette":
		return s.cliRunTool(ctx, "palette", parsed)
	case "image":
		return s.cliCmdImage(ctx, parsed)
	default:
		// 域子包命令(#140 刀法):boards/email/mailbox/… 居 agent/<domain>,
		// 由 runtime 接线注入(本包不得 import 子包——防环)。
		if s.domainDispatch != nil {
			if res, ok := s.domainDispatch(sub, ctx, parsed); ok {
				return res
			}
		}
		return cliErr("unknown subcommand: " + sub + "\nrun \"cumora help\" for usage")
	}
}
