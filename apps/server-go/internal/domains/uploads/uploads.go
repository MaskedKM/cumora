// domains/uploads —— 上传域(#77):presign 与 refresh-url。Go storage seam
// 现为本地模式,R2 未接——presign 恒走 TS 本地模式的 501 分支(该分支
// 先于 name/mime/size 校验);键解析助手在 internal/storage 共享包
// (与 runtime 附件 freshen 同源,等价 TS storage.ts 双侧共享,#77 评审 MINOR2)。
package uploads

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/storage"
)

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
		key := storage.NormalizeStorageKey(bodyString(body, "key"))
		if key == "" {
			key = storage.StorageKeyFromPublicUrl(bodyString(body, "url"))
		}
		if key == "" {
			httpx.WriteError(w, http.StatusBadRequest, "not a Cumora storage URL")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"key": key, "url": storage.PublicUrl(key)})
	}
}
