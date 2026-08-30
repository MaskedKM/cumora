// domains/core/apple —— POST /api/auth/apple/native(#123):iOS 原生
// Sign in with Apple。与 Google/GitHub OAuth(换码 → userinfo)不同,
// Apple 把 OIDC identity_token 直接交给客户端,服务端的职责是对着
// Apple 公布的 JWKS 验 RS256 签名并取出稳定的 sub + email。无浏览器
// 重定向,会话 token 以 JSON 返回。逐段对齐 server/src/apple.ts 与
// oauth.ts handleAppleNativeSignIn(router.ts 718–765)。
//
// 已验证 claims 的语义(见 apple.ts 头注):
//   - iss 恒 https://appleid.apple.com;aud 是 iOS bundle id
//     (仅 io.cumora.app);sub 是每 (user, app) 终身稳定的不透明 id,
//     user_identities 挂它。
//   - email 可能是真地址或 Apple 中继 …@privaterelay.appleid.com,
//     两者都算已验证;email_verified 在 JWT 里是字符串 "true"。
//   - 凭据元数据(用户名等)仅首次授权返回且不参与身份匹配 ——
//     回头客全靠已持久化的 sub 链接解析。
package core

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

const (
	appleJwksURLDefault = "https://appleid.apple.com/auth/keys"
	appleIssuer         = "https://appleid.apple.com"
	appleJwksTTL        = 24 * time.Hour // 密钥季度轮换,24h 缓存绰绰有余
)

// CUMORA_APPLE_JWKS_URL:仅测试/开发桩覆盖(生产空 = Apple 正源)。
func appleJwksURL() string {
	if u := os.Getenv("CUMORA_APPLE_JWKS_URL"); u != "" {
		return u
	}
	return appleJwksURLDefault
}

type appleJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"` // base64url 模数
	E   string `json:"e"` // base64url 指数
}

type appleJwksEntry struct {
	fetchedAt time.Time
	keys      map[string]appleJWK
}

var (
	appleJwksMu    sync.Mutex
	appleJwksCache *appleJwksEntry
)

