// computers —— BYOA computer 注册中心(#60):配对令牌(公司级持久 +
// 单机重连)、配对兑换(精确令牌/按主机名重挂/新建三路)、心跳、离线
// 扫描、设备令牌解析、agent 运行时 JWT、发现列表与升级标注。
// 对齐 已退役 TS server 的 agents/computer/registry.ts。
package computers

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

const (
	StaleMS       = 90_000   // 心跳静默阈值(daemon ~30s 一跳)
	AgentTokenTTL = 2 * 3600 // agent 运行时 JWT;daemon 到期前刷新
	latestTTL     = time.Hour
)

var pairableEngines = map[string]bool{"claude": true, "codex": true, "grok": true, "cursor": true, "zcode": true}

// hashToken 对齐 TS:sha256 → base64url(带垫)。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.URLEncoding.EncodeToString(sum[:])
}

// utf16Slice:JS slice(0,n) 按 UTF-16 码元(rune 截在代理对拆分上多算
// 半码元;TS 语义的真身是码元计数)。daemonVersion/hostName 钳位用(#94)。
func utf16Slice(s string, n int) string {
	count := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if count+w > n {
			return s[:i]
		}
		count += w
	}
	return s
}

func randB64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func broadcastComputerStatus(ctx context.Context, computerID, companyID, status string) {
	payload, _ := json.Marshal(map[string]any{
		"type": "computers.status", "computerId": computerID, "status": status, "companyId": companyID,
	})
	_ = events.PublishRaw(ctx, events.ChStatus, payload)
}

func AnnounceComputerOnline(ctx context.Context, computerID, companyID string) {
	broadcastComputerStatus(ctx, computerID, companyID, "online")
}

/* ───────────── 配对令牌 ───────────── */

// IssuePairingCode:公司级持久 add-computer 令牌(首取即铸,并发收敛)。
func IssuePairingCode(ctx context.Context, db *sql.DB, companyID string) (string, any, error) {
	var token sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT pair_token FROM companies WHERE id = $1`, companyID).Scan(&token); err != nil {
		return "", nil, err
	}
	if !token.Valid || token.String == "" {
		fresh := randB64(24)
		_, _ = db.ExecContext(ctx,
			`UPDATE companies SET pair_token = $1 WHERE id = $2 AND pair_token IS NULL`, fresh, companyID)
		_ = db.QueryRowContext(ctx,
			`SELECT pair_token FROM companies WHERE id = $1`, companyID).Scan(&token)
		if !token.Valid || token.String == "" {
			token = sql.NullString{String: fresh, Valid: true}
		}
	}
	return token.String, nil, nil // expiresInSeconds: null(持久)
}

// IssueRepairCode:单机重连令牌(绑 computer 行)。不可重连 → ok=false。
func IssueRepairCode(ctx context.Context, db *sql.DB, companyID, computerID string) (string, bool) {
	var token sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT pair_token FROM computers
		 WHERE id = $1 AND company_id = $2 AND kind <> 'cloud' AND revoked_at IS NULL LIMIT 1`,
		computerID, companyID).Scan(&token)
	if err != nil {
		return "", false
	}
	if !token.Valid || token.String == "" {
		fresh := randB64(24)
		_, _ = db.ExecContext(ctx, `
			UPDATE computers SET pair_token = $1 WHERE id = $2 AND company_id = $3 AND pair_token IS NULL`,
			fresh, computerID, companyID)
		_ = db.QueryRowContext(ctx,
			`SELECT pair_token FROM computers WHERE id = $1 AND company_id = $2`, computerID, companyID).Scan(&token)
		if !token.Valid || token.String == "" {
			token = sql.NullString{String: fresh, Valid: true}
		}
	}
	return token.String, true
}

// PairResult 对齐 pairComputer 返回。
type PairResult struct {
	ComputerID  string `json:"computerId"`
	CompanyID   string `json:"companyId"`
	DeviceToken string `json:"deviceToken"`
}

