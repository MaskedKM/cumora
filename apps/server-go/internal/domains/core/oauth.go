// domains/core/oauth —— /api/auth/start|callback OAuth 登录流(#109):
// provider 表驱动(google/github),state 铸造/单次消费走 Redis(仅存
// sha256,5min TTL,GETDEL),回调换码取档后三路 find-or-create(已链 →
// 跨链补绑 → 新建+个人区),会话 token 走 URL fragment 重定向(绝不落
// query/日志)。逐段对齐 已退役 TS server 的 oauth.ts;env 直读与
// CUMORA_OAUTH_<P>_BASE 覆盖同 TS 侧(metrics 直读先例,双跑同桩)。
package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/core"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/onboard"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
)

type oauthDeps struct {
	db  *sql.DB
	rdb *redis.Client
}

var oauthHTTP = &http.Client{Timeout: 10 * time.Second}

// ---- provider 配置 ----

type oauthProviderCfg struct {
	authorizeURL string
	tokenURL     string
	userInfoURL  string
	emailsURL    string // github 专用;google 为空
	scope        string
	clientID     string
	clientSecret string
}

func oauthProviderConfig(p string) oauthProviderCfg {
	if p == "google" {
		authz, token, user := "https://accounts.google.com", "https://oauth2.googleapis.com", "https://openidconnect.googleapis.com"
		if base := config.OAuthGoogleBase(); base != "" {
			authz, token, user = base, base, base
		}
		return oauthProviderCfg{
			authorizeURL: authz + "/o/oauth2/v2/auth",
			tokenURL:     token + "/token",
			userInfoURL:  user + "/v1/userinfo",
			scope:        "openid email profile",
			clientID:     config.GoogleClientID(),
			clientSecret: config.GoogleClientSecret(),
		}
	}
	gh, api := "https://github.com/login/oauth", "https://api.github.com"
	if base := config.OAuthGitHubBase(); base != "" {
		gh, api = base, base
	}
	return oauthProviderCfg{
		authorizeURL: gh + "/authorize",
		tokenURL:     gh + "/access_token",
		userInfoURL:  api + "/user",
		emailsURL:    api + "/user/emails",
		scope:        "read:user user:email",
		clientID:     config.GitHubClientID(),
		clientSecret: config.GitHubClientSecret(),
	}
}

func oauthProviderEnabled(p string) bool {
	cfg := oauthProviderConfig(p)
	return cfg.clientID != "" && cfg.clientSecret != ""
}

func oauthPublicOrigin() string {
	origin := config.PublicOrigin()
	if origin == "" {
		origin = "http://localhost:5181"
	}
	return strings.TrimRight(origin, "/")
}

func oauthAuthDoneURL() string {
	if u := config.CumoraAuthDoneURL(); u != "" {
		return u
	}
	return "http://localhost:5173/"
}

func oauthRedirectURI(p string) string {
	return oauthPublicOrigin() + "/api/auth/callback/" + p
}

// returnUrlAllowed:白名单严格前缀(子路径可,源伪造不可)。TS 同名
// 直读 process.env;未设时按空白名单拒(生产经 systemd .env 注入)。
func oauthReturnURLAllowed(raw string) bool {
	if raw == "" {
		return false
	}
	for _, prefix := range strings.Split(config.AuthReturnAllowlist(), ",") {
		if prefix = strings.TrimSpace(prefix); prefix != "" && strings.HasPrefix(raw, prefix) {
			return true
		}
	}
	return false
}

// ---- state(Redis 单次消费) ----

type oauthStateData struct {
	Provider    string  `json:"provider"`
	ReturnURL   *string `json:"returnUrl"`
	InviteToken *string `json:"inviteToken"`
}

func oauthHashState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func oauthCreateState(ctx context.Context, rdb *redis.Client, provider string, returnURL, inviteToken *string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(b)
	data, _ := json.Marshal(oauthStateData{provider, returnURL, inviteToken})
	return state, rdb.Set(ctx, "oauth:state:"+oauthHashState(state), data, 300*time.Second).Err()
}

func oauthConsumeState(ctx context.Context, rdb *redis.Client, state string) (*oauthStateData, error) {
	v, err := rdb.GetDel(ctx, "oauth:state:"+oauthHashState(state)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil // 未知/过期/已消费 —— 与 TS 的 null 同义
	}
	if err != nil {
		return nil, err // Redis 故障:TS 会让它冒泡到 catch(login_failed 审计)
	}
	var data oauthStateData
	if json.Unmarshal([]byte(v), &data) != nil {
		return nil, nil
	}
	if data.Provider != "google" && data.Provider != "github" {
		return nil, nil
	}
	return &data, nil
}

