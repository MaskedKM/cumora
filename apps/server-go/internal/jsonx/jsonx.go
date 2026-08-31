// Package jsonx —— JSON 序列化小助手的唯一实现(#267 收敛)。
// 收敛前同形助手散在五处且错误语义不一:agent/presence 失败兜底 "{}"、
// conversations/admin/shipping 静默返回 nil/""。nil/"" 会被调用方直接
// 写进 WS 帧/HTTP 响应产出非法 JSON,故统一取 "{}" 兜底。
// Unmarshal 不设包装——各处直接用 encoding/json(#267 删除三份委托副本)。
package jsonx

import "encoding/json"

// MustJSON:marshal,失败兜底 "{}"(保持合法 JSON 形状)。
func MustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// MustJSONString:string 形态(admin/shipping 原各自实现的统一)。
func MustJSONString(v any) string {
	return string(MustJSON(v))
}
