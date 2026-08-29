// domains/shipping —— #125(#117-f):shipping 全子面(16 路由)。逐段
// 对齐 api/shipping-router.ts(880 行):feature 契约机(draft→…→learned,
// 迁移前置断言)、invariants/verifications(建设者不得自证)/releases
// (staged production + readback)/regressions/friction(读回失败自动
// 升 critical 摩擦)。事件流 shipping_events 记每次变更。
package shipping

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

/* ───────── 错误与公共件 ───────── */

type shippingError struct {
	status int
	msg    string
}

func (e *shippingError) Error() string { return e.msg }

func fail(status int, format string, a ...any) *shippingError {
	return &shippingError{status, fmt.Sprintf(format, a...)}
}

func writeShipError(w http.ResponseWriter, r *http.Request, err error) bool {
	var se *shippingError
	if se2, ok := err.(*shippingError); ok {
		se = se2
	}
	if se != nil {
		httpx.WriteError(w, se.status, se.msg)
		return true
	}
	httpx.WriteInternalError(w, r, err)
	return true
}

func randID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func nullStr(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

func nullTime(nt sql.NullTime) any {
	if !nt.Valid {
		return nil
	}
	return isoTime(nt.Time)
}

/* ───────── body 字段访问(undefined 语义:键在才算有) ───────── */

type shipBody map[string]json.RawMessage

func (b shipBody) has(k string) bool { _, ok := b[k]; return ok }

// text:TS typeof string → trim → 截 max;非字符串给 ”。
func (b shipBody) text(k string, max int) string {
	raw, ok := b[k]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	s = strings.TrimSpace(s)
	// TS slice 按 UTF-16 码元;Go 按 rune 近似(文本上限守门,非精确对账)。
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

func (b shipBody) optText(k string, max int) sql.NullString {
	t := b.text(k, max)
	if t == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: t, Valid: true}
}

func (b shipBody) boolean(k string, fallback bool) bool {
	raw, ok := b[k]
	if !ok {
		return fallback
	}
	var v bool
	if json.Unmarshal(raw, &v) != nil {
		return fallback
	}
	return v
}

// stringArray:字符串项 trim 去重去空,截 max。
func (b shipBody) stringArray(k string, max int) []string {
	raw, ok := b[k]
	if !ok {
		return []string{}
	}
	var arr []any
	if json.Unmarshal(raw, &arr) != nil {
		return []string{}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, v := range arr {
		s, ok := v.(string)
		if !ok {
			continue
		}
		t := strings.TrimSpace(s)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= max {
			break
		}
	}
	return out
}

// jsonArray:任意数组截 max(非数组 → 空)。
func (b shipBody) jsonArray(k string, max int) []any {
	raw, ok := b[k]
	if !ok {
		return []any{}
	}
	var arr []any
	if json.Unmarshal(raw, &arr) != nil {
		return []any{}
	}
	if len(arr) > max {
		return arr[:max]
	}
	return arr
}

func (b shipBody) enumValue(k string, allowed map[string]bool, fallback string) string {
	s := b.text(k, 10_000)
	if allowed[s] {
		return s
	}
	return fallback
}

// isoOrNull:空 → NULL;非法 → 400 invalid timestamp(TS 同款)。
func (b shipBody) isoOrNull(k string) (sql.NullTime, *shippingError) {
	s := b.text(k, 100)
	if s == "" {
		return sql.NullTime{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return sql.NullTime{}, fail(http.StatusBadRequest, "invalid timestamp")
	}
	return sql.NullTime{Time: t, Valid: true}, nil
}

var (
	featureStatuses = map[string]bool{"draft": true, "contract": true, "building": true, "verifying": true,
		"ready": true, "releasing": true, "watching": true, "learned": true, "paused": true, "archived": true}
	priorities          = map[string]bool{"critical": true, "high": true, "medium": true, "low": true}
	riskLevels          = map[string]bool{"critical": true, "high": true, "medium": true, "low": true}
	invariantKinds      = map[string]bool{"behavior": true, "architecture": true, "data": true, "security": true, "performance": true, "ux": true, "operability": true}
	verificationMethods = map[string]bool{"user_path": true, "property": true, "trace": true,
		"data_reconciliation": true, "design_qa": true, "security": true, "performance": true, "release_note": true}
	verificationStatuses = map[string]bool{"pending": true, "running": true, "passed": true, "failed": true, "waived": true}
	releaseEnvironments  = map[string]bool{"development": true, "staging": true, "canary": true, "production": true}
	frictionSeverities   = map[string]bool{"critical": true, "high": true, "medium": true, "low": true}
	frictionFrequencies  = map[string]bool{"once": true, "occasional": true, "frequent": true, "constant": true}
	frictionStatuses     = map[string]bool{"open": true, "triaged": true, "planned": true, "resolved": true, "dismissed": true}
	regressionKinds      = map[string]bool{"automated": true, "benchmark": true, "manual_replay": true, "monitor": true}
	regressionStatuses   = map[string]bool{"active": true, "passing": true, "failing": true, "disabled": true}
)

var featureTransitions = map[string]map[string]bool{
	"draft":     {"contract": true, "paused": true, "archived": true},
	"contract":  {"building": true, "draft": true, "paused": true, "archived": true},
	"building":  {"verifying": true, "paused": true, "archived": true},
	"verifying": {"building": true, "ready": true, "paused": true, "archived": true},
	"ready":     {"releasing": true, "building": true, "paused": true, "archived": true},
	"releasing": {"watching": true, "ready": true, "paused": true, "archived": true},
	"watching":  {"learned": true, "releasing": true, "paused": true, "archived": true},
	"learned":   {"building": true, "archived": true},
	"paused":    {"draft": true, "contract": true, "building": true, "verifying": true, "ready": true, "archived": true},
	"archived":  {},
}

func mustJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// dbtx:db 与事务共用的最小执行面。
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func recordEvent(ctx context.Context, db dbtx, companyID, featureID, actorID, kind string, data any) error {
	var fid, aid any
	if featureID != "" {
		fid = featureID
	}
	if actorID != "" {
		aid = actorID
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO shipping_events (id, company_id, feature_id, actor_id, kind, data)
		   VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		randID("se"), companyID, fid, aid, kind, mustJSONString(orEmptyMap(data)))
	return err
}

func orEmptyMap(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

type shipFeatureCore struct {
	id         string
	status     string
	builderIDs []string
	title      string
	priority   string
	riskLevel  string
}

func requireFeature(ctx context.Context, db *sql.DB, companyID, featureID string) (*shipFeatureCore, *shippingError) {
	var f shipFeatureCore
	var builders []byte
	err := db.QueryRowContext(ctx,
		`SELECT id, status, builder_ids, title, priority, risk_level
		   FROM shipping_features WHERE id = $1 AND company_id = $2`,
		featureID, companyID).Scan(&f.id, &f.status, &builders, &f.title, &f.priority, &f.riskLevel)
	if err != nil {
		return nil, fail(http.StatusNotFound, "shipping feature not found")
	}
	_ = json.Unmarshal(builders, &f.builderIDs)
	if f.builderIDs == nil {
		f.builderIDs = []string{}
	}
	return &f, nil
}

func assertParticipants(ctx context.Context, db *sql.DB, companyID string, ids []string) *shippingError {
	if len(ids) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id FROM participants WHERE company_id = $1 AND id = ANY($2::text[]) AND departed_at IS NULL`,
		companyID, pqTextArray(ids))
	if err != nil {
		return fail(http.StatusInternalServerError, "%s", err.Error())
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		found[id] = true
	}
	var missing []string
	for _, id := range ids {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fail(http.StatusBadRequest, "unknown active participant(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

type featureLinks struct {
	projectID, conversationID, documentID, boardCardID sql.NullString
}

// assertFeatureLinks:四链接各自属本租户才放行;TS Promise.all 顺序取
// 首个失效报错 —— Go 按同序检查。
func assertFeatureLinks(ctx context.Context, db *sql.DB, companyID string, links featureLinks) *shippingError {
	type check struct {
		valid bool
		name  string
		sql   string
		arg   sql.NullString
	}
	checks := []check{
		{links.projectID.Valid, "project", `SELECT 1 FROM projects WHERE id = $1 AND company_id = $2`, links.projectID},
		{links.conversationID.Valid, "conversation", `SELECT 1 FROM conversations WHERE id = $1 AND company_id = $2`, links.conversationID},
		{links.documentID.Valid, "document", `SELECT 1 FROM documents WHERE id = $1 AND company_id = $2`, links.documentID},
		{links.boardCardID.Valid, "board card", `SELECT 1 FROM board_cards c JOIN boards b ON b.id = c.board_id WHERE c.id = $1 AND b.company_id = $2`, links.boardCardID},
	}
	for _, c := range checks {
		if !c.valid {
			continue
		}
		var one int
		if db.QueryRowContext(ctx, c.sql, c.arg.String, companyID).Scan(&one) != nil {
			return fail(http.StatusBadRequest, "%s does not belong to this company", c.name)
		}
	}
	return nil
}

func assertConversationMessageLinks(ctx context.Context, db *sql.DB, companyID string, conversationID, messageId sql.NullString) *shippingError {
	if !conversationID.Valid && !messageId.Valid {
		return nil
	}
	var one int
	err := db.QueryRowContext(ctx, `
		SELECT 1
		  FROM conversations c
		  LEFT JOIN messages m ON m.conversation_id = c.id AND ($2::text IS NULL OR m.id = $2)
		 WHERE c.company_id = $1 AND ($3::text IS NULL OR c.id = $3)
		   AND ($2::text IS NULL OR m.id IS NOT NULL)
		 LIMIT 1`,
		companyID, nullStrPtr(messageId), nullStrPtr(conversationID)).Scan(&one)
	if err != nil {
		return fail(http.StatusBadRequest, "conversation/message evidence does not belong to this company")
	}
	return nil
}

func nullStrPtr(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

// pqTextArray:{a,b} 字面量 —— pgx 直收 text[](无需 pq 依赖)。
func pqTextArray(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = `"` + strings.ReplaceAll(strings.ReplaceAll(id, `\`, `\\`), `"`, `\"`) + `"`
	}
	return "{" + strings.Join(quoted, ",") + "}"
}

/* ───────── featureDetail(全嵌套详情) ───────── */

func scanJSONStrings(b []byte) []string {
	var out []string
	_ = json.Unmarshal(b, &out)
	if out == nil {
		out = []string{}
	}
	return out
}

func scanJSONAny(ns sql.NullString) any {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var v any
	if json.Unmarshal([]byte(ns.String), &v) != nil {
		return []any{}
	}
	return v
}

func detailFeature(ctx context.Context, db *sql.DB, companyID, featureID string) (map[string]any, *shippingError) {
	var projectID, conversationID, documentID, boardCardID, releaseTarget, createdBy, updatedBy sql.NullString
	var builderIDs []byte
	var createdAt, updatedAt, archivedAt sql.NullTime
	var id, title, problem, desiredOutcome, contractSummary, status, priority, riskLevel string
	err := db.QueryRowContext(ctx, `
		SELECT id, title, problem, desired_outcome, contract_summary, status, priority, risk_level,
		       release_target, builder_ids, project_id, conversation_id, document_id, board_card_id,
		       created_by, updated_by, created_at, updated_at, archived_at
		  FROM shipping_features WHERE id = $1 AND company_id = $2`, featureID, companyID).
		Scan(&id, &title, &problem, &desiredOutcome, &contractSummary, &status, &priority, &riskLevel,
			&releaseTarget, &builderIDs, &projectID, &conversationID, &documentID, &boardCardID,
			&createdBy, &updatedBy, &createdAt, &updatedAt, &archivedAt)
	if err != nil {
		return nil, fail(http.StatusNotFound, "shipping feature not found")
	}
	invariants, err := detailInvariants(ctx, db, featureID)
	if err != nil {
		return nil, fail(http.StatusInternalServerError, "%s", err.Error())
	}
	verifications, err := detailVerifications(ctx, db, featureID)
	if err != nil {
		return nil, fail(http.StatusInternalServerError, "%s", err.Error())
	}
	releases, err := detailReleases(ctx, db, featureID)
	if err != nil {
		return nil, fail(http.StatusInternalServerError, "%s", err.Error())
	}
	frictions, err := detailFrictions(ctx, db, featureID)
	if err != nil {
		return nil, fail(http.StatusInternalServerError, "%s", err.Error())
	}
	regressions, err := detailRegressions(ctx, db, featureID)
	if err != nil {
		return nil, fail(http.StatusInternalServerError, "%s", err.Error())
	}
	events, err := detailEvents(ctx, db, featureID)
	if err != nil {
		return nil, fail(http.StatusInternalServerError, "%s", err.Error())
	}
	return map[string]any{
		"id": id, "title": title, "problem": problem, "desiredOutcome": desiredOutcome,
		"contractSummary": contractSummary, "status": status, "priority": priority, "riskLevel": riskLevel,
		"releaseTarget": nullStr(releaseTarget), "builderIds": scanJSONStrings(builderIDs),
		"projectId": nullStr(projectID), "conversationId": nullStr(conversationID),
		"documentId": nullStr(documentID), "boardCardId": nullStr(boardCardID),
		"createdBy": nullStr(createdBy), "updatedBy": nullStr(updatedBy),
		"createdAt": nullTime(createdAt), "updatedAt": nullTime(updatedAt), "archivedAt": nullTime(archivedAt),
		"invariants": invariants, "verifications": verifications, "releases": releases,
		"frictions": frictions, "regressions": regressions, "events": events,
	}, nil
}

func detailInvariants(ctx context.Context, db *sql.DB, featureID string) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, title, description, kind, required, position, created_by, created_at, updated_at
		  FROM shipping_invariants WHERE feature_id = $1 ORDER BY position ASC, created_at ASC`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, title, kind string
		var description sql.NullString
		var required bool
		var position int
		var createdBy sql.NullString
		var createdAt, updatedAt sql.NullTime
		if rows.Scan(&id, &title, &description, &kind, &required, &position, &createdBy, &createdAt, &updatedAt) != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "title": title, "description": nullStr(description), "kind": kind,
			"required": required, "position": position,
			"createdBy": nullStr(createdBy), "createdAt": nullTime(createdAt), "updatedAt": nullTime(updatedAt),
		})
	}
	return out, rows.Err()
}

func detailVerifications(ctx context.Context, db *sql.DB, featureID string) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, invariant_id, title, description, method, required, status, owner_id, verified_by_id,
		       builder_ids, evidence, notes, position, due_at, completed_at, created_by, created_at, updated_at
		  FROM shipping_verifications WHERE feature_id = $1 ORDER BY position ASC, created_at ASC`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, title, method, status string
		var invariantID, description, ownerID, verifiedByID, notes, createdBy sql.NullString
		var required bool
		var position int
		var builderIDs, evidence []byte
		var dueAt, completedAt, createdAt, updatedAt sql.NullTime
		if rows.Scan(&id, &invariantID, &title, &description, &method, &required, &status, &ownerID, &verifiedByID,
			&builderIDs, &evidence, &notes, &position, &dueAt, &completedAt, &createdBy, &createdAt, &updatedAt) != nil {
			continue
		}
		var evidenceJSON sql.NullString
		if len(evidence) > 0 {
			evidenceJSON = sql.NullString{String: string(evidence), Valid: true}
		}
		out = append(out, map[string]any{
			"id": id, "invariantId": nullStr(invariantID), "title": title, "description": nullStr(description),
			"method": method, "required": required, "status": status,
			"ownerId": nullStr(ownerID), "verifiedById": nullStr(verifiedByID),
			"builderIds": scanJSONStrings(builderIDs), "evidence": scanJSONAny(evidenceJSON),
			"notes": nullStr(notes), "position": position,
			"dueAt": nullTime(dueAt), "completedAt": nullTime(completedAt),
			"createdBy": nullStr(createdBy), "createdAt": nullTime(createdAt), "updatedAt": nullTime(updatedAt),
		})
	}
	return out, rows.Err()
}

func detailReleases(ctx context.Context, db *sql.DB, featureID string) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, environment, status, version, commit_sha, started_by, approved_by,
		       release_notes, rollback_plan, known_gaps, baseline, smoke_evidence,
		       readback_due_at, readback_status, readback_evidence,
		       started_at, completed_at, rolled_back_at, rollback_reason, created_at, updated_at
		  FROM shipping_releases WHERE feature_id = $1 ORDER BY created_at DESC`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, environment, status string
		var version, commitSha, startedBy, approvedBy, releaseNotes, rollbackPlan sql.NullString
		var knownGaps, baseline, smokeEvidence, readbackEvidence []byte
		var readbackDueAt, startedAt, completedAt, rolledBackAt, createdAt, updatedAt sql.NullTime
		var readbackStatus, rollbackReason sql.NullString
		if rows.Scan(&id, &environment, &status, &version, &commitSha, &startedBy, &approvedBy,
			&releaseNotes, &rollbackPlan, &knownGaps, &baseline, &smokeEvidence,
			&readbackDueAt, &readbackStatus, &readbackEvidence,
			&startedAt, &completedAt, &rolledBackAt, &rollbackReason, &createdAt, &updatedAt) != nil {
			continue
		}
		jsonOrNull := func(b []byte) any {
			if len(b) == 0 {
				return nil
			}
			var v any
			if json.Unmarshal(b, &v) != nil {
				return []any{}
			}
			return v
		}
		out = append(out, map[string]any{
			"id": id, "environment": environment, "status": status,
			"version": nullStr(version), "commitSha": nullStr(commitSha),
			"startedBy": nullStr(startedBy), "approvedBy": nullStr(approvedBy),
			"releaseNotes": nullStr(releaseNotes), "rollbackPlan": nullStr(rollbackPlan),
			"knownGaps": jsonOrNull(knownGaps), "baseline": jsonOrNull(baseline),
			"smokeEvidence": jsonOrNull(smokeEvidence),
			"readbackDueAt": nullTime(readbackDueAt), "readbackStatus": nullStr(readbackStatus),
			"readbackEvidence": jsonOrNull(readbackEvidence),
			"startedAt":        nullTime(startedAt), "completedAt": nullTime(completedAt),
			"rolledBackAt": nullTime(rolledBackAt), "rollbackReason": nullStr(rollbackReason),
			"createdAt": nullTime(createdAt), "updatedAt": nullTime(updatedAt),
		})
	}
	return out, rows.Err()
}

