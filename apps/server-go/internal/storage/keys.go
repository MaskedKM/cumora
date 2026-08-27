// storage —— 存储键语义共享包(#77 评审 MINOR2:router 的 uploads 面与
// runtime 的附件 freshen 共用同一套键解析,等价 TS storage.ts 被两侧
// 共享的结构;R2 presign 接入时也落这里)。
package storage

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// KeyPrefixes:storage.ts 的三前缀白名单。
var KeyPrefixes = []string{"attachments/", "email-attachments/", "avatars/"}

func stripQueryAndHash(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if i := strings.IndexByte(path, '#'); i >= 0 {
		path = path[:i]
	}
	return path
}

// NormalizeStorageKey:trim → 去 query/hash → decodeURIComponent(PathUnescape,
// '+' 不转空格;非法 UTF-8[孤立代理对]判无效=TS decodeURIComponent 抛
// → null)→ 去前导 / → 前缀白名单。
func NormalizeStorageKey(raw string) string {
	trimmed := strings.TrimSpace(raw)
	decoded, err := url.PathUnescape(strings.TrimLeft(stripQueryAndHash(trimmed), "/"))
	if err != nil || !utf8.ValidString(decoded) {
		return ""
	}
	for _, p := range KeyPrefixes {
		if strings.HasPrefix(decoded, p) {
			return decoded
		}
	}
	return ""
}

// StorageKeyFromPublicUrl:/uploads/<key> 短 URL → key;R2 公网基座未配置
// 时其余形态返回 ""(TS env.R2_PUBLIC_BASE 缺省同判)。
func StorageKeyFromPublicUrl(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "/uploads/") {
		return NormalizeStorageKey(strings.TrimPrefix(value, "/uploads/"))
	}
	return ""
}

// PublicUrl:本地模式 storage.publicUrl —— /uploads/<key>。
func PublicUrl(key string) string { return "/uploads/" + key }
