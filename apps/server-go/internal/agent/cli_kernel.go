// agent 包 cli_kernel —— 域子包(agent/<domain>)专用的内核管道导出面
// (#140 刀法):cli 解析/结果/UTF-16 助手以别名+包装函数导出,内核内部
// 名零改动,移出的域文件只做符号限定。
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	"github.com/MaskedKM/cumora/apps/server-go/internal/jsonx"
)

// Parsed / Result / StrArr:内核 cliParsed/cliResult/cliStrArr 的导出别名。
type Parsed = cliParsed
type Result = cliResult
type StrArr = cliStrArr

// OK / Err / ErrCode:cliOK/cliErr/cliErrCode 的导出包装。
func OK(text string, effects ...CliSideEffect) Result { return cliOK(text, effects...) }
func Err(text string) Result                          { return cliErr(text) }
func ErrCode(text string, code int) Result            { return cliErrCode(text, code) }

// FlagStr / FlagStrOr / FlagTruey / JoinBodyArgs:cliParsed 私有方法的
// 导出包装(域子包经别名类型只能触达导出方法)。
func (p cliParsed) FlagStr(key string) (string, bool) { return p.flagStr(key) }
func (p cliParsed) FlagStrOr(key, def string) string  { return p.flagStrOr(key, def) }
func (p cliParsed) FlagTruey(key string) bool         { return p.flagTruey(key) }
func (p cliParsed) JoinBodyArgs(start int) string     { return p.joinBodyArgs(start) }

// UTF16Slice / UTF16PadEnd / UnescapeChat / MarshalStrings / UUIDHex:
// 内核文本与序列化助手的导出包装。
func UTF16Slice(s string, n int) string  { return utf16Slice(s, n) }
func UTF16PadEnd(s string, n int) string { return utf16PadEnd(s, n) }
func UnescapeChat(s string) string       { return cliUnescapeChat(s) }
func MarshalStrings(xs []string) (string, error) {
	return jsonMarshalStrings(xs)
}
func UUIDHex() string { return uuidHex() }

// Positional:cliParsed.positional 字段的只读访问器(域子包专用)。
func (p cliParsed) Positional() []string { return p.positional }

// WithPositional:返回 positional 被替换的值副本(域子包的改写形,
// email 的 `contacts` 无子命令垫片用)。
func (p cliParsed) WithPositional(pos []string) cliParsed {
	p.positional = pos
	return p
}

// FlagsMap:cliParsed.flags 的整表只读访问(域子包遍历形)。
func (p cliParsed) FlagsMap() map[string]any { return p.flags }

// OKWithEffects:带副作用的成功结果构造(域子包的 Result 字面量等价)。
func OKWithEffects(text string, sides []CliSideEffect) Result {
	return cliResult{ok: true, text: text, exitCode: 0, sideEffects: sides}
}

// FlagValue:cliParsed.flags 的按键取值(域子包的 `v, ok :=` 形)。
func (p cliParsed) FlagValue(key string) (any, bool) {
	v, ok := p.flags[key]
	return v, ok
}

// PublishRaw / AgentCompany:内核同名私有方法的导出包装(域子包经
// Domain 嵌入只能触达导出方法)。
func (s *Service) PublishRaw(channel string, payload []byte) error {
	return s.publishRaw(channel, payload)
}

func (s *Service) AgentCompany(ctx context.Context, agentID string) (string, error) {
	return s.cliAgentCompany(ctx, agentID)
}

// TryClaimTenantWork:租户级工作 claim(cli_reply 定义,avatar/doc/
// calendar 域共用)的导出包装。
func (s *Service) TryClaimTenantWork(companyID, agentID, taskType, subject string) *cliResult {
	return s.cliTryClaimTenantWork(companyID, agentID, taskType, subject)
}

// ISOTime / TimeOf:cliISOTime(扫库时间型)与 timeOf 转换的导出面
// (timeOf 原居 cli_boards,cli_doc 同用 → 归内核,boards 经此调用)。
type ISOTime = cliISOTime

func TimeOf(t ISOTime) time.Time { return timeOf(t) }

func timeOf(t ISOTime) time.Time { return time.Time(t) }

// ResolveAs / PositionalFrom / ErrThrow:身份与参数件(s.cliAgentCompany
// 等 Service 方法经 Domain 嵌入直接可达,无需包装)。
func ResolveAs(p Parsed) (string, error) { return cliResolveAs(p) }

func PositionalFrom(p Parsed, from int) []string { return positionalFrom(p, from) }

func ErrThrow(e error) Result { return cliErrThrow(e) }

// JSONStringify / JSONList / MarshalOrdered:JSON 序列化件。
func JSONStringify(v any) (string, error)    { return cliJSONStringify(v) }
func JSONList[T any](xs []T) (string, error) { return cliJSONList(xs) }
func MarshalOrdered(v any) ([]byte, error)   { return jsonMarshalOrdered(v) }

// ISOMilli / NodeDateToString / JSFloorNumber:时间与数值件。
func ISOMilli(t time.Time) string         { return isoMilli(t) }
func NodeDateToString(t time.Time) string { return nodeDateToString(t) }
func JSFloorNumber(v any) (int, bool)     { return jsFloorNumber(v) }
func JSFloor(f float64) float64           { return floorJS(f) }
func ISONowMs() string                    { return isoNowMs() }

/* ───────── 共享件归位(#140 4/9:原居 cli_mailbox,内核文件同用)───────── */