func detailFrictions(ctx context.Context, db *sql.DB, featureID string) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, title, description, source, severity, frequency, status, reporter_id,
		       conversation_id, message_id, evidence, occurrence_count, first_seen_at, last_seen_at
		  FROM shipping_friction_reports WHERE feature_id = $1 ORDER BY last_seen_at DESC`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, title, source, severity, frequency, status string
		var description, reporterID, conversationID, messageID sql.NullString
		var evidence []byte
		var occurrenceCount int
		var firstSeenAt, lastSeenAt sql.NullTime
		if rows.Scan(&id, &title, &description, &source, &severity, &frequency, &status, &reporterID,
			&conversationID, &messageID, &evidence, &occurrenceCount, &firstSeenAt, &lastSeenAt) != nil {
			continue
		}
		var evidenceJSON sql.NullString
		if len(evidence) > 0 {
			evidenceJSON = sql.NullString{String: string(evidence), Valid: true}
		}
		out = append(out, map[string]any{
			"id": id, "title": title, "description": nullStr(description), "source": source,
			"severity": severity, "frequency": frequency, "status": status,
			"reporterId": nullStr(reporterID), "conversationId": nullStr(conversationID),
			"messageId": nullStr(messageID), "evidence": scanJSONAny(evidenceJSON),
			"occurrenceCount": occurrenceCount, "firstSeenAt": nullTime(firstSeenAt), "lastSeenAt": nullTime(lastSeenAt),
		})
	}
	return out, rows.Err()
}

func detailRegressions(ctx context.Context, db *sql.DB, featureID string) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, invariant_id, source_verification_id, title, kind, command, expected, status,
		       last_result, last_evidence, last_run_at, created_by, created_at, updated_at
		  FROM shipping_regressions WHERE feature_id = $1 ORDER BY updated_at DESC`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, title, kind, status string
		var invariantID, sourceVerificationID, command, expected, lastResult, createdBy sql.NullString
		var lastEvidence []byte
		var lastRunAt, createdAt, updatedAt sql.NullTime
		if rows.Scan(&id, &invariantID, &sourceVerificationID, &title, &kind, &command, &expected, &status,
			&lastResult, &lastEvidence, &lastRunAt, &createdBy, &createdAt, &updatedAt) != nil {
			continue
		}
		var lastEvidenceJSON sql.NullString
		if len(lastEvidence) > 0 {
			lastEvidenceJSON = sql.NullString{String: string(lastEvidence), Valid: true}
		}
		out = append(out, map[string]any{
			"id": id, "invariantId": nullStr(invariantID), "sourceVerificationId": nullStr(sourceVerificationID),
			"title": title, "kind": kind, "command": nullStr(command), "expected": nullStr(expected),
			"status": status, "lastResult": nullStr(lastResult), "lastEvidence": scanJSONAny(lastEvidenceJSON),
			"lastRunAt": nullTime(lastRunAt), "createdBy": nullStr(createdBy),
			"createdAt": nullTime(createdAt), "updatedAt": nullTime(updatedAt),
		})
	}
	return out, rows.Err()
}

