// agent 包 cli_kernel —— 域子包(agent/<domain>)专用的内核管道导出面
// (#140 刀法):cli 解析/结果/UTF-16 助手以别名+包装函数导出,内核内部
// 名零改动,移出的域文件只做符号限定。
package agent

import (
	"context"
	"time"
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