func oauthAuthorizeURL(p, state string) string {
	cfg := oauthProviderConfig(p)
	// 与 fragment 同理:TS URLSearchParams 按插入序,google 的
	// prompt/access_type 恒在尾 —— 手工拼不用 Values.Encode(字母序)。
	pairs := [][2]string{
		{"client_id", cfg.clientID},
		{"redirect_uri", oauthRedirectURI(p)},
		{"response_type", "code"},
		{"scope", cfg.scope},
		{"state", state},
	}
	if p == "google" {
		// 恒 prompt=select_account:杜绝绕过账号选择器的静默重登。
		pairs = append(pairs, [2]string{"prompt", "select_account"}, [2]string{"access_type", "online"})
	}
	return cfg.authorizeURL + "?" + oauthFrag(pairs...)
}

// ---- 换码 + 取档 ----

type oauthProfile struct {
	providerID  string
	email       string // 已 lower
	displayName string
	avatarURL   string // 空串 = 无
}

func oauthExchangeCode(ctx context.Context, p, code string) (string, error) {
	cfg := oauthProviderConfig(p)
	form := url.Values{
		"client_id":     {cfg.clientID},
		"client_secret": {cfg.clientSecret},
		"code":          {code},
		"redirect_uri":  {oauthRedirectURI(p)},
		"grant_type":    {"authorization_code"},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	res, err := oauthHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s token exchange: %w", p, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("%s token exchange %d: %s", p, res.StatusCode, body)
	}
	var parsed struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(body, &parsed) != nil || parsed.AccessToken == "" {
		reason := parsed.ErrorDescription
		if reason == "" {
			reason = parsed.Error
		}
		if reason == "" {
			reason = "unknown"
		}
		return "", fmt.Errorf("%s token response missing access_token: %s", p, reason)
	}
	return parsed.AccessToken, nil
}

func oauthFetchJSON(ctx context.Context, u, accessToken string, hdr map[string]string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("authorization", "Bearer "+accessToken)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := oauthHTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// 裸状态码:调用方拼 "github user %w" → "github user 401",
		// 对齐 TS `${p} user ${status}`(不带 http 前缀)。
		return fmt.Errorf("%d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
}

func oauthFetchProfile(ctx context.Context, p, accessToken string) (*oauthProfile, error) {
	cfg := oauthProviderConfig(p)
	if p == "google" {
		var g struct {
			Sub           string `json:"sub"`
			Email         string `json:"email"`
			EmailVerified bool   `json:"email_verified"`
			Name          string `json:"name"`
			Picture       string `json:"picture"`
		}
		if err := oauthFetchJSON(ctx, cfg.userInfoURL, accessToken, nil, &g); err != nil {
			return nil, fmt.Errorf("google userinfo %w", err)
		}
		if g.Email == "" || !g.EmailVerified {
			return nil, errors.New("google account has no verified email")
		}
		name := strings.TrimSpace(g.Name)
		if name == "" {
			name = strings.SplitN(g.Email, "@", 2)[0]
		}
		return &oauthProfile{g.Sub, strings.ToLower(g.Email), name, g.Picture}, nil
	}
	// github:/user 常不回 email(隐私默认),双取 /user/emails 取
	// primary&&verified,退而求其次任意 verified —— verified 才 attest
	// 所有权,primary 只是 GitHub 的默认地址挑选。
	var u struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	hdr := map[string]string{"accept": "application/json", "user-agent": "cumora"}
	if err := oauthFetchJSON(ctx, cfg.userInfoURL, accessToken, hdr, &u); err != nil {
		return nil, fmt.Errorf("github user %w", err)
	}
	if err := oauthFetchJSON(ctx, cfg.emailsURL, accessToken, hdr, &emails); err != nil {
		return nil, fmt.Errorf("github emails %w", err)
	}
	verified := ""
	fallback := ""
	for _, e := range emails {
		if !e.Verified {
			continue
		}
		if e.Primary {
			verified = e.Email
			break
		}
		if fallback == "" {
			fallback = e.Email
		}
	}
	if verified == "" {
		verified = fallback
	}
	if verified == "" {
		return nil, errors.New("github account has no verified email")
	}
	name := strings.TrimSpace(u.Name)
	if name == "" {
		name = u.Login
	}
	return &oauthProfile{fmt.Sprintf("%d", u.ID), strings.ToLower(verified), name, u.AvatarURL}, nil
}

// MirrorAvatar:admin 域复用(approve 建号同款 provider 头像镜像)。
func MirrorAvatar(userID, providerURL string) string { return oauthMirrorAvatar(userID, providerURL) }

// oauthMirrorAvatar:拉 provider 头像转存本地(同源,免第三方 URL 轮换
// /CORS);任何失败回退原 URL,绝不阻断登录。mime 白名单 + 2MB 上限 +
// 5s 超时;本地上传模式即 storage.put 语义(uploads 根经 config.
// UploadsDir() 统一解析,#208:<key> → /uploads/<key>)。
func oauthMirrorAvatar(userID, providerURL string) string {
	if providerURL == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, providerURL, nil)
	req.Header.Set("user-agent", "cumora")
	res, err := oauthHTTP.Do(req)
	if err != nil {
		return providerURL
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return providerURL
	}
	mime := strings.ToLower(strings.TrimSpace(strings.SplitN(res.Header.Get("content-type"), ";", 2)[0]))
	ext := map[string]string{
		"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp", "image/gif": "gif",
	}[mime]
	if ext == "" {
		return providerURL
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024+1))
	if err != nil || len(body) > 2*1024*1024 {
		return providerURL
	}
	key := "avatars/" + userID + "." + ext
	full := filepath.Join(config.UploadsDir(), filepath.FromSlash(key))
	if os.MkdirAll(filepath.Dir(full), 0o755) != nil {
		return providerURL
	}
	if os.WriteFile(full, body, 0o644) != nil {
		return providerURL
	}
	return "/uploads/" + key
}

