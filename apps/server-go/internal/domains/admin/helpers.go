// domains/admin/helpers —— #124 子面共享小件:gravatar 兜底、审计行、
// 时间/空值 wire 形状、随机 id。与 core/oauth.go 同源语义(core 侧未导
// 出的实现就地小抄,语义一致、互不依赖)。
package admin

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// avatarOrGravatar:管理行恒有头像 —— 库里 NULL(存量/种子号)落
// gravatar identicon 兜底。
func avatarOrGravatar(ns sql.NullString, email string) any {
	if ns.Valid && ns.String != "" {
		return ns.String
	}
	low := strings.ToLower(strings.TrimSpace(email))
	sum := md5.Sum([]byte(low))
	return fmt.Sprintf("https://www.gravatar.com/avatar/%x?d=identicon&s=256", sum)
}

// isoTime:wire 时间形状对齐 TS(pg Date → JSON ISO,毫秒精度 + Z)。
func isoTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func nullTimeAny(nt sql.NullTime) any {
	if !nt.Valid {
		return nil
	}
	return isoTime(nt.Time)
}

func nullStrAny(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

// adminAudit:audit_events 直写(fire-and-forget,同 core oauthAudit)。
func adminAudit(db *sql.DB, userID, kind string, detail map[string]any) {
	var detailJSON any
	if detail != nil {
		b, _ := json.Marshal(detail)
		detailJSON = string(b)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.ExecContext(ctx,
			`INSERT INTO audit_events (user_id, company_id, ip, user_agent, kind, detail)
			   VALUES ($1, NULL, NULL, NULL, $2, $3::jsonb)`, userID, kind, detailJSON)
	}()
}

// randHexID:TS randomUUID().slice(0,N) 的十六进制段同型。
func randHexID(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// isAllowlistedAdmin:CUMORA_ADMIN_EMAILS 白名单(oauth 同款语义;
// approve 建号判 is_admin 用)。
func isAllowlistedAdmin(email string) bool {
	mine := strings.ToLower(strings.TrimSpace(email))
	for _, e := range strings.Split(os.Getenv("CUMORA_ADMIN_EMAILS"), ",") {
		if strings.ToLower(strings.TrimSpace(e)) == mine && mine != "" {
			return true
		}
	}
	return false
}