// isoMilli:JS Date.toISOString()(UTC, 毫秒 3 位)。
func isoMilli(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ParseJSDate:JS `new Date(s)` 的常用子集(ISO 8601 两种、日期、
// 日期+空格时间、时间无秒)。时区缺省按本地时区(JS 同)。
func ParseJSDate(s string) (time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04",
		"2006-01-02", "2006-01-02T15:04:05.999999999",
		// PG timestamptz ::text 形态("... 05:05:09.123+00" / 无毫秒变体)
		"2006-01-02 15:04:05.999999999-07", "2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05-07:00",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func containsString(list cliStrArr, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Poll / Attachment / RawJSON:内核消息行类型的导出别名(域子包行结构用)。
type Poll = cliPoll
type Attachment = cliAttachment
type RawJSON = cliRawJSON

// StoragePut / RandHex32 / ExtToMime / MsgFlagNum / UTF16Len /
// NewJSONEncoderNoEscape / NodeHM / JSONUnmarshal:内核散件导出包装。
func StoragePut(key string, body []byte) (string, error) { return cliStoragePut(key, body) }
func RandHex32() string                                  { return randHex32() }
func ExtToMime(ext string) string                        { return extToMime(ext) }
func MsgFlagNum(p Parsed, key string, def, max int) (int, error) {
	return cliMsgFlagNum(p, key, def, max)
}
func UTF16Len(s string) int { return utf16Len(s) }
func NewJSONEncoderNoEscape(w *bytes.Buffer) *json.Encoder {
	return newJSONEncoderNoEscape(w)
}
func NodeHM(t time.Time) string                 { return nodeHM(t) }
func JSONUnmarshal(b []byte, v any) error       { return json.Unmarshal(b, v) }
func ContainsString(list StrArr, s string) bool { return containsString(list, s) }
func MustJSON(v any) []byte                     { return jsonx.MustJSON(v) }
func MsSince(t0 time.Time) int64                { return msSince(t0) }

// Present:cliPoll.present 字段的只读访问器(域子包判 poll 有效)。
func (p cliPoll) Present() bool { return p.present }

// RenderAttachment / RenderPollForMessage:渲染件(pollPayload 不出内核,
// 经 cliPoll 包装;域子包不触未导出字段)。
func RenderAttachment(att Attachment) string { return renderAttachment(att) }
func RenderPollForMessage(messageID string, p Poll) []string {
	return renderPollBlock(messageID, p.parsed)
}

// PollPayload / PollOption:poll 消息模型(pollPayload 居 cli_read,
// cli_poll 命令面与读路径共用)的导出别名。
type PollPayload = pollPayload
type PollOption = cliPollOption

// JSONString / CompactJSON:内核 JSON 文本件导出包装。
func JSONString(s string) []byte { return cliJSONString(s) }
func CompactJSON(v any) string   { return compactJSON(v) }

// jsFloorNumber:JS Math.floor(Number(v)) 语义;不可解析返回 valid=false。
func jsFloorNumber(v any) (int, bool) {
	switch t := v.(type) {
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0, false
		}
		return int(floorJS(f)), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func floorJS(f float64) float64 {
	if f < 0 {
		return -float64(int(-f))
	}
	return float64(int(f))
}

func msSince(t0 time.Time) int64 { return time.Since(t0).Milliseconds() }

// SupportModelEnv:OPENAI_MODEL_SUPPORT(TS 缺省 gpt-5.4-mini)。
func SupportModelEnv() string { return config.OpenAIModelSupport() }

// hashStrJS:FNV-1a 32 位,按 UTF-16 code unit(charCodeAt 语义)。
func hashStrJS(s string) uint32 {
	h := uint32(2166136261)
	for _, u := range utf16.Encode([]rune(s)) {
		h ^= uint32(u)
		h *= 16777619
	}
	return h
}

// AgentAttachment:agentAttachment(生成图像附件,storage 面)的导出别名。
type AgentAttachment = agentAttachment

// EventsPublishMessageNew / StripLoneSurrogates / DerefStr / ImageModelEnv /
// HashStrJS:内核散件导出包装。
func EventsPublishMessageNew(ctx context.Context, companyID *string, convID string, msg map[string]any) {
	eventsPublishMessageNew(ctx, companyID, convID, msg)
}
func StripLoneSurrogates(s string) string { return cliStripLoneSurrogates(s) }
func HashStrJS(s string) uint32           { return hashStrJS(s) }
func DerefStr(p *string) string           { return derefStr(p) }
func ImageModelEnv() string               { return imageModelEnv() }

// ImagesGenerate / GenerateAndUploadImage:图像生成内核方法(cli_llm 定义)
// 的导出包装(avatar 域共用)。
func (s *Service) ImagesGenerate(ctx context.Context, tenant, model, prompt, size string) ([]byte, error) {
	return s.cliImagesGenerate(ctx, tenant, model, prompt, size)
}

func (s *Service) GenerateAndUploadImage(prompt, size, tenant, agentID string) (AgentAttachment, error) {
	return s.cliGenerateAndUploadImage(prompt, size, tenant, agentID)
}

// NodeLocaleString:时间件导出包装。
func NodeLocaleString(t time.Time) string { return nodeLocaleString(t) }

// HTTPPrefixRe:http(s) 前缀正则(skills/avatar 两域共用)。
var HTTPPrefixRe = regexp.MustCompile(`^https?://`)

func cliJSONString(s string) []byte {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString("\\\"")
		case '\\':
			sb.WriteString("\\\\")
		case '\n':
			sb.WriteString("\\n")
		case '\r':
			sb.WriteString("\\r")
		case '\t':
			sb.WriteString("\\t")
		default:
			if r < 0x20 {
				fmt.Fprintf(&sb, "\\u%04x", r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return []byte(sb.String())
}

// Without:cliStrArr.without(去首个同值成员,不动原切片)的导出包装。
func (a StrArr) Without(s string) StrArr { return a.without(s) }