// ---- find-or-create(三路)+ finalize ----

type oauthWaitlistedError struct{ email, displayName string }

func (e *oauthWaitlistedError) Error() string { return "waitlisted: " + e.email }

type oauthSuspendedError struct {
	email  string
	reason string // 空串 = 无理由
}

func (e *oauthSuspendedError) Error() string { return "suspended: " + e.email }

type oauthCompletion struct {
	userID, email, displayName string
	companyID                  string // 空串 = 无区(invite 路径/存量无区)
}

func oauthIsAllowlistedAdmin(email string) bool {
	mine := strings.ToLower(strings.TrimSpace(email))
	for _, e := range config.AdminEmails() {
		if e == mine && mine != "" {
			return true
		}
	}
	return false
}

func oauthIsDupKey(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func oauthRandHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)[:n]
}

// oauthFindOrCreate:事务三路;Path C 尾段(建区)受 inviteToken 门控。
// sub2api 供给(#109 延后):本部署 SUB2API_* 未配置,门保留为 no-op。
func oauthFindOrCreate(ctx context.Context, db *sql.DB, p string, profile *oauthProfile, inviteToken *string) (*oauthCompletion, error) {
	// 事务豁免(#213,#235 复审仍留):三路分支两次 mid-body Commit
	// (Path A/B)、Path C 显式 Rollback 后改走 db(非 tx)写 waitlist,
	// 尾段另有 slug SAVEPOINT 嵌套,WithTx 单提交点无法表达。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Path A:身份已链 → 直接登录。
	var linked string
	err = tx.QueryRowContext(ctx,
		`SELECT user_id FROM user_identities WHERE provider = $1 AND provider_id = $2`,
		p, profile.providerID).Scan(&linked)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return oauthFinalize(ctx, db, linked, profile)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Path B:同 email 跨链补绑(provider 已 attest 所有权)。
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE LOWER(email) = $1 LIMIT 1`, profile.email).Scan(&existing)
	if err == nil {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_identities (provider, provider_id, user_id, email_lower)
			   VALUES ($1, $2, $3, $4)
			   ON CONFLICT (provider, provider_id) DO NOTHING`,
			p, profile.providerID, existing, profile.email); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return oauthFinalize(ctx, db, existing, profile)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Path C:新用户;等待名单开门且非白名单 admin → 入列不建号。
	var waitlistOn []byte
	_ = tx.QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key = 'waitlist_enabled' LIMIT 1`).Scan(&waitlistOn)
	if string(waitlistOn) == "true" && !oauthIsAllowlistedAdmin(profile.email) {
		_ = tx.Rollback()
		_, werr := db.ExecContext(ctx,
			`INSERT INTO waitlist (id, provider, provider_id, email, display_name, avatar_url)
			   VALUES ($1, $2, $3, $4, $5, NULLIF($6,''))
			   ON CONFLICT (provider, provider_id) DO UPDATE
			   SET email = EXCLUDED.email,
			       display_name = EXCLUDED.display_name,
			       avatar_url = EXCLUDED.avatar_url,
			       requested_at = NOW(),
			       status = CASE WHEN waitlist.status = 'rejected' THEN 'rejected' ELSE 'pending' END`,
			"wl-"+oauthRandHex(12), p, profile.providerID, profile.email, profile.displayName, profile.avatarURL)
		if werr != nil {
			return nil, werr // TS:入列抛错冒泡 → 调用方 login_failed
		}
		return nil, &oauthWaitlistedError{profile.email, profile.displayName}
	}

	userID := "u-" + oauthRandHex(12)
	admin := oauthIsAllowlistedAdmin(profile.email)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, password_hash, email_verified_at, is_admin, tier)
		   VALUES ($1, $2, $3, NULL, NOW(), $4, 'free')`,
		userID, profile.email, profile.displayName, admin); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_identities (provider, provider_id, user_id, email_lower)
		   VALUES ($1, $2, $3, $4)`,
		p, profile.providerID, userID, profile.email); err != nil {
		return nil, err
	}
	avatar := oauthMirrorAvatar(userID, profile.avatarURL)
	if avatar == "" {
		avatar = gravatarURL(profile.email)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET avatar_url = $1 WHERE id = $2`, avatar, userID); err != nil {
		return nil, err
	}

	companyID := ""
	if inviteToken == nil {
		companyID = "co-" + oauthRandHex(10)
		local := strings.SplitN(profile.email, "@", 2)[0]
		seed := oauthSlugSeed(local)
		slug := seed
		for attempt := 0; attempt < 3; attempt++ {
			// 唯一键冲突会 abort 整个事务,tx 内重试必死于 25P02(TS 同款
			// 潜伏 bug,#141 rider 修)——SAVEPOINT 包住单次 INSERT,
			// 冲突回滚到检查点再换 slug 重试。
			if _, spErr := tx.ExecContext(ctx, `SAVEPOINT slug_insert`); spErr != nil {
				return nil, spErr
			}
			_, err = tx.ExecContext(ctx,
				`INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
				companyID, profile.displayName+"'s team", slug, userID)
			if err == nil {
				if _, relErr := tx.ExecContext(ctx, `RELEASE SAVEPOINT slug_insert`); relErr != nil {
					return nil, relErr
				}
				break
			}
			if !oauthIsDupKey(err) {
				return nil, err
			}
			if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT slug_insert`); rbErr != nil {
				return nil, rbErr
			}
			slug = fmt.Sprintf("%s-%s", seed, oauthRandHex(4))
		}
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO company_members (company_id, user_id, role) VALUES ($1, $2, 'owner')`,
			companyID, userID); err != nil {
			return nil, err
		}
		// TS initial = displayName.charAt(0).toUpperCase()(UTF-16 首码元);
		// Go 取首 rune 大写 —— BMP 内等价,代理对极端不构成实际人名差异。
		initial := strings.ToUpper(oauthFirstRune(profile.displayName))
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO participants (id, kind, name, role, initial, avatar_bg, avatar_url, status, company_id)
			   VALUES ($1, 'human', $2, NULL, $3, '#FF8870', $4, 'avail', $5)
			   ON CONFLICT (id, company_id) DO NOTHING`,
			userID, profile.displayName, initial, avatar, companyID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// #all-hands 尽力而为 —— 绝不阻断首登(组未建即 no-op)。
	if companyID != "" {
		onboard.JoinAllHands(ctx, db, companyID, userID)
	}
	return &oauthCompletion{userID, profile.email, profile.displayName, companyID}, nil
}

