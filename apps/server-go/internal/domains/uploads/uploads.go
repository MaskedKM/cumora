// domains/uploads —— 上传域(#77):presign 与 refresh-url。Go storage seam
// 现为本地模式,R2 未接——presign 恒走 TS 本地模式的 501 分支(该分支
// 先于 name/mime/size 校验);键解析助手在 internal/storage 共享包
// (与 runtime 附件 freshen 同源,等价 TS storage.ts 双侧共享,#77 评审 MINOR2)。
package uploads

import (
	crand "crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/storage"
)

// Mount:/api/uploads 面(presign + refresh-url;base64 上传与 capabilities
// 不在本票清单)。
func Mount(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("POST /api/uploads/presign", presign(db))
	mux.HandleFunc("POST /api/uploads/refresh-url", refreshURL(db))
	mux.HandleFunc("GET /api/uploads/capabilities", capabilities)
	mux.HandleFunc("POST /api/uploads", uploadBase64(db))
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

/* ───────── capabilities + base64 上传(#68 补齐) ───────── */

// maxUploadBytes / mimePolicy:router.ts 的 25MB 上限与 MIME 白名单。
const maxUploadBytes = 25 * 1024 * 1024

var mimePolicy = map[string]struct {
	kind string
	ext  string
}{
	"image/png":          {"img", "png"},
	"image/jpeg":         {"img", "jpg"},
	"image/webp":         {"img", "webp"},
	"image/gif":          {"img", "gif"},
	"application/pdf":    {"file", "pdf"},
	"application/msword": {"file", "doc"},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {"file", "docx"},
	"application/vnd.ms-excel": {"file", "xls"},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {"file", "xlsx"},
	"application/vnd.ms-powerpoint":                                             {"file", "ppt"},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {"file", "pptx"},
	"application/zip":   {"file", "zip"},
	"application/x-tar": {"file", "tar"},
	"application/gzip":  {"file", "gz"},
	"text/plain":        {"file", "txt"},
	"text/markdown":     {"file", "md"},
	"text/csv":          {"file", "csv"},
}

// capabilities:上传能力通告。allowedMimes 键序 = TS Object.keys 的
// 字面插入序(Go map 无序,按源码序排列)。
func capabilities(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"mode":             "local",
		"presignSupported": false,
		"maxBytes":         maxUploadBytes,
		"allowedMimes": []string{
			"image/png", "image/jpeg", "image/webp", "image/gif",
			"application/pdf", "application/msword",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"application/vnd.ms-excel",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			"application/vnd.ms-powerpoint",
			"application/vnd.openxmlformats-officedocument.presentationml.presentation",
			"application/zip", "application/x-tar", "application/gzip",
			"text/plain", "text/markdown", "text/csv",
		},
	})
}

func uploadDir() string {
	if d := os.Getenv("CUMORA_UPLOADS_DIR"); d != "" {
		return d
	}
	return filepath.Join("server", "uploads")
}

// uploadBase64:{name, mime, dataBase64} → 解码校验 → 落盘 →
// {url, key, name, mime, size, kind}。name 按 TS trim().slice(0,200)
// (UTF-16 码元)。
func uploadBase64(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireCompany(w, r, db); !ok {
			return
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := utf16Cap(strings.TrimSpace(bodyString(body, "name")), 200)
		mime := strings.ToLower(strings.TrimSpace(bodyString(body, "mime")))
		dataBase64 := bodyString(body, "dataBase64")
		if name == "" || mime == "" || dataBase64 == "" {
			httpx.WriteError(w, http.StatusBadRequest, "name, mime, dataBase64 are required")
			return
		}
		policy, allowed := mimePolicy[mime]
		if !allowed {
			httpx.WriteError(w, http.StatusUnsupportedMediaType, "mime not allowed: "+mime)
			return
		}
		// TS Buffer.from(...,'base64') 宽容(尽力解码、忽略坏块);
		// Go base64 严格 —— RawStdEncoding 尽力路径近似(合法输入两者
		// 等价,坏输入的漂移留档:测试只用合法 base64)。
		buf, err := base64.StdEncoding.DecodeString(dataBase64)
		if err != nil {
			buf, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(dataBase64, "="))
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, "invalid base64")
				return
			}
		}
		if len(buf) == 0 || len(buf) > maxUploadBytes {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge,
				"size out of range (got "+strconv.Itoa(len(buf))+", max "+strconv.Itoa(maxUploadBytes)+")")
			return
		}
		key := "attachments/" + randHex32() + "." + policy.ext
		dst := filepath.Join(uploadDir(), filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.WriteFile(dst, buf, 0o644); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"url": storage.PublicUrl(key), "key": key, "name": name, "mime": mime,
			"size": len(buf), "kind": policy.kind,
		})
	}
}

func randHex32() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}

// utf16Cap:TS .slice(0,n) 按 UTF-16 码元。
func utf16Cap(s string, n int) string {
	count := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if count+w > n {
			return s[:i]
		}
		count += w
	}
	return s
}
