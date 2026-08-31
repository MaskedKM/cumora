// push —— 推送(#59):APNs(iOS)/FCM(Android)发送面 + 消息触达扇出。
// 对齐 已退役 TS server 的 push.ts + fcm.ts:凭据缺失软关停(启动告警一次,
// 发送路径 no-op);死令牌软禁用;在线用户(WS avail)不推送。
package push

import (
	"crypto"
	"crypto/ecdsa"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
)

// Payload 对齐 PushPayload(形似 APNs aps,便于 Android 映射)。
type Payload struct {
	Title    string
	Body     string
	ThreadID string
	Badge    *int
	Data     map[string]any
}

/* ───────────── APNs 凭据与 JWT ───────────── */

var (
	apnsCredsOnce  sync.Once
	apnsConfigured bool
)

func apnsOn() bool {
	apnsCredsOnce.Do(func() {
		apnsConfigured = config.APNSKeyPath() != "" && config.APNSKeyID() != "" &&
			config.APNSTeamID() != "" && config.APNSTopic() != ""
		if !apnsConfigured {
			slog.Warn("APNs credentials not configured (APNS_KEY_PATH/APNS_KEY_ID/APNS_TEAM_ID) — push send path is a no-op.")
		}
	})
	return apnsConfigured
}

var (
	apnsKeyOnce sync.Once
	apnsKeyPEM  string
	apnsKeyErr  error
)