// oauthSlugSeed:TS 的 localpart.replace(/[^a-z0-9]+/g,'-').slice(0,30)
// || 'workspace'(先折叠后截断,空则兜底)。
func oauthSlugSeed(local string) string {
	low := strings.ToLower(local)
	var b strings.Builder
	prevDash := false
	for _, c := range low {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	// TS 不 trim 首尾连字符("_foo_" → "-foo-"),仅截 30、空则 workspace。
	out := b.String()
	if len(out) > 30 {
		out = out[:30]
	}
	if out == "" {
		return "workspace"
	}
	return out
}

func oauthFirstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return "?"
}

// oauthFinalize:UPDATE last_login RETURNING 停用态(原子,无 TOCTOU),
// 再取最早加入的区。
func oauthFinalize(ctx context.Context, db *sql.DB, userID string, profile *oauthProfile) (*oauthCompletion, error) {
	var suspendedAt, suspendedReason sql.NullString
	if err := db.QueryRowContext(ctx,
		`UPDATE users SET last_login_at = NOW() WHERE id = $1
		  RETURNING suspended_at, suspension_reason`, userID).
		Scan(&suspendedAt, &suspendedReason); err != nil {
		return nil, err
	}
	if suspendedAt.Valid {
		return nil, &oauthSuspendedError{profile.email, suspendedReason.String}
	}
	var companyID sql.NullString
	_ = db.QueryRowContext(ctx,
		`SELECT company_id FROM company_members WHERE user_id = $1 ORDER BY joined_at ASC LIMIT 1`,
		userID).Scan(&companyID)
	return &oauthCompletion{userID, profile.email, profile.displayName, companyID.String}, nil
}

