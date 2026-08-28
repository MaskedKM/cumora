// apple_test.go —— verifyAppleIdentityToken / resolveTrustedAppleEmail
// 单测(#123):httptest 假 JWKS + 现场 RSA 签名,钉住验签、claims 检
// 查、密钥未命中重取与 email 信任边界。对齐 apple.ts 的行为面。
package core

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type appleTestKeys struct {
	key *rsa.PrivateKey
	kid string
}

// b64urlNoPad:JWT 段编码(与来料解码 appleB64url 互逆)。
func b64urlNoPad(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// signAppleToken:RS256 签一个三段式 identity_token;claims 原样注入。
func signAppleToken(t *testing.T, tk appleTestKeys, headerExtra func(map[string]any), claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "kid": tk.kid}
	if headerExtra != nil {
		headerExtra(header)
	}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(claims)
	signing := b64urlNoPad(hb) + "." + b64urlNoPad(pb)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, tk.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return string(signing) + "." + b64urlNoPad(sig)
}

// jwksHandler:可变形桩 —— served 控制当前公布的密钥集(kid→key)。
func jwksHandler(served *map[string]appleTestKeys) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys := []map[string]string{}
		for kid, tk := range *served {
			keys = append(keys, map[string]string{
				"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
				"n": b64urlNoPad(tk.key.N.Bytes()),
				"e": b64urlNoPad(bigToBytes(tk.key.E)),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}
}

func bigToBytes(e int) []byte {
	return []byte{byte(e >> 24), byte(e >> 16), byte(e >> 8), byte(e)}
}

func resetAppleJwksCache() {
	appleJwksMu.Lock()
	appleJwksCache = nil
	appleJwksMu.Unlock()
}

// appleSignKey:套件共享签名钥(独立于桩内容,坏签名用例另配钥匙)。
var appleSignKey = mustAppleKey()

func mustAppleKey() appleTestKeys {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return appleTestKeys{key: key, kid: "test-kid-1"}
}

// freshAppleToken:合法基线 claims,mutate 改写后再签。
func freshAppleToken(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"iss": appleIssuer, "aud": "io.cumora.app", "sub": "001234.abcdef.1234",
		"exp": float64(time.Now().Add(10 * time.Minute).Unix()),
	}
	if mutate != nil {
		mutate(claims)
	}
	return signAppleToken(t, appleSignKey, nil, claims)
}

func TestVerifyAppleIdentityTokenHappyPath(t *testing.T) {
	served := map[string]appleTestKeys{appleSignKey.kid: appleSignKey}
	srv := httptest.NewServer(jwksHandler(&served))
	defer srv.Close()
	t.Setenv("CUMORA_APPLE_JWKS_URL", srv.URL)
	resetAppleJwksCache()

	token := signAppleToken(t, appleSignKey, nil, map[string]any{
		"iss": appleIssuer, "aud": "io.cumora.app", "sub": "sub-123",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
		// Apple 发字符串 "true",不是布尔。
		"email": "User@Example.com", "email_verified": "true", "is_private_email": "false",
	})
	claims, err := verifyAppleIdentityToken(context.Background(), token, []string{"io.cumora.app"})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.sub != "sub-123" || claims.email != "user@example.com" ||
		!claims.emailVerified || claims.isPrivateEmail || claims.aud != "io.cumora.app" {
		t.Fatalf("claims mismatch: %+v", claims)
	}
}