// PairComputer:三路兑换——①精确 computer 令牌重连;②公司令牌 + 同名
// (主机名)重挂;③公司令牌新建。deferBroadcast 时调用方须自 Announce。
func PairComputer(ctx context.Context, db *sql.DB, code string, hostName string, engines []string, version string, supervised *bool, deferBroadcast bool) (*PairResult, error) {
	filtered := []string{}
	for _, e := range engines {
		if pairableEngines[e] {
			filtered = append(filtered, e)
		}
	}
	enginesJSON, _ := json.Marshal(filtered)
	// TS 对应钳位按 UTF-16 码元(非 rune):代理对拆分语义一致。
	v := utf16Slice(version, 32)
	var versionArg, supervisedArg any
	if v != "" {
		versionArg = v
	}
	if supervised != nil {
		supervisedArg = *supervised
	}
	deviceToken := randB64(32)
	reported := utf16Slice(hostName, 80)
	name := reported
	if name == "" {
		name = "My computer"
	}
	cred := hashToken(deviceToken)

	// ① 精确 computer 令牌
	var id, companyID string
	err := db.QueryRowContext(ctx, `
		SELECT id, company_id FROM computers
		 WHERE pair_token = $1 AND kind <> 'cloud' AND revoked_at IS NULL LIMIT 1`, code).Scan(&id, &companyID)
	if err == nil {
		if _, err := db.ExecContext(ctx, `
			UPDATE computers SET credential_hash = $1, available_engines = $2::jsonb,
			    name = COALESCE(NULLIF($3, ''), name),
			    daemon_version = COALESCE($5, daemon_version),
			    daemon_supervised = COALESCE($6, daemon_supervised),
			    status = 'online', last_seen_at = NOW(), paired_at = NOW()
			  WHERE id = $4`,
			cred, enginesJSON, reported, id, versionArg, supervisedArg); err != nil {
			return nil, err
		}
		if !deferBroadcast {
			broadcastComputerStatus(ctx, id, companyID, "online")
		}
		return &PairResult{ComputerID: id, CompanyID: companyID, DeviceToken: deviceToken}, nil
	}

	// ② 公司令牌
	var ownerUserID sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT id, owner_user_id FROM companies WHERE pair_token = $1 LIMIT 1`, code).Scan(&companyID, &ownerUserID)
	if err != nil {
		return nil, nil // invalid pairing token
	}
	// 同名重挂
	var existing string
	err = db.QueryRowContext(ctx, `
		SELECT id FROM computers
		 WHERE company_id = $1 AND kind <> 'cloud' AND revoked_at IS NULL AND name = $2
		 ORDER BY paired_at DESC NULLS LAST LIMIT 1`, companyID, name).Scan(&existing)
	if err == nil {
		if _, err := db.ExecContext(ctx, `
			UPDATE computers SET credential_hash = $1, available_engines = $2::jsonb,
			    daemon_version = COALESCE($4, daemon_version),
			    daemon_supervised = COALESCE($5, daemon_supervised),
			    status = 'online', last_seen_at = NOW(), paired_at = NOW(), revoked_at = NULL
			  WHERE id = $3`,
			cred, enginesJSON, existing, versionArg, supervisedArg); err != nil {
			return nil, err
		}
		if !deferBroadcast {
			broadcastComputerStatus(ctx, existing, companyID, "online")
		}
		return &PairResult{ComputerID: existing, CompanyID: companyID, DeviceToken: deviceToken}, nil
	}
	// ③ 新建
	computerID := "comp-" + randHex(6)
	var ownerAny any
	if ownerUserID.Valid {
		ownerAny = ownerUserID.String
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO computers
		  (id, company_id, owner_user_id, name, kind, available_engines, status, credential_hash, paired_at, last_seen_at, daemon_version, daemon_supervised)
		VALUES ($1, $2, $3, $4, 'local', $5::jsonb, 'online', $6, NOW(), NOW(), $7, $8)`,
		computerID, companyID, ownerAny, name, enginesJSON, cred, versionArg, supervisedArg); err != nil {
		return nil, err
	}
	if !deferBroadcast {
		broadcastComputerStatus(ctx, computerID, companyID, "online")
	}
	return &PairResult{ComputerID: computerID, CompanyID: companyID, DeviceToken: deviceToken}, nil
}