// ---- 重定向 URL 构造 ----

// oauthFrag:插入序构造 fragment 段 —— TS URLSearchParams 按插入序
// 串行化(token 恒在前),url.Values.Encode 会按字母序翻转键序
// (companyId 会抢到 token 前),构成 wire 分歧,故手工拼。
func oauthFrag(pairs ...[2]string) string {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p[0]))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p[1]))
	}
	return b.String()
}

func oauthDoneURL(base, token, companyID string) string {
	frag := oauthFrag([2]string{"token", token})
	if companyID != "" {
		frag += "&" + oauthFrag([2]string{"companyId", companyID})
	}
	return base + "#" + frag
}

func oauthWaitlistURL(base, email string) string {
	// TS 怪形状:query 与 fragment 同串(query 在 ?,fragment 在 #)。
	params := oauthFrag([2]string{"waitlist", "1"}, [2]string{"email", email})
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + params + "#" + params
}

func oauthSuspendedURL(base, email, reason string) string {
	pairs := [][2]string{{"suspended", "1"}, {"email", email}}
	if reason != "" {
		pairs = append(pairs, [2]string{"reason", reason})
	}
	return base + "#" + oauthFrag(pairs...)
}

func oauthErrorURL(base string, msg string) string {
	if base == "" {
		base = oauthAuthDoneURL()
	}
	return base + "#" + oauthFrag([2]string{"error", msg})
}

// ---- 审计(fire-and-forget,绝不阻断请求) ----

