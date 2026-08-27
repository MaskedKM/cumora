// domains/uploads —— 上传域(#77):presign 与 refresh-url。Go storage seam
// 现为本地模式(cli_storage.go 同底座),R2 未接——presign 恒走 TS 本地
// 模式的 501 分支(该分支先于 name/mime/size 校验);refresh-url 的键解析
// (normalizeStorageKey / storageKeyFromPublicUrl)与 publicUrl 完整落地,
// 也是 #94 延后的附件 URL freshen 的 storage 面。
package uploads

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// storageKeyPrefixes:storage.ts 的三前缀白名单。
var storageKeyPrefixes = []string{"attachments/", "email-attachments/", "avatars/"}

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
// '+' 不转空格)→ 去前导 / → 前缀白名单。解码失败返回 ""(TS catch → null)。
func NormalizeStorageKey(raw string) string {
	trimmed := strings.TrimSpace(raw)
	decoded, err := url.PathUnescape(strings.TrimLeft(stripQueryAndHash(trimmed), "/"))
	if err != nil {
		return ""
	}
	for _, p := range storageKeyPrefixes {
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

// Mount:/api/uploads 面(presign + refresh-url;base64 上传与 capabilities
// 不在本票清单)。
func Mount(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("POST /api/uploads/presign", presign(db))
	mux.HandleFunc("POST /api/uploads/refresh-url", refreshURL(db))
}

func requireCompany(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, bool) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return "", false
	}
	companyID, ok := httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return "", false
	}
	return companyID, true
}

func bodyString(body map[string]json.RawMessage, key string) string {
	var v any
	_ = json.Unmarshal(body[key], &v)
	s, _ := v.(string)
	return s
}

// presign:本地模式恒 501(TS storage.mode!=='r2' 分支——先于 body 校验,
// 本地模式下任何 body 都拿到同一条 501)。
func presign(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireCompany(w, r, db); !ok {
			return
		}
		httpx.WriteError(w, http.StatusNotImplemented,
			"presign not available in local mode — POST /uploads instead")
	}
}

// refreshURL:{url?, key?} → {key, url}。key 解析顺序:显式 key 走
// normalizeStorageKey,否则 url 走 storageKeyFromPublicUrl;都不成 →
// 400 'not a Cumora storage URL'。
func refreshURL(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireCompany(w, r, db); !ok {
			return
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		key := NormalizeStorageKey(bodyString(body, "key"))
		if key == "" {
			key = StorageKeyFromPublicUrl(bodyString(body, "url"))
		}
		if key == "" {
			httpx.WriteError(w, http.StatusBadRequest, "not a Cumora storage URL")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"key": key, "url": PublicUrl(key)})
	}
}