// HeartbeatComputer:在线态安静 bump;离线→在线才广播。
func HeartbeatComputer(ctx context.Context, db *sql.DB, computerID, version string, supervised *bool) {
	var vArg, sArg any
	if version != "" {
		// TS version.slice(0, 32) 按 UTF-16 码元(#141 rider)。
		version = httpx.UTF16Cap(version, 32)
		vArg = version
	}
	if supervised != nil {
		sArg = *supervised
	}
	var one int
	if db.QueryRowContext(ctx, `
		UPDATE computers SET last_seen_at = NOW(), daemon_version = COALESCE($2, daemon_version),
		       daemon_supervised = COALESCE($3, daemon_supervised)
		 WHERE id = $1 AND revoked_at IS NULL AND status = 'online' RETURNING 1`,
		computerID, vArg, sArg).Scan(&one) == nil {
		return // 在线态静默
	}
	var companyID string
	if db.QueryRowContext(ctx, `
		UPDATE computers SET status = 'online', last_seen_at = NOW(), daemon_version = COALESCE($2, daemon_version),
		       daemon_supervised = COALESCE($3, daemon_supervised)
		 WHERE id = $1 AND revoked_at IS NULL RETURNING company_id`,
		computerID, vArg, sArg).Scan(&companyID) == nil {
		broadcastComputerStatus(ctx, computerID, companyID, "online")
	}
}

// SweepOfflineComputers:心跳过期 → offline(逐台广播)。云端行跳过。
func SweepOfflineComputers(ctx context.Context, db *sql.DB) {
	rows, err := db.QueryContext(ctx, `
		UPDATE computers SET status = 'offline'
		 WHERE kind <> 'cloud' AND status = 'online'
		   AND (last_seen_at IS NULL OR last_seen_at < NOW() - ($1::int * interval '1 millisecond'))
		 RETURNING id, company_id`, StaleMS)
	if err != nil {
		return
	}
	type flip struct{ id, company string }
	flips := []flip{}
	for rows.Next() {
		var f flip
		if rows.Scan(&f.id, &f.company) == nil {
			flips = append(flips, f)
		}
	}
	rows.Close()
	for _, f := range flips {
		broadcastComputerStatus(ctx, f.id, f.company, "offline")
	}
}

// StartSweepWorker:index.ts 同款 30s 周期。
func StartSweepWorker(ctx context.Context, db *sql.DB) {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				SweepOfflineComputers(ctx, db)
			}
		}
	}()
}

// ResolveDevice:设备令牌 → computer(拒已吊销;顺带 bump liveness)。
func ResolveDevice(ctx context.Context, db *sql.DB, token string) (computerID, companyID string, ok bool) {
	if token == "" {
		return "", "", false
	}
	err := db.QueryRowContext(ctx, `
		UPDATE computers SET last_seen_at = NOW()
		 WHERE credential_hash = $1 AND revoked_at IS NULL
		 RETURNING id, company_id`, hashToken(token)).Scan(&computerID, &companyID)
	return computerID, companyID, err == nil
}

/* ───────────── agent 运行时 JWT(HS256,自管) ───────────── */