func detailEvents(ctx context.Context, db *sql.DB, featureID string) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, actor_id, kind, data, created_at
		  FROM shipping_events WHERE feature_id = $1 ORDER BY created_at ASC`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, kind string
		var actorID sql.NullString
		var data []byte
		var createdAt sql.NullTime
		if rows.Scan(&id, &actorID, &kind, &data, &createdAt) != nil {
			continue
		}
		var dataJSON sql.NullString
		if len(data) > 0 {
			dataJSON = sql.NullString{String: string(data), Valid: true}
		}
		out = append(out, map[string]any{
			"id": id, "actorId": nullStr(actorID), "kind": kind,
			"data": scanJSONAny(dataJSON), "createdAt": nullTime(createdAt),
		})
	}
	return out, rows.Err()
}

/* ───────── 迁移前置断言(assertTransitionReady) ───────── */

func assertTransitionReady(ctx context.Context, db *sql.DB, featureID, from, to string) *shippingError {
	if to == "contract" {
		var problem, desiredOutcome, contractSummary string
		_ = db.QueryRowContext(ctx,
			`SELECT problem, desired_outcome, contract_summary FROM shipping_features WHERE id = $1`, featureID).
			Scan(&problem, &desiredOutcome, &contractSummary)
		if strings.TrimSpace(problem) == "" || strings.TrimSpace(desiredOutcome) == "" || strings.TrimSpace(contractSummary) == "" {
			return fail(http.StatusConflict, "problem, desired outcome, and contract summary are required before contract")
		}
	}
	if to == "building" {
		var builders, invariants int
		_ = db.QueryRowContext(ctx, `
			SELECT jsonb_array_length(builder_ids)::int,
			       (SELECT count(*)::int FROM shipping_invariants WHERE feature_id = $1)
			  FROM shipping_features WHERE id = $1`, featureID).Scan(&builders, &invariants)
		if builders == 0 {
			return fail(http.StatusConflict, "at least one builder is required before building")
		}
		if invariants == 0 {
			return fail(http.StatusConflict, "at least one invariant is required before building")
		}
	}
	if to == "verifying" {
		var missingOwner, uncovered int
		_ = db.QueryRowContext(ctx, `
			SELECT
			   count(*) FILTER (WHERE required AND owner_id IS NULL)::int,
			   (SELECT count(*)::int
			      FROM shipping_invariants i
			     WHERE i.feature_id = $1 AND i.required
			       AND NOT EXISTS (SELECT 1 FROM shipping_verifications v WHERE v.invariant_id = i.id AND v.required)) AS uncovered
			  FROM shipping_verifications WHERE feature_id = $1`, featureID).Scan(&missingOwner, &uncovered)
		if uncovered > 0 {
			return fail(http.StatusConflict, "every required invariant needs a required verification square")
		}
		if missingOwner > 0 {
			return fail(http.StatusConflict, "every required verification square needs an owner")
		}
	}
	if to == "ready" {
		var remaining, userPath, trace, releaseNote int
		_ = db.QueryRowContext(ctx, `
			SELECT
			   count(*) FILTER (WHERE required AND status <> 'passed')::int,
			   count(*) FILTER (WHERE required AND method = 'user_path' AND status = 'passed')::int,
			   count(*) FILTER (WHERE required AND method = 'trace' AND status = 'passed')::int,
			   count(*) FILTER (WHERE required AND method = 'release_note' AND status = 'passed')::int
			  FROM shipping_verifications WHERE feature_id = $1`, featureID).
			Scan(&remaining, &userPath, &trace, &releaseNote)
		if remaining > 0 {
			return fail(http.StatusConflict, "%d required verification square(s) are not passed", remaining)
		}
		if userPath == 0 || trace == 0 || releaseNote == 0 {
			return fail(http.StatusConflict, "ready requires passed user-path, trace-coverage, and release-note squares")
		}
	}
	if to == "watching" {
		var count int
		_ = db.QueryRowContext(ctx, `
			SELECT count(*)::int FROM shipping_releases
			  WHERE feature_id = $1 AND environment = 'production' AND status = 'succeeded'`, featureID).Scan(&count)
		if count == 0 {
			return fail(http.StatusConflict, "watching requires a successful production release")
		}
	}
	if to == "learned" {
		var passed, failing int
		_ = db.QueryRowContext(ctx, `
			SELECT
			   (SELECT count(*)::int FROM shipping_releases WHERE feature_id = $1 AND environment = 'production' AND readback_status = 'passed'),
			   (SELECT count(*)::int FROM shipping_regressions WHERE feature_id = $1 AND status = 'failing')`, featureID).
			Scan(&passed, &failing)
		if passed == 0 {
			return fail(http.StatusConflict, "learned requires a passed production readback")
		}
		if failing > 0 {
			return fail(http.StatusConflict, "learned is blocked by failing regressions")
		}
	}
	if !featureTransitions[from][to] {
		return fail(http.StatusConflict, "invalid feature transition: %s → %s", from, to)
	}
	return nil
}