func oauthAudit(db *sql.DB, kind, userID, companyID, ip, ua string, detail map[string]any) {
	var detailJSON any
	if detail != nil {
		b, _ := json.Marshal(detail)
		detailJSON = string(b)
	}
	nullify := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	go func() {
		_, _ = db.Exec(
			`INSERT INTO audit_events (user_id, company_id, ip, user_agent, kind, detail)
			   VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
			nullify(userID), nullify(companyID), nullify(ip), nullify(ua), kind, detailJSON)
	}()
}

// ---- 回调编排 ----

func oauthHandleCallback(ctx context.Context, deps oauthDeps, provider, code string, returnURL string, ip, ua string, inviteToken *string) (string, error) {
	result, err := oauthCompleteFlow(ctx, deps, provider, code, inviteToken)
	if err != nil {
		var wl *oauthWaitlistedError
		if errors.As(err, &wl) {
			oauthAudit(deps.db, "signup_waitlisted", "", "", ip, ua, map[string]any{"provider": provider, "email": wl.email})
			base := oauthAuthDoneURL()
			if returnURL != "" {
				base = returnURL
			}
			return oauthWaitlistURL(base, wl.email), nil
		}
		var su *oauthSuspendedError
		if errors.As(err, &su) {
			// TS detail 的 reason 为 null(无理由时),非空串。
			var reason any
			if su.reason != "" {
				reason = su.reason
			}
			oauthAudit(deps.db, "login_suspended", "", "", ip, ua, map[string]any{"provider": provider, "email": su.email, "reason": reason})
			base := oauthAuthDoneURL()
			if returnURL != "" {
				base = returnURL
			}
			return oauthSuspendedURL(base, su.email, su.reason), nil
		}
		return "", err
	}
	token, _, err := authn.CreateSession(ctx, deps.db, result.userID, ip, ua)
	if err != nil {
		return "", err
	}
	oauthAudit(deps.db, "login", result.userID, result.companyID, ip, ua, map[string]any{"provider": provider, "email": result.email})
	base := oauthAuthDoneURL()
	if returnURL != "" {
		base = returnURL
	}
	return oauthDoneURL(base, token, result.companyID), nil
}

func oauthCompleteFlow(ctx context.Context, deps oauthDeps, provider, code string, inviteToken *string) (*oauthCompletion, error) {
	accessToken, err := oauthExchangeCode(ctx, provider, code)
	if err != nil {
		return nil, err
	}
	profile, err := oauthFetchProfile(ctx, provider, accessToken)
	if err != nil {
		return nil, err
	}
	return oauthFindOrCreate(ctx, deps.db, provider, profile, inviteToken)
}

// ---- 路由 ----

func oauthClientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// AuthProviders —— 登录面 provider 探活(#284):未配的 provider 在按钮
// 层显性化,而不是点进去吃裸 503(桌面首启向导与 AuthScreen 共用)。
// 只报配置态,不回显任何配置内容。
func (s *Server) AuthProviders(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{
		"github": oauthProviderEnabled("github"),
		"google": oauthProviderEnabled("google"),
	})
}

func (s *Server) AuthStart(w http.ResponseWriter, r *http.Request, provider string, params contract.AuthStartParams) {
	deps := oauthDeps{db: s.DB, rdb: s.RDB}
	if provider != "google" && provider != "github" {
		httpx.WriteError(w, http.StatusNotFound, "unknown provider")
		return
	}
	if !oauthProviderEnabled(provider) {
		httpx.WriteError(w, http.StatusServiceUnavailable, provider+" oauth not configured")
		return
	}
	raw := r.URL.Query().Get("return")
	if raw != "" && !oauthReturnURLAllowed(raw) {
		httpx.WriteError(w, http.StatusBadRequest, "return URL not allowed")
		return
	}
	var returnURL, inviteToken *string
	if raw != "" {
		returnURL = &raw
	}
	if inv := r.URL.Query().Get("invite"); len(inv) >= 8 && len(inv) <= 200 {
		inviteToken = &inv
	}
	state, err := oauthCreateState(r.Context(), deps.rdb, provider, returnURL, inviteToken)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	http.Redirect(w, r, oauthAuthorizeURL(provider, state), http.StatusFound)
}

func (s *Server) AuthCallback(w http.ResponseWriter, r *http.Request, provider string) {
	deps := oauthDeps{db: s.DB, rdb: s.RDB}
	if provider != "google" && provider != "github" {
		httpx.WriteError(w, http.StatusNotFound, "unknown provider")
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Redirect(w, r, oauthErrorURL("", "missing_code_or_state"), http.StatusFound)
		return
	}
	ip := oauthClientIP(r)
	ua := r.UserAgent()
	claimed, err := oauthConsumeState(r.Context(), deps.rdb, state)
	if err != nil {
		// Redis 故障与业务错误同路:login_failed 审计(全文)+ #error=<截断>。
		full := err.Error()
		oauthAudit(deps.db, "login_failed", "", "", ip, ua, map[string]any{"provider": provider, "error": full})
		http.Redirect(w, r, oauthErrorURL("", oauthCut120(full)), http.StatusFound)
		return
	}
	if claimed == nil || claimed.Provider != provider {
		http.Redirect(w, r, oauthErrorURL("", "bad_state"), http.StatusFound)
		return
	}
	var returnURL, inviteToken string
	if claimed.ReturnURL != nil {
		returnURL = *claimed.ReturnURL
	}
	if claimed.InviteToken != nil {
		inviteToken = *claimed.InviteToken
	}
	var inv *string
	if inviteToken != "" {
		inv = &inviteToken
	}
	target, err := oauthHandleCallback(r.Context(), deps, provider, code, returnURL, ip, ua, inv)
	if err != nil {
		// TS:审计存全文,仅 URL 截 120(UTF-16 码元;Go 以 rune 近似)。
		full := err.Error()
		oauthAudit(deps.db, "login_failed", "", "", ip, ua, map[string]any{"provider": provider, "error": full})
		http.Redirect(w, r, oauthErrorURL(returnURL, oauthCut120(full)), http.StatusFound)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// oauthCut120:重定向 URL 的错误文案截断 —— TS slice(0,120) 按 UTF-16
// 码元(#141 rider 换 UTF16Cap 精确对齐;审计侧不截)。
func oauthCut120(s string) string {
	return httpx.UTF16Cap(s, 120)
}