// agentRuntimeSecret:开发回退密钥仅供本地/集成双跑;生产拒启守卫在
// cmd/server 的 config.ProdEnvViolations(NODE_ENV=production 且未设密钥
// 时进程直接退出,不会走到这里)。
func agentRuntimeSecret() string {
	if s := strings.TrimSpace(config.AgentRuntimeSecret()); s != "" {
		return s
	}
	return "dev-agent-runtime-secret-do-not-use-in-prod"
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// SignAgentToken 对齐 runtime/jwt.ts:{sub,companyId,scope:'agent-runner',iat,exp}。
func SignAgentToken(agentID string, companyID *string, ttlSeconds int) string {
	now := time.Now().Unix()
	if ttlSeconds <= 0 {
		ttlSeconds = 3600
	}
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	claims := map[string]any{
		"sub": agentID, "scope": "agent-runner", "iat": now, "exp": now + int64(ttlSeconds),
	}
	if companyID != nil {
		claims["companyId"] = *companyID
	} else {
		claims["companyId"] = nil
	}
	payload, _ := json.Marshal(claims)
	signingInput := b64u(header) + "." + b64u(payload)
	mac := hmac.New(sha256.New, []byte(agentRuntimeSecret()))
	mac.Write([]byte(signingInput))
	return signingInput + "." + b64u(mac.Sum(nil))
}

// VerifyAgentToken:验签+exp+scope;失败返回空 sub。
func VerifyAgentToken(token string) (agentID string, companyID *string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", nil, false
	}
	mac := hmac.New(sha256.New, []byte(agentRuntimeSecret()))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(want, got) {
		return "", nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", nil, false
	}
	var claims struct {
		Sub       string  `json:"sub"`
		CompanyID *string `json:"companyId"`
		Scope     string  `json:"scope"`
		Exp       float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Scope != "agent-runner" {
		return "", nil, false
	}
	if claims.Exp < float64(time.Now().Unix()) {
		return "", nil, false
	}
	if claims.Sub == "" {
		return "", nil, false // TS jwt.ts 'missing sub' 同拒
	}
	return claims.Sub, claims.CompanyID, true
}

// MintAgentRuntimeToken:仅当 agent 真分配在该 computer(同租户)。
func MintAgentRuntimeToken(ctx context.Context, db *sql.DB, computerID, agentID string) (string, int, bool) {
	var companyID sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT company_id FROM participants
		 WHERE id = $1 AND kind = 'agent' AND computer_id = $2 LIMIT 1`, agentID, computerID).Scan(&companyID)
	if err != nil {
		return "", 0, false
	}
	var cid *string
	if companyID.Valid {
		s := companyID.String
		cid = &s
	}
	return SignAgentToken(agentID, cid, AgentTokenTTL), AgentTokenTTL, true
}

/* ───────────── 发现与治理 ───────────── */

// AgentEntry 对齐 listAgentsForComputer 行(含引擎默认模型钉扎)。
type AgentEntry struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Role         *string `json:"role"`
	SystemPrompt *string `json:"systemPrompt"`
	Engine       *string `json:"engine"`
	Model        *string `json:"model"`
	FastModel    *string `json:"fastModel"`
	// ChatRegister:#24 聊天体语域开关(human-audience 会话说人话)。
	// 列 NOT NULL DEFAULT true;nil 仅出现在旧行迁移前的空档,按开处理。
	ChatRegister *bool `json:"chatRegister"`
}

func engineDefault(engine string) *string {
	keys := map[string]string{
		"claude": "CUMORA_DEFAULT_CLAUDE_MODEL", "codex": "CUMORA_DEFAULT_CODEX_MODEL",
		"grok": "CUMORA_DEFAULT_GROK_MODEL", "cursor": "CUMORA_DEFAULT_CURSOR_MODEL",
		"zcode": "CUMORA_DEFAULT_ZCODE_MODEL",
	}
	if k, ok := keys[engine]; ok {
		if v := strings.TrimSpace(config.Getenv(k)); v != "" {
			return &v
		}
	}
	return nil
}

func ListAgentsForComputer(ctx context.Context, db *sql.DB, computerID string) []AgentEntry {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, role, system_prompt, engine, model, fast_model, chat_register FROM participants
		 WHERE computer_id = $1 AND kind = 'agent' AND departed_at IS NULL
		 ORDER BY name ASC`, computerID)
	if err != nil {
		return []AgentEntry{}
	}
	out := []AgentEntry{}
	for rows.Next() {
		var e AgentEntry
		var role, sysPrompt, engine, model, fastModel sql.NullString
		var chatRegister sql.NullBool
		if rows.Scan(&e.ID, &e.Name, &role, &sysPrompt, &engine, &model, &fastModel, &chatRegister) == nil {
			if role.Valid {
				e.Role = &role.String
			}
			if sysPrompt.Valid {
				e.SystemPrompt = &sysPrompt.String
			}
			if engine.Valid {
				e.Engine = &engine.String
			}
			if model.Valid {
				e.Model = &model.String
			}
			if fastModel.Valid {
				e.FastModel = &fastModel.String
			}
			if chatRegister.Valid {
				e.ChatRegister = &chatRegister.Bool
			}
			// 模型钉扎:无显式 model 时按引擎默认(#60 平价;防底层 CLI 换默认)
			if e.Model == nil && e.Engine != nil {
				e.Model = engineDefault(*e.Engine)
			}
			out = append(out, e)
		}
	}
	rows.Close()
	return out
}

