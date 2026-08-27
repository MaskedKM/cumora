// Package admin —— /api/admin 面(#112):requireAdmin 门(users.is_admin,
// 401 'authentication required' / 403 'admin only')+ settings 读写(两个
// 开关 + 六个 Cerebellum Route 字段;API key AES-256-GCM 落库、只回
// {configured, suffix} 永不回明文,ADR 0001)+ /me 门探 + 在线 computer
// 引擎并集。逐段对齐 server/src/api/admin-router.ts + cerebellum-settings.ts
// + admin.ts(requireAdmin/getSettings/setSetting);users/waitlist/stats/
// observability-llm 子面留待完整化票(本部署单管理员,settings+engines
// 为配对页与路由开关的刚需)。
package admin

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func Mount(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("GET /api/admin/me", me(db))
	mux.HandleFunc("GET /api/admin/settings", settingsGet(db))
	mux.HandleFunc("PUT /api/admin/settings", settingsPut(db))
	mux.HandleFunc("GET /api/admin/computers/available-engines", engines(db))
}

// requireAdmin:门语义逐字对齐 admin.ts(401 匿名 / 403 非管理员)。
func requireAdmin(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, bool) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return "", false
	}
	var isAdmin bool
	if err := db.QueryRowContext(r.Context(),
		`SELECT is_admin FROM users WHERE id = $1`, uid).Scan(&isAdmin); err != nil || !isAdmin {
		httpx.WriteError(w, http.StatusForbidden, "admin only")
		return "", false
	}
	return uid, true
}

func me(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := requireAdmin(w, r, db)
		if !ok {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"userId": uid, "isAdmin": true})
	}
}

// ---- app_settings 读写 ----

func getSettingBool(db *sql.DB, key string) bool {
	var v []byte
	if db.QueryRow(`SELECT value FROM app_settings WHERE key = $1`, key).Scan(&v) != nil {
		return false
	}
	return string(v) == "true"
}

func setSettingJSON(w http.ResponseWriter, r *http.Request, db *sql.DB, key, valJSON, updatedBy string) bool {
	_, err := db.ExecContext(r.Context(), `
		INSERT INTO app_settings (key, value, updated_at, updated_by)
		  VALUES ($1, $2::jsonb, NOW(), $3)
		  ON CONFLICT (key) DO UPDATE
		    SET value = EXCLUDED.value, updated_at = NOW(), updated_by = EXCLUDED.updated_by`,
		key, valJSON, updatedBy)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "settings write failed")
		return false
	}
	return true
}

// ---- Cerebellum AES-256-GCM(密钥方案与 runtime 包解密侧同源:
// key=sha256(CUMORA_SECRETS_KEY),存储 `iv.tag.ct` 各 base64)----

func secretsKeyRaw() string {
	return os.Getenv("CUMORA_SECRETS_KEY")
}

func secretsKeySum() [32]byte {
	return sha256.Sum256([]byte(secretsKeyRaw()))
}

func encryptApiKey(plaintext string) (string, bool) {
	raw := secretsKeyRaw()
	if raw == "" {
		return "", false
	}
	key := secretsKeySum()
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", false
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", false
	}
	ct := gcm.Seal(nil, iv, []byte(plaintext), nil)
	tag := ct[len(ct)-gcm.Overhead():]
	body := ct[:len(ct)-gcm.Overhead()]
	b64 := base64.StdEncoding.EncodeToString
	return b64(iv) + "." + b64(tag) + "." + b64(body), true
}

func decryptApiKey(stored string) string {
	parts := strings.Split(stored, ".")
	if len(parts) != 3 {
		return ""
	}
	raw := secretsKeyRaw()
	if raw == "" {
		return ""
	}
	key := secretsKeySum()
	iv, err1 := base64.StdEncoding.DecodeString(parts[0])
	tag, err2 := base64.StdEncoding.DecodeString(parts[1])
	ct, err3 := base64.StdEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return ""
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return ""
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ""
	}
	out, err := gcm.Open(nil, iv, append(ct, tag...), nil)
	if err != nil {
		return ""
	}
	return string(out)
}

// apiKeyStatus:管理员可见的唯一读取形状(ADR 0001/issue #22)。
func apiKeyStatus(db *sql.DB) (configured bool, suffix any) {
	var stored []byte
	if db.QueryRow(`SELECT value FROM app_settings WHERE key = 'cerebellum_api_key' LIMIT 1`).Scan(&stored) != nil {
		return false, nil
	}
	var enc string
	if json.Unmarshal(stored, &enc) != nil || enc == "" {
		return false, nil
	}
	plain := decryptApiKey(enc)
	if plain == "" {
		return false, nil
	}
	runes := []rune(plain)
	if len(runes) > 4 {
		runes = runes[len(runes)-4:]
	}
	return true, string(runes)
}

// ---- 响应组装 ----

type cerebellumPlain struct {
	route, localEngine, provider, baseURL, model string
}