// appleFetchJwks:24h TTL 缓存;锁横跨取数以串发并发首请求(该端点
// 低频,正确性优先)。
func appleFetchJwks(ctx context.Context) (map[string]appleJWK, error) {
	appleJwksMu.Lock()
	defer appleJwksMu.Unlock()
	if appleJwksCache != nil && time.Since(appleJwksCache.fetchedAt) < appleJwksTTL {
		return appleJwksCache.keys, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, appleJwksURL(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("apple JWKS fetch %d", resp.StatusCode)
	}
	var body struct {
		Keys []appleJWK `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	keys := map[string]appleJWK{}
	for _, k := range body.Keys {
		keys[k.Kid] = k
	}
	appleJwksCache = &appleJwksEntry{fetchedAt: time.Now(), keys: keys}
	return keys, nil
}

// appleB64url:JWT 段是不带 padding 的 base64url;容忍来料带 '='。
func appleB64url(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func appleRSAPublicKey(jwk appleJWK) (*rsa.PublicKey, error) {
	if jwk.Kty != "RSA" {
		return nil, errors.New("unsupported kty")
	}
	nb, err := appleB64url(jwk.N)
	if err != nil {
		return nil, err
	}
	eb, err := appleB64url(jwk.E)
	if err != nil {
		return nil, err
	}
	e := new(big.Int).SetBytes(eb)
	if !e.IsInt64() || e.Int64() <= 0 || e.Int64() > math.MaxInt32 {
		return nil, errors.New("bad exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(e.Int64())}, nil
}

type appleClaims struct {
	sub            string
	email          string // 空串 = 无 email claim
	emailVerified  bool
	isPrivateEmail bool
	aud            string
}

// appleTruthy:Apple 把 email_verified / is_private_email 发成字符串
// "true" 而非布尔,两种都收。
func appleTruthy(v any) bool {
	return v == true || v == "true"
}

// verifyAppleIdentityToken:任何篡改/过期/错 audience/错 issuer/未知
// 签名密钥都报错;全过才回可信 claims。对齐 apple.ts
// verifyAppleIdentityToken。
func verifyAppleIdentityToken(ctx context.Context, token string, expectedAud []string) (*appleClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed apple identity_token")
	}
	headerB64, payloadB64, sigB64 := parts[0], parts[1], parts[2]
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	hb, err := appleB64url(headerB64)
	if err != nil || json.Unmarshal(hb, &header) != nil {
		return nil, errors.New("malformed apple identity_token header")
	}
	alg := header.Alg
	if alg == "" {
		alg = "missing"
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("apple alg %s not supported", alg)
	}
	if header.Kid == "" {
		return nil, errors.New("apple identity_token missing kid")
	}

	// 解签名密钥:未命中先失效缓存强制重取一次(季度轮钥,缓存可能陈旧)。
	keys, err := appleFetchJwks(ctx)
	if err != nil {
		return nil, err
	}
	jwk, ok := keys[header.Kid]
	if !ok {
		appleJwksMu.Lock()
		appleJwksCache = nil
		appleJwksMu.Unlock()
		if keys, err = appleFetchJwks(ctx); err != nil {
			return nil, err
		}
		jwk, ok = keys[header.Kid]
	}
	if !ok {
		return nil, fmt.Errorf("apple identity_token signed with unknown key kid=%s", header.Kid)
	}

	// RS256:签名输入是点连的 header.payload。
	sig, err := appleB64url(sigB64)
	if err != nil {
		return nil, errors.New("apple identity_token bad signature")
	}
	pub, err := appleRSAPublicKey(jwk)
	if err != nil {
		return nil, errors.New("apple identity_token bad signature")
	}
	sum := sha256.Sum256([]byte(headerB64 + "." + payloadB64))
	if rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
		return nil, errors.New("apple identity_token bad signature")
	}

	// claims 走动态 map 而非结构体:TS typeof 检查(exp 是字符串即拒)
	// 的行为要逐位复刻。
	pb, err := appleB64url(payloadB64)
	if err != nil {
		return nil, errors.New("malformed apple identity_token payload")
	}
	var raw map[string]any
	if json.Unmarshal(pb, &raw) != nil {
		return nil, errors.New("malformed apple identity_token payload")
	}
	if iss, _ := raw["iss"].(string); iss != appleIssuer {
		return nil, fmt.Errorf("apple iss=%v expected %s", raw["iss"], appleIssuer)
	}
	sub, _ := raw["sub"].(string)
	if sub == "" {
		return nil, errors.New("apple identity_token missing sub")
	}
	aud, _ := raw["aud"].(string)
	if aud == "" || !slices.Contains(expectedAud, aud) {
		return nil, fmt.Errorf("apple aud=%v not in [%s]", raw["aud"], strings.Join(expectedAud, ","))
	}
	exp, isNum := raw["exp"].(float64)
	if !isNum || exp < float64(time.Now().Unix()) {
		return nil, errors.New("apple identity_token expired")
	}
	email := ""
	if e, _ := raw["email"].(string); e != "" {
		email = strings.ToLower(e)
	}
	return &appleClaims{
		sub:            sub,
		email:          email,
		emailVerified:  appleTruthy(raw["email_verified"]),
		isPrivateEmail: appleTruthy(raw["is_private_email"]),
		aud:            aud,
	}, nil
}

// resolveTrustedAppleEmail:只收可安全用于账号查找的 email —— 已链
// sub 的持久化 email 恒赢(后续 token 带别的值也不信);未链 sub 只认
// Apple 签名 token 里已验证的 email。请求体回调字段刻意不进这道
// 边界:原生凭据元数据是展示数据,不是邮箱所有权证明。
func resolveTrustedAppleEmail(linkedEmail, tokenEmail string, tokenEmailVerified bool) (string, error) {
	linked := strings.ToLower(strings.TrimSpace(linkedEmail))
	if linked != "" {
		return linked, nil
	}
	token := strings.ToLower(strings.TrimSpace(tokenEmail))
	if token == "" || !tokenEmailVerified {
		return "", errors.New("apple sign-in: verified email not available for unlinked identity")
	}
	return token, nil
}

// appleNative:POST /api/auth/apple/native —— 验 JWT、find-or-create、
// 以 JSON 回会话 token(无浏览器重定向)。错误三态:waitlisted/suspended
// → 403 携 email;其余(含验签失败)→ 400。对齐 router.ts 718–765。
func (s *Server) AuthAppleNative(w http.ResponseWriter, r *http.Request) {
	deps := oauthDeps{db: s.DB, rdb: s.RDB}
	var body struct {
		IdentityToken *string `json:"identityToken"`
		Name          *string `json:"name"`
		InviteToken   *string `json:"inviteToken"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.IdentityToken == nil || *body.IdentityToken == "" {
		httpx.WriteError(w, http.StatusBadRequest, "identityToken required")
		return
	}
	ip := oauthClientIP(r)
	ua := r.UserAgent()
	claims, err := verifyAppleIdentityToken(r.Context(), *body.IdentityToken, []string{"io.cumora.app"})
	if err == nil {
		// 回头客不需要 email claim;首登只认签名 token 里已验证的
		// email,绝不认请求体元数据。
		var knownEmail sql.NullString
		_ = deps.db.QueryRowContext(r.Context(),
			`SELECT email_lower FROM user_identities WHERE provider = 'apple' AND provider_id = $1`,
			claims.sub).Scan(&knownEmail)
		var email string
		email, err = resolveTrustedAppleEmail(knownEmail.String, claims.email, claims.emailVerified)
		if err == nil {
			// TS (fallbackName ?? email 邮箱前缀 ?? 'You').trim() || 'You'
			// —— ?? 只认 null/undefined,空串名 trim 后落 'You'。
			displayName := strings.SplitN(email, "@", 2)[0]
			if body.Name != nil {
				displayName = *body.Name
			}
			displayName = strings.TrimSpace(displayName)
			if displayName == "" {
				displayName = "You"
			}
			var inv *string
			if body.InviteToken != nil && *body.InviteToken != "" {
				inv = body.InviteToken
			}
			var result *oauthCompletion
			result, err = oauthFindOrCreate(r.Context(), deps.db, "apple", &oauthProfile{
				providerID: claims.sub, email: email, displayName: displayName, avatarURL: "",
			}, inv)
			if err == nil {
				var token string
				token, _, err = authn.CreateSession(r.Context(), deps.db, result.userID, ip, ua)
				if err == nil {
					oauthAudit(deps.db, "login", result.userID, result.companyID, ip, ua,
						map[string]any{"provider": "apple", "email": result.email})
					var companyID any // TS null 语义
					if result.companyID != "" {
						companyID = result.companyID
					}
					httpx.WriteJSON(w, http.StatusOK, map[string]any{
						"token":     token,
						"user":      map[string]any{"id": result.userID, "email": result.email, "displayName": result.displayName},
						"companyId": companyID,
					})
					return
				}
			}
		}
	}
	var wl *oauthWaitlistedError
	if errors.As(err, &wl) {
		oauthAudit(deps.db, "signup_waitlisted", "", "", ip, ua,
			map[string]any{"provider": "apple", "email": wl.email})
		httpx.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "waitlisted", "email": wl.email})
		return
	}
	var su *oauthSuspendedError
	if errors.As(err, &su) {
		var reason any // TS reason null(无理由时)
		if su.reason != "" {
			reason = su.reason
		}
		oauthAudit(deps.db, "login_suspended", "", "", ip, ua,
			map[string]any{"provider": "apple", "email": su.email, "reason": reason})
		httpx.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "suspended", "email": su.email, "reason": reason})
		return
	}
	log.Printf("[auth] apple native sign-in failed: %v", err)
	// TS baseline:console.warn + 400 透传失败原因(登录面需要理由),
	// 非 errorHandler 面 —— 不进 WriteInternalError。
	httpx.WriteError(w, http.StatusBadRequest, err.Error())
}
