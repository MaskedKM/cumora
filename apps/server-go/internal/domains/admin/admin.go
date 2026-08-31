// Package admin —— /api/admin 面(#112):requireAdmin 门(users.is_admin,
// 401 'authentication required' / 403 'admin only')+ settings 读写(两个
// 开关 + 六个 Cerebellum Route 字段;API key AES-256-GCM 落库、只回
// {configured, suffix} 永不回明文,ADR 0001)+ /me 门探 + 在线 computer
// 引擎并集。逐段对齐 已退役 TS server 的 api/admin-router.ts + cerebellum-settings.ts
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

	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/admin"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// Server:admin tag(13 路由)的域实现(#187 机械迁移,方法散在
// 本包各文件)。
type Server struct{ DB *sql.DB }

var _ contract.ServerInterface = (*Server)(nil)

func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = contract.HandlerFromMux(&Server{DB: db}, mux)
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

func (s *Server) AdminMe(w http.ResponseWriter, r *http.Request) {
	uid, ok := requireAdmin(w, r, s.DB)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"userId": uid, "isAdmin": true})
}

// ---- app_settings 读写 ----

// getSettingBool:缺行=false 缺省;查询错误上抛(TS throw → 500,
// 绝不用假缺省值 200 糊弄——管理员会把假值存回去)。
func getSettingBool(db *sql.DB, key string) (bool, error) {
	var v []byte
	err := db.QueryRow(`SELECT value FROM app_settings WHERE key = $1`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return string(v) == "true", nil
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

// apiKeyStatus:管理员可见的唯一读取形状(ADR 0001/issue #22)。查询
// 错误按未配置呈现(TS:任何读失败 → configured false,不 500——
// decryptApiKey 的 null-on-failure 语义延伸到行读取)。
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

// cerebellumRead:读侧归一(route 门/localEngine 非空缺省/baseUrl 去
// 尾斜杠)逐字对齐 cerebellum-settings.ts;查询错误上抛。
func cerebellumRead(db *sql.DB) (cerebellumPlain, error) {
	out := cerebellumPlain{route: "remote", localEngine: "claude"}
	rows, err := db.Query(`SELECT key, value FROM app_settings WHERE key = ANY($1::text[])`,
		[]string{"cerebellum_route", "cerebellum_local_engine", "cerebellum_provider", "cerebellum_base_url", "cerebellum_model"})
	if err != nil {
		return out, err
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
	return out, nil
}

func buildSettingsResponse(w http.ResponseWriter, db *sql.DB) {
	c, err := cerebellumRead(db)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	waitlist, err := getSettingBool(db, "waitlist_enabled")
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	signups, err := getSettingBool(db, "signups_paused")
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	configured, suffix := apiKeyStatus(db)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"waitlist_enabled":              waitlist,
		"signups_paused":                signups,
		"cerebellum_route":              c.route,
		"cerebellum_local_engine":       c.localEngine,
		"cerebellum_provider":           c.provider,
		"cerebellum_base_url":           c.baseURL,
		"cerebellum_model":              c.model,
		"cerebellum_api_key_configured": configured,
		"cerebellum_api_key_suffix":     suffix,
	})
}

func (s *Server) AdminGetSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r, s.DB); !ok {
		return
	}
	buildSettingsResponse(w, s.DB)
}

// settingsPut:类型门部分更新(对齐 admin-router.ts 113–137):
// 布尔开关仅认 boolean;route 仅认 remote|byoa;其余字段仅认 string;
// **JSON null 一律忽略**(TS typeof 门语义;null 直通 Go unmarshal 会
// 被当零值放行,cerebellum_api_key:null 曾静默删密钥);
// cerebellum_api_key 是 string 即触发写(空串=显式清除,缺键=不动);
// 加密失败(CUMORA_SECRETS_KEY 缺)→ 500 绝不装成功;
// 全空更新集 → 400 'no settings to update'。
func (s *Server) AdminPutSettings(w http.ResponseWriter, r *http.Request) {
	uid, ok := requireAdmin(w, r, s.DB)
	if !ok {
		return
	}
	// 空体按 {} 处理(TS express.json 语义)→ 落到 400 no settings;
	// 坏 JSON 才 400 invalid JSON body。
	body := map[string]json.RawMessage{}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil && err != io.EOF {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// TS 的门是 typeof(布尔/字符串);JSON null 在 Go unmarshal 里
	// 无错也不置值,会把 null 当 false/"" 放行 —— 须显式拒 null
	// (否则 cerebellum_api_key:null 会静默删掉已存密钥)。
	isNull := func(raw json.RawMessage) bool { return string(raw) == "null" }
	strOf := func(key string) (string, bool) {
		raw, present := body[key]
		if !present || isNull(raw) {
			return "", false
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return s, true
	}
	boolOf := func(key string) (bool, bool) {
		raw, present := body[key]
		if !present || isNull(raw) {
			return false, false
		}
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return false, false
		}
		return b, true
	}
	failed := false
	upsert := func(key, valJSON string) {
		if failed {
			return
		}
		if !setSettingJSON(w, r, s.DB, key, valJSON, uid) {
			failed = true
		}
	}
	hasUpdate := false
	if b, ok := boolOf("waitlist_enabled"); ok {
		hasUpdate = true
		upsert("waitlist_enabled", mustJSON(b))
	}
	if b, ok := boolOf("signups_paused"); ok {
		hasUpdate = true
		upsert("signups_paused", mustJSON(b))
	}
	if s, ok := strOf("cerebellum_route"); ok && (s == "remote" || s == "byoa") {
		hasUpdate = true
		upsert("cerebellum_route", mustJSON(s))
	}
	for _, k := range []string{"cerebellum_local_engine", "cerebellum_provider", "cerebellum_base_url", "cerebellum_model"} {
		if s, ok := strOf(k); ok {
			hasUpdate = true
			upsert(k, mustJSON(s))
		}
	}
	if skey, ok := strOf("cerebellum_api_key"); ok {
		hasUpdate = true
		switch {
		case skey == "":
			_, _ = s.DB.ExecContext(r.Context(), `DELETE FROM app_settings WHERE key = 'cerebellum_api_key'`)
		default:
			// 加密失败 = 服务器缺 CUMORA_SECRETS_KEY(TS:throw → 500),
			// 绝不能装作成功。
			enc, encOK := encryptApiKey(skey)
			if !encOK {
				httpx.WriteError(w, http.StatusInternalServerError, "CUMORA_SECRETS_KEY is not configured on the server")
				return
			}
			upsert("cerebellum_api_key", mustJSON(enc))
		}
	}
	if failed {
		return // upsert 已写 500
	}
	if !hasUpdate {
		httpx.WriteError(w, http.StatusBadRequest, "no settings to update")
		return
	}
	buildSettingsResponse(w, s.DB)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// engines:在线且未吊销 computer 的 available_engines 并集,字典序。
func (s *Server) AdminAvailableEngines(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r, s.DB); !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(),
		`SELECT available_engines FROM computers WHERE status = 'online' AND revoked_at IS NULL`)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
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