var (
	latestMu     sync.Mutex
	latestVer    string
	latestAt     time.Time
	latestClient = &http.Client{Timeout: 5 * time.Second}
)

// updateAPIBase:与 daemon 自更新(selfupdate.go)同源同键 —— 自家
// GitHub release;CUMORA_UPDATE_API 可覆盖(测试/镜像)。
func updateAPIBase() string {
	if v := strings.TrimSpace(os.Getenv("CUMORA_UPDATE_API")); v != "" {
		return v
	}
	return "https://api.github.com/repos/MaskedKM/cumora"
}

// getLatestDaemonVersion:自家 GitHub release 的 tag_name(daemon 自更新
// 下载面同源;1h 缓存,失败保旧)。旧实现查 npm registry 'cumora' ——
// post-fork 后该 npm 包属上游,v0.11.0 是上游版本,"设备页升级横幅"会把
// 自托管部署指向上游(2026-09-01 实锾示警);#67 把 daemon 下载面改到
// 自家 release 时这里漏改,现对齐。
func getLatestDaemonVersion() *string {
	latestMu.Lock()
	defer latestMu.Unlock()
	now := time.Now()
	if latestVer != "" && now.Sub(latestAt) < latestTTL {
		return &latestVer
	}
	res, err := latestClient.Get(updateAPIBase() + "/releases/latest")
	if err == nil && res.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
		res.Body.Close()
		var parsed struct {
			TagName string `json:"tag_name"`
		}
		if json.Unmarshal(body, &parsed) == nil && parsed.TagName != "" {
			latestVer = parsed.TagName
			latestAt = now
		}
	}
	if latestVer == "" {
		return nil
	}
	return &latestVer
}

// 测试缝:重置/注入 latest 缓存(registry_latest_test 用)。
func resetLatestForTest() {
	latestMu.Lock()
	defer latestMu.Unlock()
	latestVer = ""
	latestAt = time.Time{}
}

func setLatestForTest(v string, at time.Time) {
	latestMu.Lock()
	defer latestMu.Unlock()
	latestVer = v
	latestAt = at
}

// versionGt:点分数值比较(预发布后缀忽略,与 TS 同)。
func versionGt(a, b string) bool {
	pa := parseVer(a)
	pb := parseVer(b)
	for i := 0; i < 3; i++ {
		if pa[i] > pb[i] {
			return true
		}
		if pa[i] < pb[i] {
			return false
		}
	}
	return false
}

func parseVer(v string) [3]int {
	var out [3]int
	// 探针改吃 GitHub tag_name(带 v 前缀)后必须剥掉——"v1.1.0" 首段
	// Atoi 失败落 0,1.0.0 起横幅比较会静默漏报(#278 评审 P2-1);
	// 对齐 daemon versionTriple 的剥前缀语义。
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	for i, p := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(strings.TrimSpace(p))
		out[i] = n
	}
	return out
}