func loadApnsKey() (string, error) {
	apnsKeyOnce.Do(func() {
		raw, err := os.ReadFile(config.APNSKeyPath())
		if err != nil {
			apnsKeyErr = err
			return
		}
		apnsKeyPEM = string(raw)
	})
	return apnsKeyPEM, apnsKeyErr
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// lenientBase64:Node Buffer.from(…,'base64') 的宽容语义 —— 依次尝试
// 标准/去垫/URL 变体。
func lenientBase64(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// parseECKey 从 PEM 解析 ES256 私钥。
func parseECKey(pemStr string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block in APNS key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if k2, err2 := x509.ParseECPrivateKey(block.Bytes); err2 == nil {
			return k2, nil
		}
		return nil, err
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("APNS key is not EC")
	}
	return ec, nil
}

var (
	apnsTokenMu   sync.Mutex
	apnsTokenJWT  string
	apnsTokenTime time.Time
)

const apnsTokenTTL = 50 * time.Minute

// mintApnsJWT:ES256(JWT 头含 kid);签名为 IEEE P1363 r||s 64 字节。
func mintApnsJWT() (string, error) {
	apnsTokenMu.Lock()
	defer apnsTokenMu.Unlock()
	now := time.Now()
	if apnsTokenJWT != "" && now.Sub(apnsTokenTime) < apnsTokenTTL {
		return apnsTokenJWT, nil
	}
	pemStr, err := loadApnsKey()
	if err != nil {
		return "", err
	}
	key, err := parseECKey(pemStr)
	if err != nil {
		return "", err
	}
	header, _ := json.Marshal(map[string]string{
		"alg": "ES256", "kid": config.APNSKeyID(), "typ": "JWT",
	})
	iat := now.Unix()
	claims, _ := json.Marshal(map[string]any{
		"iss": config.APNSTeamID(), "iat": iat,
	})
	signingInput := b64url(header) + "." + b64url(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(crand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	// P1363:定宽 r||s(P-256 → 各 32 字节)
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	jwt := signingInput + "." + b64url(sig)
	apnsTokenJWT = jwt
	apnsTokenTime = now
	return jwt, nil
}

func apnsHost() string {
	if config.APNSEnv() == "production" {
		return "https://api.push.apple.com"
	}
	return "https://api.sandbox.push.apple.com"
}

// h2 client:Apple 期望长连复用;http.Transport 的连接池天然复用 HTTP/2。
var apnsClient = &http.Client{
	Transport: &http.Transport{ForceAttemptHTTP2: true, MaxConnsPerHost: 8},
	Timeout:   15 * time.Second,
}

// #136:FCM 出站(令牌兑换 + 发送)禁止走无超时 http.DefaultClient——
// 挂起端点会连坐扇出 goroutine 与池连接;对齐 APNs 侧 15s。var 以便
// 测试注入短超时/替身端点。
var fcmClient = &http.Client{Timeout: 15 * time.Second}

type apnsResult struct {
	ok     bool
	status int
	reason string
}

func sendOneApns(token string, payload Payload) apnsResult {
	jwt, err := mintApnsJWT()
	if err != nil {
		slog.Warn("APNs jwt mint failed", "err", err)
		return apnsResult{reason: "jwt-error"}
	}
	aps := map[string]any{
		"alert":           map[string]string{"title": payload.Title, "body": payload.Body},
		"sound":           "default",
		"mutable-content": 1,
	}
	if payload.Badge != nil {
		aps["badge"] = *payload.Badge
	}
	if payload.ThreadID != "" {
		aps["thread-id"] = payload.ThreadID
	}
	bodyMap := map[string]any{"aps": aps}
	for k, v := range payload.Data {
		bodyMap[k] = v
	}
	raw, _ := json.Marshal(bodyMap)
	req, err := http.NewRequest(http.MethodPost, apnsHost()+"/3/device/"+token, strings.NewReader(string(raw)))
	if err != nil {
		return apnsResult{reason: "session-error"}
	}
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-topic", config.APNSTopic())
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("content-type", "application/json")
	res, err := apnsClient.Do(req)
	if err != nil {
		return apnsResult{reason: "stream-error"}
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return apnsResult{ok: true, status: res.StatusCode}
	}
	reason := ""
	var parsed struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		reason = parsed.Reason
	}
	return apnsResult{status: res.StatusCode, reason: reason}
}

// deadApnsReasons 对齐 DEAD_REASONS;410 亦视为死。
var deadApnsReasons = map[string]bool{
	"BadDeviceToken": true, "Unregistered": true,
	"ExpiredToken": true, "DeviceTokenNotForTopic": true,
}

/* ───────────── FCM ───────────── */

type serviceAccount struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
}

var (
	fcmOnce    sync.Once
	fcmAccount *serviceAccount
	fcmLogged  bool
)

func fcmOn() bool {
	fcmOnce.Do(func() {
		raw := ""
		if v := config.FCMServiceAccountJSON(); v != "" {
			if strings.HasPrefix(v, "{") {
				raw = v
			} else if dec, ok := lenientBase64(v); ok {
				raw = string(dec)
			}
		} else if p := config.FCMServiceAccountPath(); p != "" {
			b, err := os.ReadFile(p)
			if err == nil {
				raw = string(b)
			}
		}
		if raw != "" {
			var acct serviceAccount
			if json.Unmarshal([]byte(raw), &acct) == nil &&
				acct.ProjectID != "" && acct.ClientEmail != "" && acct.PrivateKey != "" {
				acct.PrivateKey = strings.ReplaceAll(acct.PrivateKey, `\n`, "\n")
				fcmAccount = &acct
				return
			}
		}
		if !fcmLogged {
			slog.Warn("FCM service account not configured (FCM_SERVICE_ACCOUNT_JSON / FCM_SERVICE_ACCOUNT_PATH) — Android push send path is a no-op.")
			fcmLogged = true
		}
	})
	return fcmAccount != nil
}

const (
	fcmTokenURL = "https://oauth2.googleapis.com/token"
	fcmScope    = "https://www.googleapis.com/auth/firebase.messaging"
	fcmTokenTTL = 50 * time.Minute
)

// fcmSendURLFmt:var 以便测试指向替身端点(#136)。
var fcmSendURLFmt = "https://fcm.googleapis.com/v1/projects/%s/messages:send"

var (
	fcmTokenMu   sync.Mutex
	fcmTokenVal  string
	fcmTokenTime time.Time
)

func getFcmAccessToken(acct *serviceAccount) (string, bool) {
	fcmTokenMu.Lock()
	defer fcmTokenMu.Unlock()
	now := time.Now()
	if fcmTokenVal != "" && now.Sub(fcmTokenTime) < fcmTokenTTL {
		return fcmTokenVal, true
	}
	key, ok := parseRsaKey(acct.PrivateKey)
	if !ok {
		return "", false
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	iat := now.Unix()
	claims, _ := json.Marshal(map[string]any{
		"iss": acct.ClientEmail, "scope": fcmScope, "aud": fcmTokenURL,
		"iat": iat, "exp": iat + 3600,
	})
	signingInput := b64url(header) + "." + b64url(claims)
	// RS256 = RSASSA-PKCS1-v1_5 over SHA-256
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", false
	}
	assertion := signingInput + "." + b64url(sig)
	form := "grant_type=" + url.QueryEscape("urn:ietf:params:oauth:grant-type:jwt-bearer") +
		"&assertion=" + url.QueryEscape(assertion)
	req, err := http.NewRequest(http.MethodPost, fcmTokenURL, strings.NewReader(form))
	if err != nil {
		return "", false
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	res, err := fcmClient.Do(req)
	if err != nil || res.StatusCode >= 300 {
		if res != nil {
			res.Body.Close()
		}
		return "", false
	}
	defer res.Body.Close()
	var parsed struct {
		AccessToken string `json:"access_token"`
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if json.Unmarshal(body, &parsed) != nil || parsed.AccessToken == "" {
		return "", false
	}
	fcmTokenVal = parsed.AccessToken
	fcmTokenTime = now
	return parsed.AccessToken, true
}

// parseRsaKey:PKCS#8 优先,PKCS#1(BEGIN RSA PRIVATE KEY)回退。
func parseRsaKey(pemStr string) (*rsa.PrivateKey, bool) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, false
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := key.(*rsa.PrivateKey); ok {
			return rk, true
		}
		return nil, false
	}
	if rk, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return rk, true
	}
	return nil, false
}