func TestVerifyAppleIdentityTokenBadSignature(t *testing.T) {
	served := map[string]appleTestKeys{appleSignKey.kid: appleSignKey}
	srv := httptest.NewServer(jwksHandler(&served))
	defer srv.Close()
	t.Setenv("CUMORA_APPLE_JWKS_URL", srv.URL)
	resetAppleJwksCache()

	// 用另一把钥匙签名,JWKS 只公布正钥。
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	token := signAppleToken(t, appleTestKeys{key: other, kid: appleSignKey.kid}, nil, map[string]any{
		"iss": appleIssuer, "aud": "io.cumora.app", "sub": "s", "exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	if _, err := verifyAppleIdentityToken(context.Background(), token, []string{"io.cumora.app"}); err == nil || err.Error() != "apple identity_token bad signature" {
		t.Fatalf("want bad signature, got %v", err)
	}
}

func TestVerifyAppleIdentityTokenClaimRejections(t *testing.T) {
	served := map[string]appleTestKeys{appleSignKey.kid: appleSignKey}
	srv := httptest.NewServer(jwksHandler(&served))
	defer srv.Close()
	t.Setenv("CUMORA_APPLE_JWKS_URL", srv.URL)
	resetAppleJwksCache()

	cases := []struct {
		name   string
		mutate func(claims map[string]any)
		want   string
	}{
		{"wrong aud", func(c map[string]any) { c["aud"] = "com.evil.app" }, "apple aud=com.evil.app not in [io.cumora.app]"},
		{"wrong iss", func(c map[string]any) { c["iss"] = "https://evil.example" }, "apple iss=https://evil.example expected " + appleIssuer},
		{"expired", func(c map[string]any) { c["exp"] = float64(time.Now().Add(-time.Hour).Unix()) }, "apple identity_token expired"},
		{"exp as string", func(c map[string]any) { c["exp"] = "9999999999" }, "apple identity_token expired"},
		{"missing sub", func(c map[string]any) { c["sub"] = "" }, "apple identity_token missing sub"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := freshAppleToken(t, tc.mutate)
			_, err := verifyAppleIdentityToken(context.Background(), token, []string{"io.cumora.app"})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestVerifyAppleIdentityTokenHeaderRejections(t *testing.T) {
	served := map[string]appleTestKeys{appleSignKey.kid: appleSignKey}
	srv := httptest.NewServer(jwksHandler(&served))
	defer srv.Close()
	t.Setenv("CUMORA_APPLE_JWKS_URL", srv.URL)
	resetAppleJwksCache()

	if _, err := verifyAppleIdentityToken(context.Background(), "not-a-jwt", []string{"io.cumora.app"}); err == nil {
		t.Fatal("malformed token must fail")
	}
	// HS256 拒
	hsToken := signAppleToken(t, appleSignKey, func(h map[string]any) { h["alg"] = "HS256" }, map[string]any{"sub": "s"})
	if _, err := verifyAppleIdentityToken(context.Background(), hsToken, []string{"io.cumora.app"}); err == nil || err.Error() != "apple alg HS256 not supported" {
		t.Fatalf("alg: %v", err)
	}
	// 无 kid 拒
	noKid := signAppleToken(t, appleSignKey, func(h map[string]any) { delete(h, "kid") }, map[string]any{"sub": "s"})
	if _, err := verifyAppleIdentityToken(context.Background(), noKid, []string{"io.cumora.app"}); err == nil || err.Error() != "apple identity_token missing kid" {
		t.Fatalf("kid: %v", err)
	}
}

// TestVerifyAppleIdentityTokenUnknownKidRefetch:首答不含 kid → 强制重取
// 一次;轮钥后新 kid 由此生效,仍未命中才报 unknown key。
func TestVerifyAppleIdentityTokenUnknownKidRefetch(t *testing.T) {
	served := map[string]appleTestKeys{} // 首答:空密钥集
	srv := httptest.NewServer(jwksHandler(&served))
	defer srv.Close()
	t.Setenv("CUMORA_APPLE_JWKS_URL", srv.URL)
	resetAppleJwksCache()

	token := freshAppleToken(t, nil)

	// 第一次:未知 kid(缓存了空集)→ 重取仍空 → unknown key。
	if _, err := verifyAppleIdentityToken(context.Background(), token, []string{"io.cumora.app"}); err == nil ||
		err.Error() != fmt.Sprintf("apple identity_token signed with unknown key kid=%s", appleSignKey.kid) {
		t.Fatalf("unknown kid: %v", err)
	}

	// 轮钥:桩公布正钥。verify 内部会把空集缓存作废重取,应当通过。
	served[appleSignKey.kid] = appleSignKey
	if _, err := verifyAppleIdentityToken(context.Background(), token, []string{"io.cumora.app"}); err != nil {
		t.Fatalf("after rotation refetch should pass: %v", err)
	}
}

func TestResolveTrustedAppleEmail(t *testing.T) {
	// 已链 email 恒赢 —— 即使后续 token 带不同值。
	got, err := resolveTrustedAppleEmail("Linked@Example.com ", "new@example.com", true)
	if err != nil || got != "linked@example.com" {
		t.Fatalf("linked wins: %q %v", got, err)
	}
	// 未链 + 已验证 → token email。
	got, err = resolveTrustedAppleEmail("", "Token@Example.com", true)
	if err != nil || got != "token@example.com" {
		t.Fatalf("token verified: %q %v", got, err)
	}
	// 未链 + 未验证 / 无 email → 拒。
	if _, err := resolveTrustedAppleEmail("", "x@example.com", false); err == nil {
		t.Fatal("unverified must fail")
	}
	if _, err := resolveTrustedAppleEmail("", "", true); err == nil {
		t.Fatal("no email must fail")
	}
}