// ListComputers:公司可见(云行排除)+ 升级标注。
func ListComputers(ctx context.Context, db *sql.DB, companyID string) []map[string]any {
	rows, err := db.QueryContext(ctx, `
		SELECT id, company_id, owner_user_id, name, kind, available_engines, status,
		       last_seen_at, paired_at, revoked_at, created_at, daemon_version, daemon_supervised
		  FROM computers
		 WHERE company_id = $1 AND kind <> 'cloud' AND revoked_at IS NULL
		 ORDER BY created_at ASC`, companyID)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()
	latest := getLatestDaemonVersion()
	out := []map[string]any{}
	for rows.Next() {
		var id, companyID, name, kind, status string
		var owner sql.NullString
		var engines []byte
		var lastSeen, pairedAt, revokedAt sql.NullTime
		var createdAt time.Time
		var daemonVersion sql.NullString
		var supervised sql.NullBool
		if rows.Scan(&id, &companyID, &owner, &name, &kind, &engines, &status,
			&lastSeen, &pairedAt, &revokedAt, &createdAt, &daemonVersion, &supervised) != nil {
			continue
		}
		var enginesAny any
		if len(engines) > 0 {
			var arr []string
			if json.Unmarshal(engines, &arr) == nil {
				enginesAny = arr
			}
		}
		row := map[string]any{
			"id": id, "company_id": companyID, "owner_user_id": nullStr(owner), "name": name,
			"kind": kind, "available_engines": enginesAny, "status": status,
			"last_seen_at": nullTime(lastSeen), "paired_at": nullTime(pairedAt),
			"revoked_at": nullTime(revokedAt), "created_at": createdAt.UTC(),
			"daemon_version": nullStr(daemonVersion), "daemon_supervised": nullBool(supervised),
			"latest_daemon_version": latest,
			"daemon_outdated":       latest != nil && (!daemonVersion.Valid || daemonVersion.String == "" || versionGt(*latest, daemonVersion.String)),
		}
		out = append(out, row)
	}
	return out
}

// AssignAgentToComputer:请求引擎须在 advertised,否则取第一台;
// 返回 resolved {kind, engine}。
func AssignAgentToComputer(ctx context.Context, db *sql.DB, agentID, companyID, computerID, engine string) (map[string]any, bool) {
	var kind string
	var enginesJSON []byte
	err := db.QueryRowContext(ctx, `
		SELECT kind, available_engines FROM computers
		 WHERE id = $1 AND company_id = $2 AND kind <> 'cloud' AND revoked_at IS NULL LIMIT 1`,
		computerID, companyID).Scan(&kind, &enginesJSON)
	if err != nil {
		return nil, false
	}
	var advertised []string
	_ = json.Unmarshal(enginesJSON, &advertised)
	var requested string
	if pairableEngines[engine] {
		requested = engine
	}
	pick := ""
	for _, a := range advertised {
		if requested != "" && a == requested {
			pick = a
			break
		}
	}
	if pick == "" && len(advertised) > 0 {
		pick = advertised[0]
	}
	if pick == "" {
		return nil, false
	}
	res, err := db.ExecContext(ctx, `
		UPDATE participants SET computer_id = $1, engine = $2
		 WHERE id = $3 AND company_id = $4 AND kind = 'agent'`,
		computerID, pick, agentID, companyID)
	if err != nil {
		return nil, false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, false
	}
	return map[string]any{"kind": kind, "engine": pick}, true
}

// RevokeComputer:吊销(令牌+派生 JWT 失效;agents 离线)。
func RevokeComputer(ctx context.Context, db *sql.DB, computerID, companyID string) bool {
	res, err := db.ExecContext(ctx, `
		UPDATE computers SET revoked_at = NOW(), status = 'offline', credential_hash = NULL
		 WHERE id = $1 AND company_id = $2 AND kind <> 'cloud' AND revoked_at IS NULL`,
		computerID, companyID)
	if err != nil {
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	broadcastComputerStatus(context.Background(), computerID, companyID, "offline")
	return true
}

func nullStr(ns sql.NullString) any {
	if ns.Valid {
		return ns.String
	}
	return nil
}

func nullTime(nt sql.NullTime) any {
	if nt.Valid {
		return nt.Time.UTC()
	}
	return nil
}

func nullBool(nb sql.NullBool) any {
	if nb.Valid {
		return nb.Bool
	}
	return nil
}
