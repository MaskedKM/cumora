// authn —— 会话令牌解析(#52),行为对齐 已退役 TS server 的 auth.ts:
// sha256(token) base64url 存库;30 天硬过期 + 14 天空闲过期;
// JOIN users 过 suspension/deletion 门;命中即滑动更新 last_used_at。
package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"strings"
	"time"
)

const (
	sessionTTL  = 30 * 24 * time.Hour
	sessionIdle = 14 * 24 * time.Hour
	wsTicketTTL = 60 * time.Second
)

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Token 随机生成(Bearer 原文只出现在响应里,库只见哈希)。
func NewToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// CreateSession 写 sessions 行并回原文 token。
func CreateSession(ctx context.Context, db *sql.DB, userID, ip, ua string) (token string, expiresAt time.Time, err error) {
	token = NewToken()
	expiresAt = time.Now().Add(sessionTTL)
	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at, ip, user_agent)
		 VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''))`,
		HashToken(token), userID, expiresAt, ip, ua)
	if err != nil {
		return "", time.Time{}, err
	}
	_, _ = db.ExecContext(ctx, `UPDATE users SET last_login_at = NOW() WHERE id = $1`, userID)
	return token, expiresAt, nil
}

// ResolveSession 按 token 找活跃会话;过期/空闲即删行返 nil,
// suspended/deleted 用户拒认证但不删行(与 baseline 逐句对齐)。
func ResolveSession(ctx context.Context, db *sql.DB, token string) (userID string, ok bool) {
	hash := HashToken(token)
	var uid string
	var expiresAt, lastUsed time.Time
	var suspendedAt, deletedAt sql.NullTime
	err := db.QueryRowContext(ctx, `
		SELECT s.user_id, s.expires_at, s.last_used_at, u.suspended_at, u.deleted_at
		  FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = $1`, hash).
		Scan(&uid, &expiresAt, &lastUsed, &suspendedAt, &deletedAt)
	if err != nil {
		return "", false
	}
	now := time.Now()
	if now.After(expiresAt) || now.Sub(lastUsed) > sessionIdle {
		_, _ = db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash)
		return "", false
	}
	if suspendedAt.Valid || deletedAt.Valid {
		return "", false
	}
	// 滑动更新;失败不阻断请求(与 baseline 的 fire-and-forget 一致)
	go db.Exec(`UPDATE sessions SET last_used_at = NOW() WHERE token_hash = $1`, hash)
	return uid, true
}

// Bearer 从 Authorization 头(或 x-session-token)取令牌;两侧均 trim。
func Bearer(header, altHeader string) string {
	if len(header) > 7 && header[:7] == "Bearer " {
		return strings.TrimSpace(header[7:])
	}
	return strings.TrimSpace(altHeader)
}

// CreateWsTicket 一次性 WS 票据(60s)。
func CreateWsTicket(ctx context.Context, db *sql.DB, userID string) (ticket string, expiresAt time.Time, err error) {
	ticket = NewToken()
	expiresAt = time.Now().Add(wsTicketTTL)
	_, err = db.ExecContext(ctx,
		`INSERT INTO ws_tickets (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		HashToken(ticket), userID, expiresAt)
	return ticket, expiresAt, err
}