func cerebellumRead(db *sql.DB) cerebellumPlain {
	out := cerebellumPlain{route: "remote", localEngine: "claude"}
	rows, err := db.Query(`SELECT key, value FROM app_settings WHERE key = ANY($1::text[])`,
		[]string{"cerebellum_route", "cerebellum_local_engine", "cerebellum_provider", "cerebellum_base_url", "cerebellum_model"})
	if err != nil {
		return out
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k string
		var v []byte
		if rows.Scan(&k, &v) == nil {
			var s string
			if json.Unmarshal(v, &s) == nil {
				m[k] = s
			}
		}
	}
	if m["cerebellum_route"] == "byoa" || m["cerebellum_route"] == "remote" {
		out.route = m["cerebellum_route"]
	}
	if m["cerebellum_local_engine"] != "" {
		out.localEngine = m["cerebellum_local_engine"]
	}
	out.provider = m["cerebellum_provider"]
	out.baseURL = strings.TrimRight(m["cerebellum_base_url"], "/")
	out.model = m["cerebellum_model"]
	return out
}

func buildSettingsResponse(w http.ResponseWriter, db *sql.DB) {
	c := cerebellumRead(db)
	configured, suffix := apiKeyStatus(db)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"waitlist_enabled":              getSettingBool(db, "waitlist_enabled"),
		"signups_paused":                getSettingBool(db, "signups_paused"),
		"cerebellum_route":              c.route,
		"cerebellum_local_engine":       c.localEngine,
		"cerebellum_provider":           c.provider,
		"cerebellum_base_url":           c.baseURL,
		"cerebellum_model":              c.model,
		"cerebellum_api_key_configured": configured,
		"cerebellum_api_key_suffix":     suffix,
	})
}

func settingsGet(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		buildSettingsResponse(w, db)
	}
}

// settingsPut:类型门部分更新(对齐 admin-router.ts 113–137):
// 布尔开关仅认 boolean;route 仅认 remote|byoa;其余字段仅认 string;
// cerebellum_api_key 是 string 即触发写(空串=显式清除,缺键=不动);
// 全空更新集 → 400 'no settings to update'。
func settingsPut(db *sql.DB) http.HandlerFunc {
	upsert := func(w http.ResponseWriter, r *http.Request, db *sql.DB, key, valJSON, uid string) bool {
		return setSettingJSON(w, r, db, key, valJSON, uid)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := requireAdmin(w, r, db)
		if !ok {
			return
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		hasUpdate := false
		strField := func(key, settingsKey string) {
			raw, present := body[key]
			if !present {
				return
			}
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return
			}
			hasUpdate = true
			if settingsKey != "" {
				_ = upsert(w, r, db, settingsKey, mustJSON(s), uid)
			}
		}
		if raw, present := body["waitlist_enabled"]; present {
			var b bool
			if json.Unmarshal(raw, &b) == nil {
				hasUpdate = true
				_ = upsert(w, r, db, "waitlist_enabled", mustJSON(b), uid)
			}
		}
		if raw, present := body["signups_paused"]; present {
			var b bool
			if json.Unmarshal(raw, &b) == nil {
				hasUpdate = true
				_ = upsert(w, r, db, "signups_paused", mustJSON(b), uid)
			}
		}
		if raw, present := body["cerebellum_route"]; present {
			var s string
			if json.Unmarshal(raw, &s) == nil && (s == "remote" || s == "byoa") {
				hasUpdate = true
				_ = upsert(w, r, db, "cerebellum_route", mustJSON(s), uid)
			}
		}
		strField("cerebellum_local_engine", "cerebellum_local_engine")
		strField("cerebellum_provider", "cerebellum_provider")
		strField("cerebellum_base_url", "cerebellum_base_url")
		strField("cerebellum_model", "cerebellum_model")
		if raw, present := body["cerebellum_api_key"]; present {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				hasUpdate = true
				if s == "" {
					_, _ = db.ExecContext(r.Context(), `DELETE FROM app_settings WHERE key = 'cerebellum_api_key'`)
				} else if enc, ok := encryptApiKey(s); ok {
					_ = upsert(w, r, db, "cerebellum_api_key", mustJSON(enc), uid)
				}
			}
		}
		if !hasUpdate {
			httpx.WriteError(w, http.StatusBadRequest, "no settings to update")
			return
		}
		buildSettingsResponse(w, db)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// engines:在线且未吊销 computer 的 available_engines 并集,字典序。
func engines(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		rows, err := db.QueryContext(r.Context(),
			`SELECT available_engines FROM computers WHERE status = 'online' AND revoked_at IS NULL`)
		if err != nil {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"engines": []string{}})
			return
		}
		defer rows.Close()
		union := map[string]bool{}
		for rows.Next() {
			var raw []byte
			if rows.Scan(&raw) != nil {
				continue
			}
			var list []string
			if json.Unmarshal(raw, &list) != nil {
				continue
			}
			for _, e := range list {
				union[e] = true
			}
		}
		out := make([]string, 0, len(union))
		for e := range union {
			out = append(out, e)
		}
		sort.Strings(out)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"engines": out})
	}
}