type fcmResult struct {
	ok     bool
	status int
	dead   bool
	reason string
}

var deadFcmStatuses = map[string]bool{
	"UNREGISTERED": true, "INVALID_ARGUMENT": true, "NOT_FOUND": true, "SENDER_ID_MISMATCH": true,
}

func sendOneFcm(token string, payload Payload) fcmResult {
	acct := fcmAccount
	if acct == nil {
		return fcmResult{reason: "not-configured"}
	}
	access, ok := getFcmAccessToken(acct)
	if !ok {
		return fcmResult{reason: "no-access-token"}
	}
	data := map[string]string{}
	for k, v := range payload.Data {
		if v != nil {
			data[k] = fmt.Sprintf("%v", v)
		}
	}
	android := map[string]any{
		"priority": "high",
		"notification": map[string]any{
			"sound": "default",
		},
	}
	if payload.ThreadID != "" {
		android["notification"].(map[string]any)["tag"] = payload.ThreadID
	}
	message := map[string]any{
		"token":        token,
		"notification": map[string]string{"title": payload.Title, "body": payload.Body},
		"data":         data,
		"android":      android,
	}
	raw, _ := json.Marshal(map[string]any{"message": message})
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf(fcmSendURLFmt, acct.ProjectID), strings.NewReader(string(raw)))
	if err != nil {
		return fcmResult{reason: "fetch-error"}
	}
	req.Header.Set("authorization", "Bearer "+access)
	req.Header.Set("content-type", "application/json")
	res, err := fcmClient.Do(req)
	if err != nil {
		return fcmResult{reason: "fetch-error"}
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return fcmResult{ok: true, status: res.StatusCode}
	}
	reason := ""
	var parsed struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		for _, d := range parsed.Error.Details {
			if d.ErrorCode != "" {
				reason = d.ErrorCode
				break
			}
		}
		if reason == "" {
			reason = parsed.Error.Status
		}
	}
	dead := res.StatusCode == 404 || (reason != "" && deadFcmStatuses[reason])
	return fcmResult{status: res.StatusCode, dead: dead, reason: reason}
}
