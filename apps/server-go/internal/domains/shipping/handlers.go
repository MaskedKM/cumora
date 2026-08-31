// shipping/handlers —— #125(#117-f):16 路由 HTTP 面。feature 增删改查/
// 状态迁移 + invariants/verifications/releases(含 action 状态机)/
// friction/regressions,逐段对齐 api/shipping-router.ts 307–880。
// #187 批次 6:shipping tag 走 ServerInterface,注册串由契约生成物接管。
package shipping

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/shipping"
	dbpkg "github.com/MaskedKM/cumora/apps/server-go/internal/db"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/jsonx"
)

// Server:shipping tag(16 路由)的域实现。方法体自原闭包工厂逐字上移;
// 规范通配符长名(invariantId/verificationId/releaseId/regressionId)
// 替换手写短名(iid/vid/rid/gid)——PathValue 按名取值,迁移即对账。
type Server struct{ DB *sql.DB }

var _ contract.ServerInterface = (*Server)(nil)

func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = contract.HandlerFromMux(&Server{DB: db}, mux)
}

func readBody(r *http.Request) shipBody {
	var body shipBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body == nil {
		body = shipBody{}
	}
	return body
}

/* ───────── overview / feature 读 ───────── */

func (s *Server) GetShippingOverview(w http.ResponseWriter, r *http.Request) {
	_, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	features := []map[string]any{}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT f.id, f.title, f.status, f.priority, f.risk_level, f.release_target, f.builder_ids,
		       f.project_id, f.updated_at,
		       count(v.id) FILTER (WHERE v.required)::int,
		       count(v.id) FILTER (WHERE v.required AND v.status = 'passed')::int,
		       count(v.id) FILTER (WHERE v.status = 'failed')::int
		  FROM shipping_features f
		  LEFT JOIN shipping_verifications v ON v.feature_id = f.id
		 WHERE f.company_id = $1 AND f.status <> 'archived'
		 GROUP BY f.id ORDER BY
		   CASE f.priority WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
		   f.updated_at DESC`, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, status, priority, riskLevel string
		var releaseTarget, projectID sql.NullString
		var builderIDs []byte
		var updatedAt sql.NullTime
		var requiredSquares, passedSquares, failedSquares int
		if rows.Scan(&id, &title, &status, &priority, &riskLevel, &releaseTarget, &builderIDs,
			&projectID, &updatedAt, &requiredSquares, &passedSquares, &failedSquares) != nil {
			continue
		}
		features = append(features, map[string]any{
			"id": id, "title": title, "status": status, "priority": priority,
			"riskLevel": riskLevel, "releaseTarget": nullStr(releaseTarget),
			"builderIds": scanJSONStrings(builderIDs), "projectId": nullStr(projectID),
			"updatedAt":       nullTime(updatedAt),
			"requiredSquares": requiredSquares, "passedSquares": passedSquares, "failedSquares": failedSquares,
		})
	}
	friction := []map[string]any{}
	frows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, feature_id, title, description, source, severity, frequency, status,
		       occurrence_count, last_seen_at
		  FROM shipping_friction_reports
		 WHERE company_id = $1 AND status IN ('open','triaged','planned')
		 ORDER BY CASE severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
		          last_seen_at DESC`, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer frows.Close()
	for frows.Next() {
		var id, featureID, title, source, severity, frequency, status string
		var description sql.NullString
		var occurrenceCount int
		var lastSeenAt sql.NullTime
		if frows.Scan(&id, &featureID, &title, &description, &source, &severity, &frequency, &status,
			&occurrenceCount, &lastSeenAt) != nil {
			continue
		}
		friction = append(friction, map[string]any{
			"id": id, "featureId": featureID, "title": title, "description": nullStr(description),
			"source": source, "severity": severity, "frequency": frequency, "status": status,
			"occurrenceCount": occurrenceCount, "lastSeenAt": nullTime(lastSeenAt),
		})
	}
	readbacks := []map[string]any{}
	rrows, err := s.DB.QueryContext(r.Context(), `
		SELECT r.id, r.feature_id, f.title, r.readback_due_at, r.readback_status
		  FROM shipping_releases r JOIN shipping_features f ON f.id = r.feature_id
		 WHERE f.company_id = $1 AND r.environment = 'production' AND r.status = 'succeeded'
		   AND r.readback_status IN ('pending','overdue')
		   AND (r.readback_status = 'overdue' OR r.readback_due_at <= NOW())
		 ORDER BY r.readback_due_at ASC`, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rrows.Close()
	for rrows.Next() {
		var id, featureID, featureTitle string
		var readbackDueAt sql.NullTime
		var readbackStatus sql.NullString
		if rrows.Scan(&id, &featureID, &featureTitle, &readbackDueAt, &readbackStatus) != nil {
			continue
		}
		readbacks = append(readbacks, map[string]any{
			"id": id, "featureId": featureID, "featureTitle": featureTitle,
			"readbackDueAt": nullTime(readbackDueAt), "readbackStatus": nullStr(readbackStatus),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"features": features, "friction": friction, "dueReadbacks": readbacks,
	})
}

func (s *Server) GetShippingFeature(w http.ResponseWriter, r *http.Request, id string) {
	_, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	detail, serr := detailFeature(r.Context(), s.DB, companyID, id)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

/* ───────── features 写 ───────── */

func (s *Server) CreateShippingFeature(w http.ResponseWriter, r *http.Request) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	body := readBody(r)
	title := body.text("title", 300)
	if title == "" {
		writeShipError(w, r, fail(http.StatusBadRequest, "title required"))
		return
	}
	builderIDs := body.stringArray("builderIds", 100)
	if serr := assertParticipants(r.Context(), s.DB, companyID, builderIDs); serr != nil {
		writeShipError(w, r, serr)
		return
	}
	links := featureLinks{
		projectID: body.optText("projectId", 2000), conversationID: body.optText("conversationId", 2000),
		documentID: body.optText("documentId", 2000), boardCardID: body.optText("boardCardId", 2000),
	}
	if serr := assertFeatureLinks(r.Context(), s.DB, companyID, links); serr != nil {
		writeShipError(w, r, serr)
		return
	}
	id := randID("ship")
	// #213:收编 db.WithTx——各步失败均 WriteInternalError(err) 同构映射,
	// 响应字节不变;detail 回查与 201 留在提交后。
	if err := dbpkg.WithTx(r.Context(), s.DB, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO shipping_features
			  (id, company_id, project_id, conversation_id, document_id, board_card_id,
			   title, problem, desired_outcome, contract_summary, priority, risk_level,
			   release_target, builder_ids, created_by, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb,$15,$15)`,
			id, companyID, nullStrPtr(links.projectID), nullStrPtr(links.conversationID),
			nullStrPtr(links.documentID), nullStrPtr(links.boardCardID),
			title, body.text("problem", 20000), body.text("desiredOutcome", 20000), body.text("contractSummary", 20000),
			body.enumValue("priority", priorities, "medium"), body.enumValue("riskLevel", riskLevels, "medium"),
			nullStrPtr(body.optText("releaseTarget", 2000)), jsonx.MustJSONString(builderIDs), uid); err != nil {
			return err
		}
		// 三张默认必答格(user_path/trace/release_note)——ready 门的地基。
		defaults := []struct {
			title    string
			method   string
			position int
		}{
			{"Walk the critical user path", "user_path", 10},
			{"Prove trace coverage and diagnostic evidence", "trace", 20},
			{"Verify release notes and known gaps", "release_note", 30},
		}
		for _, sq := range defaults {
			if _, err := tx.ExecContext(r.Context(), `
				INSERT INTO shipping_verifications
				  (id, feature_id, title, description, method, required, builder_ids, position, created_by)
				VALUES ($1,$2,$3,$4,$5,TRUE,$6::jsonb,$7,$8)`,
				randID("sv"), id, sq.title, "", sq.method, jsonx.MustJSONString(builderIDs), sq.position, uid); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO shipping_events (id, company_id, feature_id, actor_id, kind, data)
			VALUES ($1,$2,$3,$4,'feature.created',$5::jsonb)`,
			randID("se"), companyID, id, uid, jsonx.MustJSONString(map[string]any{"title": title})); err != nil {
			return err
		}
		return nil
	}); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	detail, serr := detailFeature(r.Context(), s.DB, companyID, id)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, detail)
}

func (s *Server) UpdateShippingFeature(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	featureID := id
	feature, serr := requireFeature(r.Context(), s.DB, companyID, featureID)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	if feature.status == "archived" {
		writeShipError(w, r, fail(http.StatusConflict, "archived features are immutable"))
		return
	}
	body := readBody(r)
	sets := []string{}
	var values []any
	add := func(col string, v any) {
		values = append(values, v)
		sets = append(sets, col+" = $"+itoa(len(values)))
	}
	if body.has("title") {
		title := body.text("title", 300)
		if title == "" {
			writeShipError(w, r, fail(http.StatusBadRequest, "title cannot be empty"))
			return
		}
		add("title", title)
	}
	if body.has("problem") {
		add("problem", body.text("problem", 20000))
	}
	if body.has("desiredOutcome") {
		add("desired_outcome", body.text("desiredOutcome", 20000))
	}
	if body.has("contractSummary") {
		add("contract_summary", body.text("contractSummary", 20000))
	}
	if body.has("priority") {
		p := body.enumValue("priority", priorities, "\x00")
		if p == "\x00" {
			writeShipError(w, r, fail(http.StatusBadRequest, "invalid priority"))
			return
		}
		add("priority", p)
	}
	if body.has("riskLevel") {
		rl := body.enumValue("riskLevel", riskLevels, "\x00")
		if rl == "\x00" {
			writeShipError(w, r, fail(http.StatusBadRequest, "invalid risk level"))
			return
		}
		add("risk_level", rl)
	}
	links := featureLinks{}
	if body.has("projectId") {
		links.projectID = body.optText("projectId", 2000)
	}
	if body.has("conversationId") {
		links.conversationID = body.optText("conversationId", 2000)
	}
	if body.has("documentId") {
		links.documentID = body.optText("documentId", 2000)
	}
	if body.has("boardCardId") {
		links.boardCardID = body.optText("boardCardId", 2000)
	}
	if serr := assertFeatureLinks(r.Context(), s.DB, companyID, links); serr != nil {
		writeShipError(w, r, serr)
		return
	}
	if body.has("releaseTarget") {
		add("release_target", nullStrPtr(body.optText("releaseTarget", 2000)))
	}
	if body.has("projectId") {
		add("project_id", nullStrPtr(links.projectID))
	}
	if body.has("conversationId") {
		add("conversation_id", nullStrPtr(links.conversationID))
	}
	if body.has("documentId") {
		add("document_id", nullStrPtr(links.documentID))
	}
	if body.has("boardCardId") {
		add("board_card_id", nullStrPtr(links.boardCardID))
	}
	var nextBuilderIDs []string
	if body.has("builderIds") {
		ids := body.stringArray("builderIds", 100)
		if serr := assertParticipants(r.Context(), s.DB, companyID, ids); serr != nil {
			writeShipError(w, r, serr)
			return
		}
		add("builder_ids", jsonx.MustJSONString(ids))
		nextBuilderIDs = ids
	}
	if len(sets) == 0 {
		detail, serr := detailFeature(r.Context(), s.DB, companyID, featureID)
		if serr != nil {
			writeShipError(w, r, serr)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, detail)
		return
	}
	values = append(values, uid, featureID, companyID)
	// #213:收编 db.WithTx——各步失败均 WriteInternalError(err) 同构映射,
	// 响应字节不变;事件补记与 detail 回查留在提交后。
	if err := dbpkg.WithTx(r.Context(), s.DB, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE shipping_features SET `+strings.Join(sets, ", ")+`, updated_by = $`+itoa(len(values)-2)+`, updated_at = NOW()
			  WHERE id = $`+itoa(len(values)-1)+` AND company_id = $`+itoa(len(values)), values...); err != nil {
			return err
		}
		if nextBuilderIDs != nil {
			if _, err := tx.ExecContext(r.Context(),
				`UPDATE shipping_verifications SET builder_ids = $1::jsonb, updated_at = NOW() WHERE feature_id = $2`,
				jsonx.MustJSONString(nextBuilderIDs), featureID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_ = recordEvent(r.Context(), s.DB, companyID, featureID, uid, "feature.updated", body)
	detail, serr := detailFeature(r.Context(), s.DB, companyID, featureID)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

func (s *Server) TransitionShippingFeature(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	feature, serr := requireFeature(r.Context(), s.DB, companyID, id)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	body := readBody(r)
	to := body.enumValue("status", featureStatuses, "")
	if to == "" {
		writeShipError(w, r, fail(http.StatusBadRequest, "valid status required"))
		return
	}
	if serr := assertTransitionReady(r.Context(), s.DB, feature.id, feature.status, to); serr != nil {
		writeShipError(w, r, serr)
		return
	}
	if _, err := s.DB.ExecContext(r.Context(), `
		UPDATE shipping_features SET status = $1, updated_by = $2, updated_at = NOW(),
		       archived_at = CASE WHEN $1 = 'archived' THEN NOW() ELSE archived_at END
		  WHERE id = $3 AND company_id = $4`, to, uid, feature.id, companyID); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_ = recordEvent(r.Context(), s.DB, companyID, feature.id, uid, "feature.transitioned",
		map[string]any{"from": feature.status, "to": to})
	detail, serr := detailFeature(r.Context(), s.DB, companyID, feature.id)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

/* ───────── invariants ───────── */

func (s *Server) CreateShippingInvariant(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	featureID := id
	if _, serr := requireFeature(r.Context(), s.DB, companyID, featureID); serr != nil {
		writeShipError(w, r, serr)
		return
	}
	body := readBody(r)
	title := body.text("title", 300)
	if title == "" {
		writeShipError(w, r, fail(http.StatusBadRequest, "title required"))
		return
	}
	invID := randID("si")
	var position int
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT COALESCE(max(position), 0) + 10 FROM shipping_invariants WHERE feature_id = $1`, featureID).
		Scan(&position)
	if position == 0 {
		position = 10
	}
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO shipping_invariants
		  (id, feature_id, title, description, kind, required, position, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		invID, featureID, title, body.text("description", 20000),
		body.enumValue("kind", invariantKinds, "behavior"), body.boolean("required", true), position, uid); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_ = recordEvent(r.Context(), s.DB, companyID, featureID, uid, "invariant.created",
		map[string]any{"id": invID, "title": title})
	detail, serr := detailFeature(r.Context(), s.DB, companyID, featureID)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, detail)
}

func (s *Server) UpdateShippingInvariant(w http.ResponseWriter, r *http.Request, id string, invariantId string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	featureID := id
	if _, serr := requireFeature(r.Context(), s.DB, companyID, featureID); serr != nil {
		writeShipError(w, r, serr)
		return
	}
	body := readBody(r)
	// TS COALESCE 形:未传字段(null)保旧值。
	var title, description, kind, required any
	if body.has("title") {
		title = body.text("title", 300)
	}
	if body.has("description") {
		description = body.text("description", 20000)
	}
	if body.has("kind") {
		kind = body.enumValue("kind", invariantKinds, "behavior")
	}
	if body.has("required") {
		required = body.boolean("required", true)
	}
	var position any
	if raw, ok := body["position"]; ok {
		var f float64
		if json.Unmarshal(raw, &f) == nil {
			position = int(f)
		}
	}
	res, err := s.DB.ExecContext(r.Context(), `
		UPDATE shipping_invariants SET
		   title = COALESCE($1, title), description = COALESCE($2, description),
		   kind = COALESCE($3, kind), required = COALESCE($4, required),
		   position = COALESCE($5, position), updated_at = NOW()
		 WHERE id = $6 AND feature_id = $7`,
		title, description, kind, required, position, invariantId, featureID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeShipError(w, r, fail(http.StatusNotFound, "invariant not found"))
		return
	}
	_ = recordEvent(r.Context(), s.DB, companyID, featureID, uid, "invariant.updated",
		map[string]any{"id": invariantId})
	detail, serr := detailFeature(r.Context(), s.DB, companyID, featureID)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

/* ───────── verifications ───────── */

func (s *Server) CreateShippingVerification(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	featureID := id
	feature, serr := requireFeature(r.Context(), s.DB, companyID, featureID)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	body := readBody(r)
	title := body.text("title", 300)
	if title == "" {
		writeShipError(w, r, fail(http.StatusBadRequest, "title required"))
		return
	}
	ownerID := body.optText("ownerId", 2000)
	if ownerID.Valid {
		if serr := assertParticipants(r.Context(), s.DB, companyID, []string{ownerID.String}); serr != nil {
			writeShipError(w, r, serr)
			return
		}
	}
	builderIDs := feature.builderIDs
	if body.has("builderIds") {
		builderIDs = body.stringArray("builderIds", 100)
	}
	if serr := assertParticipants(r.Context(), s.DB, companyID, builderIDs); serr != nil {
		writeShipError(w, r, serr)
		return
	}
	if ownerID.Valid && contains(builderIDs, ownerID.String) {
		writeShipError(w, r, fail(http.StatusConflict, "verification owner cannot be one of the builders"))
		return
	}
	invariantID := body.optText("invariantId", 2000)
	if invariantID.Valid {
		var one int
		if s.DB.QueryRowContext(r.Context(),
			`SELECT 1 FROM shipping_invariants WHERE id = $1 AND feature_id = $2`,
			invariantID.String, feature.id).Scan(&one) != nil {
			writeShipError(w, r, fail(http.StatusBadRequest, "invariant does not belong to this feature"))
			return
		}
	}
	var position int
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT COALESCE(max(position), 0) + 10 FROM shipping_verifications WHERE feature_id = $1`, feature.id).
		Scan(&position)
	if position == 0 {
		position = 10
	}
	dueAt, serr2 := body.isoOrNull("dueAt")
	if serr2 != nil {
		writeShipError(w, r, serr2)
		return
	}
	verID := randID("sv")
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO shipping_verifications
		  (id, feature_id, invariant_id, title, description, method, required, owner_id,
		   builder_ids, position, due_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12)`,
		verID, feature.id, nullStrPtr(invariantID), title, body.text("description", 20000),
		body.enumValue("method", verificationMethods, "user_path"), body.boolean("required", true),
		nullStrPtr(ownerID), jsonx.MustJSONString(builderIDs), position, nullTimePtr(dueAt), uid); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_ = recordEvent(r.Context(), s.DB, companyID, feature.id, uid, "verification.created",
		map[string]any{"id": verID, "title": title, "ownerId": nullStr(ownerID)})
	detail, serr := detailFeature(r.Context(), s.DB, companyID, feature.id)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, detail)
}

func (s *Server) UpdateShippingVerification(w http.ResponseWriter, r *http.Request, id string, verificationId string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	featureID := id
	feature, serr := requireFeature(r.Context(), s.DB, companyID, featureID)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	verificationID := verificationId
	var currentBuilderIDs []byte
	var currentStatus, currentTitle string
	var currentOwnerID sql.NullString
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT builder_ids, status, owner_id, title FROM shipping_verifications WHERE id = $1 AND feature_id = $2`,
		verificationID, feature.id).Scan(&currentBuilderIDs, &currentStatus, &currentOwnerID, &currentTitle)
	if err != nil {
		writeShipError(w, r, fail(http.StatusNotFound, "verification square not found"))
		return
	}
	body := readBody(r)
	ownerID := currentOwnerID
	if body.has("ownerId") {
		ownerID = body.optText("ownerId", 2000)
	}
	builderIDs := scanJSONStrings(currentBuilderIDs)
	if body.has("builderIds") {
		builderIDs = body.stringArray("builderIds", 100)
	}
	if ownerID.Valid {
		if serr := assertParticipants(r.Context(), s.DB, companyID, []string{ownerID.String}); serr != nil {
			writeShipError(w, r, serr)
			return
		}
	}
	if serr := assertParticipants(r.Context(), s.DB, companyID, builderIDs); serr != nil {
		writeShipError(w, r, serr)
		return
	}
	if ownerID.Valid && contains(builderIDs, ownerID.String) {
		writeShipError(w, r, fail(http.StatusConflict, "verification owner cannot be one of the builders"))
		return
	}
	nextStatus := currentStatus
	if body.has("status") {
		nextStatus = body.enumValue("status", verificationStatuses, "")
	}
	if nextStatus == "" {
		writeShipError(w, r, fail(http.StatusBadRequest, "invalid verification status"))
		return
	}
	completing := nextStatus == "passed" || nextStatus == "failed" || nextStatus == "waived"
	if completing && contains(builderIDs, uid) {
		writeShipError(w, r, fail(http.StatusConflict, "the builder cannot verify their own work"))
		return
	}
	var evidence []any
	hasEvidence := false
	if body.has("evidence") {
		evidence = body.jsonArray("evidence", 100)
		hasEvidence = true
	}
	if (nextStatus == "passed" || nextStatus == "failed") && (!hasEvidence || len(evidence) == 0) {
		writeShipError(w, r, fail(http.StatusConflict, "passed/failed verification requires evidence"))
		return
	}
	if nextStatus == "waived" && body.text("notes", 20000) == "" {
		writeShipError(w, r, fail(http.StatusConflict, "waiving a square requires a written reason"))
		return
	}
	var title, description, method, required, notes, dueAt, evidenceSQL any
	if body.has("title") {
		title = body.text("title", 300)
	}
	if body.has("description") {
		description = body.text("description", 20000)
	}
	if body.has("method") {
		method = body.enumValue("method", verificationMethods, "user_path")
	}
	if body.has("required") {
		required = body.boolean("required", true)
	}
	if hasEvidence {
		evidenceSQL = jsonx.MustJSONString(evidence)
	}
	if body.has("notes") {
		notes = body.text("notes", 20000)
	}
	if body.has("dueAt") {
		due, serr2 := body.isoOrNull("dueAt")
		if serr2 != nil {
			writeShipError(w, r, serr2)
			return
		}
		dueAt = nullTimePtr(due)
	}
	var verifiedBy any
	if completing {
		verifiedBy = uid
	}
	if _, err := s.DB.ExecContext(r.Context(), `
		UPDATE shipping_verifications SET
		   title = COALESCE($1, title), description = COALESCE($2, description),
		   method = COALESCE($3, method), required = COALESCE($4, required),
		   owner_id = $5, builder_ids = $6::jsonb, status = $7,
		   evidence = COALESCE($8::jsonb, evidence), notes = COALESCE($9, notes),
		   due_at = COALESCE($10, due_at), verified_by_id = CASE WHEN $11 THEN $12 ELSE verified_by_id END,
		   completed_at = CASE WHEN $11 THEN NOW() ELSE NULL END, updated_at = NOW()
		 WHERE id = $13 AND feature_id = $14`,
		title, description, method, required, nullStrPtr(ownerID), jsonx.MustJSONString(builderIDs), nextStatus,
		evidenceSQL, notes, dueAt, completing, verifiedBy, verificationID, feature.id); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if nextStatus == "failed" {
		// 失格自动升格:可重放回归资产 + 摩擦收件箱。
		titleForAssets := body.text("title", 300)
		if titleForAssets == "" {
			titleForAssets = currentTitle
		}
		if _, err := s.DB.ExecContext(r.Context(), `
			INSERT INTO shipping_regressions
			  (id, feature_id, source_verification_id, title, kind, expected, status, created_by)
			VALUES ($1,$2,$3,$4,'manual_replay',$5,'failing',$6)
			ON CONFLICT (source_verification_id) WHERE source_verification_id IS NOT NULL
			DO UPDATE SET status='failing', updated_at=NOW()`,
			randID("rg"), feature.id, verificationID,
			"Replay failed square: "+titleForAssets,
			"The behavior proven by this square remains true", uid); err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		if _, err := s.DB.ExecContext(r.Context(), `
			INSERT INTO shipping_friction_reports
			  (id, company_id, feature_id, reporter_id, source, source_key, title,
			   description, severity, frequency, status, evidence)
			VALUES ($1,$2,$3,$4,'verification',$5,$6,$7,'high','once','open',$8::jsonb)
			ON CONFLICT (company_id, source_key) WHERE source_key IS NOT NULL
			DO UPDATE SET occurrence_count=shipping_friction_reports.occurrence_count+1,
			              last_seen_at=NOW(), updated_at=NOW(), status='open', evidence=EXCLUDED.evidence`,
			randID("fr"), companyID, feature.id, uid,
			"verification:"+verificationID, "Verification failed: "+titleForAssets,
			"A required proof failed and has been promoted into the friction inbox plus a replayable regression asset.",
			jsonx.MustJSONString(orEmpty(evidence))); err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
	}
	_ = recordEvent(r.Context(), s.DB, companyID, feature.id, uid, "verification.updated",
		map[string]any{"id": verificationID, "status": nextStatus})
	detail, serr := detailFeature(r.Context(), s.DB, companyID, feature.id)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

func orEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func nullTimePtr(nt sql.NullTime) any {
	if !nt.Valid {
		return nil
	}
	return nt.Time
}

/* ───────── releases ───────── */

func (s *Server) CreateShippingRelease(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	feature, serr := requireFeature(r.Context(), s.DB, companyID, id)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	body := readBody(r)
	environment := body.enumValue("environment", releaseEnvironments, "")
	if environment == "" {
		writeShipError(w, r, fail(http.StatusBadRequest, "valid environment required"))
		return
	}
	if environment == "staging" || environment == "canary" || environment == "production" {
		if feature.status != "ready" && feature.status != "releasing" && feature.status != "watching" {
			writeShipError(w, r, fail(http.StatusConflict, "%s release requires a ready feature", environment))
			return
		}
	}
	readbackDue, serr2 := body.isoOrNull("readbackDueAt")
	if serr2 != nil {
		writeShipError(w, r, serr2)
		return
	}
	relID := randID("sr")
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO shipping_releases
		  (id, feature_id, environment, version, commit_sha, release_notes, rollback_plan,
		   known_gaps, baseline, readback_due_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10)`,
		relID, feature.id, environment, nullStrPtr(body.optText("version", 200)), nullStrPtr(body.optText("commitSha", 200)),
		body.text("releaseNotes", 20000), body.text("rollbackPlan", 20000),
		jsonx.MustJSONString(body.jsonArray("knownGaps", 100)),
		jsonx.MustJSONString(body.jsonArray("baseline", 100)), nullTimePtr(readbackDue)); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_ = recordEvent(r.Context(), s.DB, companyID, feature.id, uid, "release.planned",
		map[string]any{"id": relID, "environment": environment})
	detail, serr := detailFeature(r.Context(), s.DB, companyID, feature.id)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, detail)
}

// releaseAction:approve/start/succeed/fail/readback_*/rollback 状态机。
// approve/rollback 走 owner/admin 特权门(TS requireCompanyRole)。
func (s *Server) ShippingReleaseAction(w http.ResponseWriter, r *http.Request, id string, releaseId string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	featureID := id
	feature, serr := requireFeature(r.Context(), s.DB, companyID, featureID)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	releaseID := releaseId
	body := readBody(r)
	action := body.text("action", 40)
	allowed := map[string]bool{"approve": true, "start": true, "succeed": true, "fail": true,
		"readback_pass": true, "readback_fail": true, "rollback": true}
	if !allowed[action] {
		writeShipError(w, r, fail(http.StatusBadRequest, "invalid release action"))
		return
	}
	actor := uid
	if action == "approve" || action == "rollback" {
		if _, ok2 := httpx.ResolveCompanyRole(w, r, s.DB, uid); !ok2 {
			return // 403 已写响应
		}
	}
	// #235 收编 db.WithTx(#213 豁免的"双 500 通道文案各异"半理由已被
	// #214 抹平:内联 Exec 错误均走 WriteInternalError(err);releaseApprove
	// 尾部的 fail(500) 仍是 *shippingError 域错,经 fn 原样回传)。
	// tx 传参不是障碍:闭包拿到的 *sql.Tx 原样下传 releaseApprove/
	// recordEvent;错误在外层经 writeShipError 二分(*shippingError →
	// WriteError 保留 4xx/域错响应体,其余 → WriteInternalError),
	// 各路径响应字节与手写版一致。
	var environment, status string
	var releaseNotes, rollbackPlan sql.NullString
	var baseline []byte
	err := dbpkg.WithTx(r.Context(), s.DB, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(r.Context(),
			`SELECT environment, status, release_notes, rollback_plan, baseline
			   FROM shipping_releases WHERE id = $1 AND feature_id = $2 FOR UPDATE`,
			releaseID, feature.id).Scan(&environment, &status, &releaseNotes, &rollbackPlan, &baseline); err != nil {
			return fail(http.StatusNotFound, "release not found")
		}
		var actionErr *shippingError
		switch action {
		case "approve":
			actionErr = releaseApprove(r, tx, feature, releaseID, environment, status, releaseNotes, rollbackPlan, baseline, actor)
		case "start":
			if environment == "production" {
				if status != "approved" {
					actionErr = fail(http.StatusConflict, "release is not approved to start")
				}
			} else if status != "planned" && status != "approved" {
				actionErr = fail(http.StatusConflict, "release is not approved to start")
			}
			if actionErr == nil {
				if _, err := tx.ExecContext(r.Context(),
					`UPDATE shipping_releases SET status='running', started_by=$1, started_at=NOW(), updated_at=NOW() WHERE id=$2`,
					actor, releaseID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(r.Context(),
					`UPDATE shipping_features SET status='releasing', updated_by=$1, updated_at=NOW() WHERE id=$2`,
					actor, feature.id); err != nil {
					return err
				}
			}
		case "succeed":
			evidence := body.jsonArray("evidence", 100)
			if status != "running" {
				actionErr = fail(http.StatusConflict, "only a running release can succeed")
			} else if len(evidence) == 0 {
				actionErr = fail(http.StatusConflict, "a successful release requires smoke evidence")
			}
			if actionErr == nil {
				var dueAt any
				if environment == "production" {
					dueAt = time.Now().Add(24 * time.Hour)
				}
				if _, err := tx.ExecContext(r.Context(), `
					UPDATE shipping_releases SET status='succeeded', smoke_evidence=$1::jsonb,
					       completed_at=NOW(), readback_due_at=COALESCE(readback_due_at,$2), updated_at=NOW() WHERE id=$3`,
					jsonx.MustJSONString(evidence), dueAt, releaseID); err != nil {
					return err
				}
				if environment == "production" {
					if _, err := tx.ExecContext(r.Context(),
						`UPDATE shipping_features SET status='watching', updated_by=$1, updated_at=NOW() WHERE id=$2`,
						actor, feature.id); err != nil {
						return err
					}
				}
			}
		case "fail":
			evidence := body.jsonArray("evidence", 100)
			if status != "running" {
				actionErr = fail(http.StatusConflict, "only a running release can fail")
			} else if len(evidence) == 0 {
				actionErr = fail(http.StatusConflict, "a failed release requires diagnostic evidence")
			}
			if actionErr == nil {
				if _, err := tx.ExecContext(r.Context(),
					`UPDATE shipping_releases SET status='failed', smoke_evidence=$1::jsonb, completed_at=NOW(), updated_at=NOW() WHERE id=$2`,
					jsonx.MustJSONString(evidence), releaseID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(r.Context(),
					`UPDATE shipping_features SET status='ready', updated_by=$1, updated_at=NOW() WHERE id=$2`,
					actor, feature.id); err != nil {
					return err
				}
			}
		case "readback_pass", "readback_fail":
			evidence := body.jsonArray("evidence", 100)
			if environment != "production" || status != "succeeded" {
				actionErr = fail(http.StatusConflict, "readback applies only to successful production releases")
			} else if len(evidence) == 0 {
				actionErr = fail(http.StatusConflict, "readback requires production evidence")
			}
			if actionErr == nil {
				rbStatus := "passed"
				if action == "readback_fail" {
					rbStatus = "failed"
				}
				if _, err := tx.ExecContext(r.Context(), `
					UPDATE shipping_releases SET readback_status=$1, readback_evidence=$2::jsonb, updated_at=NOW() WHERE id=$3`,
					rbStatus, jsonx.MustJSONString(evidence), releaseID); err != nil {
					return err
				}
				if action == "readback_fail" {
					if _, err := tx.ExecContext(r.Context(), `
						INSERT INTO shipping_friction_reports
						  (id,company_id,feature_id,reporter_id,source,source_key,title,description,severity,frequency,status,evidence)
						VALUES ($1,$2,$3,$4,'production-readback',$5,$6,$7,'critical','frequent','open',$8::jsonb)
						ON CONFLICT (company_id, source_key) WHERE source_key IS NOT NULL
						DO UPDATE SET occurrence_count=shipping_friction_reports.occurrence_count+1,
						              last_seen_at=NOW(),updated_at=NOW(),status='open',evidence=EXCLUDED.evidence`,
						randID("fr"), companyID, feature.id, actor, "readback:"+releaseID,
						"Production readback failed: "+feature.title,
						"Observed production behavior diverged from the release baseline; investigate and add a replayable regression.",
						jsonx.MustJSONString(evidence)); err != nil {
						return err
					}
					if _, err := tx.ExecContext(r.Context(),
						`UPDATE shipping_features SET status='building', updated_by=$1, updated_at=NOW() WHERE id=$2`,
						actor, feature.id); err != nil {
						return err
					}
				}
			}
		case "rollback":
			if status != "running" && status != "succeeded" && status != "failed" {
				actionErr = fail(http.StatusConflict, "only an active or completed release can be rolled back")
			}
			reason := body.text("reason", 4000)
			if actionErr == nil && reason == "" {
				actionErr = fail(http.StatusConflict, "rollback requires a reason")
			}
			if actionErr == nil {
				if _, err := tx.ExecContext(r.Context(), `
					UPDATE shipping_releases SET status='rolled_back', rolled_back_at=NOW(), rollback_reason=$1, updated_at=NOW() WHERE id=$2`,
					reason, releaseID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(r.Context(),
					`UPDATE shipping_features SET status='building', updated_by=$1, updated_at=NOW() WHERE id=$2`,
					actor, feature.id); err != nil {
					return err
				}
			}
		}
		if actionErr != nil {
			return actionErr
		}
		// 事件失败不阻断(手写版 `_ =` 语义保留:不回滚、不影响提交)。
		_ = recordEvent(r.Context(), tx, companyID, feature.id, actor, "release."+action,
			map[string]any{"releaseId": releaseID, "evidence": body.jsonArray("evidence", 100)})
		return nil
	})
	if err != nil {
		// 二分:*shippingError(4xx 域错与 releaseApprove 尾部 fail(500))
		// → WriteError 原响应体;其余(BeginTx/内联 Exec/Commit)→
		// WriteInternalError,与手写版双通道逐路径一致。
		writeShipError(w, r, err)
		return
	}
	detail, serr := detailFeature(r.Context(), s.DB, companyID, feature.id)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

func releaseApprove(r *http.Request, tx *sql.Tx, feature *shipFeatureCore, releaseID, environment, status string,
	releaseNotes, rollbackPlan sql.NullString, baseline []byte, actor string) *shippingError {
	if status != "planned" {
		return fail(http.StatusConflict, "only planned releases can be approved")
	}
	if environment == "production" {
		if stringsBlank(releaseNotes.String) {
			return fail(http.StatusConflict, "production approval requires release notes")
		}
		if stringsBlank(rollbackPlan.String) {
			return fail(http.StatusConflict, "production approval requires a rollback plan")
		}
		if len(baseline) == 0 || string(baseline) == "[]" || string(baseline) == "null" {
			return fail(http.StatusConflict, "production approval requires a measurable baseline")
		}
		var staged int
		_ = tx.QueryRowContext(r.Context(), `
			SELECT count(*)::int FROM shipping_releases
			  WHERE feature_id = $1 AND environment IN ('staging','canary') AND status = 'succeeded'`, feature.id).Scan(&staged)
		if staged == 0 {
			return fail(http.StatusConflict, "production approval requires a successful staging or canary release")
		}
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE shipping_releases SET status='approved', approved_by=$1, updated_at=NOW() WHERE id=$2`, actor, releaseID); err != nil {
		return fail(http.StatusInternalServerError, "%s", err.Error())
	}
	return nil
}

func stringsBlank(s string) bool { return len(s) == 0 || strings.TrimSpace(s) == "" }

/* ───────── friction ───────── */

func (s *Server) ListShippingFriction(w http.ResponseWriter, r *http.Request) {
	_, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, feature_id, conversation_id, message_id, reporter_id, source, title,
		       description, severity, frequency, status, evidence,
		       occurrence_count, first_seen_at, last_seen_at, created_at, updated_at
		  FROM shipping_friction_reports WHERE company_id = $1
		 ORDER BY CASE status WHEN 'open' THEN 0 WHEN 'triaged' THEN 1 WHEN 'planned' THEN 2 ELSE 3 END,
		          CASE severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
		          last_seen_at DESC`, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, source, title, severity, frequency, status string
		var featureID, conversationID, messageID, reporterID, description sql.NullString
		var evidence []byte
		var occurrenceCount int
		var firstSeenAt, lastSeenAt, createdAt, updatedAt sql.NullTime
		if rows.Scan(&id, &featureID, &conversationID, &messageID, &reporterID, &source, &title,
			&description, &severity, &frequency, &status, &evidence,
			&occurrenceCount, &firstSeenAt, &lastSeenAt, &createdAt, &updatedAt) != nil {
			continue
		}
		var evidenceJSON sql.NullString
		if len(evidence) > 0 {
			evidenceJSON = sql.NullString{String: string(evidence), Valid: true}
		}
		out = append(out, map[string]any{
			"id": id, "featureId": nullStr(featureID), "conversationId": nullStr(conversationID),
			"messageId": nullStr(messageID), "reporterId": nullStr(reporterID), "source": source,
			"title": title, "description": nullStr(description), "severity": severity,
			"frequency": frequency, "status": status, "evidence": scanJSONAny(evidenceJSON),
			"occurrenceCount": occurrenceCount, "firstSeenAt": nullTime(firstSeenAt),
			"lastSeenAt": nullTime(lastSeenAt), "createdAt": nullTime(createdAt), "updatedAt": nullTime(updatedAt),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) CreateShippingFriction(w http.ResponseWriter, r *http.Request) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	body := readBody(r)
	title := body.text("title", 300)
	if title == "" {
		writeShipError(w, r, fail(http.StatusBadRequest, "title required"))
		return
	}
	id := randID("fr")
	featureID := body.optText("featureId", 2000)
	if featureID.Valid {
		if _, serr := requireFeature(r.Context(), s.DB, companyID, featureID.String); serr != nil {
			writeShipError(w, r, serr)
			return
		}
	}
	conversationID := body.optText("conversationId", 2000)
	messageID := body.optText("messageId", 2000)
	if serr := assertConversationMessageLinks(r.Context(), s.DB, companyID, conversationID, messageID); serr != nil {
		writeShipError(w, r, serr)
		return
	}
	source := body.optText("source", 100)
	if !source.Valid {
		source = sql.NullString{String: "manual", Valid: true}
	}
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO shipping_friction_reports
		  (id, company_id, feature_id, conversation_id, message_id, reporter_id, source,
		   title, description, severity, frequency, status, evidence)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::jsonb)`,
		id, companyID, nullStrPtr(featureID), nullStrPtr(conversationID), nullStrPtr(messageID),
		uid, source.String, title, body.text("description", 20000),
		body.enumValue("severity", frictionSeverities, "medium"),
		body.enumValue("frequency", frictionFrequencies, "once"),
		body.enumValue("status", frictionStatuses, "open"),
		jsonx.MustJSONString(body.jsonArray("evidence", 100))); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_ = recordEvent(r.Context(), s.DB, companyID, nullStrPtr2Str(featureID), uid, "friction.created",
		map[string]any{"frictionId": id, "title": title})
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func nullStrPtr2Str(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

func (s *Server) UpdateShippingFriction(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	body := readBody(r)
	var featureID sql.NullString
	hasFeature := body.has("featureId")
	if hasFeature {
		featureID = body.optText("featureId", 2000)
	}
	if featureID.Valid {
		if _, serr := requireFeature(r.Context(), s.DB, companyID, featureID.String); serr != nil {
			writeShipError(w, r, serr)
			return
		}
	}
	var title, description, severity, frequency, status, evidenceSQL any
	if body.has("title") {
		title = body.text("title", 300)
	}
	if body.has("description") {
		description = body.text("description", 20000)
	}
	if body.has("severity") {
		severity = body.enumValue("severity", frictionSeverities, "medium")
	}
	if body.has("frequency") {
		frequency = body.enumValue("frequency", frictionFrequencies, "once")
	}
	if body.has("status") {
		status = body.enumValue("status", frictionStatuses, "open")
	}
	if body.has("evidence") {
		evidenceSQL = jsonx.MustJSONString(body.jsonArray("evidence", 100))
	}
	increment := 0
	if body.has("incrementOccurrence") && body.boolean("incrementOccurrence", false) {
		increment = 1
	}
	res, err := s.DB.ExecContext(r.Context(), `
		UPDATE shipping_friction_reports SET
		   feature_id = CASE WHEN $1 THEN $2 ELSE feature_id END,
		   title = COALESCE($3, title), description = COALESCE($4, description),
		   severity = COALESCE($5, severity), frequency = COALESCE($6, frequency),
		   status = COALESCE($7, status), evidence = COALESCE($8::jsonb, evidence),
		   occurrence_count = occurrence_count + COALESCE($9, 0),
		   last_seen_at = CASE WHEN COALESCE($9,0) > 0 THEN NOW() ELSE last_seen_at END,
		   updated_at = NOW()
		 WHERE id = $10 AND company_id = $11`,
		hasFeature, nullStrPtr(featureID), title, description, severity, frequency, status,
		evidenceSQL, increment, id, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeShipError(w, r, fail(http.StatusNotFound, "friction report not found"))
		return
	}
	_ = recordEvent(r.Context(), s.DB, companyID, nullStrPtr2Str(featureID), uid, "friction.updated",
		map[string]any{"frictionId": id})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ───────── regressions ───────── */

func (s *Server) CreateShippingRegression(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	featureID := id
	if _, serr := requireFeature(r.Context(), s.DB, companyID, featureID); serr != nil {
		writeShipError(w, r, serr)
		return
	}
	body := readBody(r)
	title := body.text("title", 300)
	if title == "" {
		writeShipError(w, r, fail(http.StatusBadRequest, "title required"))
		return
	}
	invariantID := body.optText("invariantId", 2000)
	if invariantID.Valid {
		var one int
		if s.DB.QueryRowContext(r.Context(),
			`SELECT 1 FROM shipping_invariants WHERE id = $1 AND feature_id = $2`,
			invariantID.String, featureID).Scan(&one) != nil {
			writeShipError(w, r, fail(http.StatusBadRequest, "invariant does not belong to this feature"))
			return
		}
	}
	sourceVerificationID := body.optText("sourceVerificationId", 2000)
	if sourceVerificationID.Valid {
		var one int
		if s.DB.QueryRowContext(r.Context(),
			`SELECT 1 FROM shipping_verifications WHERE id = $1 AND feature_id = $2`,
			sourceVerificationID.String, featureID).Scan(&one) != nil {
			writeShipError(w, r, fail(http.StatusBadRequest, "source verification does not belong to this feature"))
			return
		}
	}
	regID := randID("rg")
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO shipping_regressions
		  (id, feature_id, invariant_id, source_verification_id, title, kind, command,
		   expected, status, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		regID, featureID, nullStrPtr(invariantID), nullStrPtr(sourceVerificationID),
		title, body.enumValue("kind", regressionKinds, "automated"),
		nullStrPtr(body.optText("command", 8000)), body.text("expected", 20000),
		body.enumValue("status", regressionStatuses, "active"), uid); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_ = recordEvent(r.Context(), s.DB, companyID, featureID, uid, "regression.created",
		map[string]any{"id": regID, "title": title})
	detail, serr := detailFeature(r.Context(), s.DB, companyID, featureID)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, detail)
}

func (s *Server) UpdateShippingRegression(w http.ResponseWriter, r *http.Request, id string, regressionId string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	featureID := id
	if _, serr := requireFeature(r.Context(), s.DB, companyID, featureID); serr != nil {
		writeShipError(w, r, serr)
		return
	}
	body := readBody(r)
	var title, kind, command, expected, status, lastResult, lastEvidenceSQL any
	if body.has("title") {
		title = body.text("title", 300)
	}
	if body.has("kind") {
		kind = body.enumValue("kind", regressionKinds, "automated")
	}
	if body.has("command") {
		command = nullStrPtr(body.optText("command", 8000))
	}
	if body.has("expected") {
		expected = body.text("expected", 20000)
	}
	if body.has("status") {
		status = body.enumValue("status", regressionStatuses, "active")
	}
	if body.has("lastResult") {
		lastResult = body.text("lastResult", 20000)
	}
	hasEvidence := body.has("lastEvidence")
	if hasEvidence {
		lastEvidenceSQL = jsonx.MustJSONString(body.jsonArray("lastEvidence", 100))
	}
	res, err := s.DB.ExecContext(r.Context(), `
		UPDATE shipping_regressions SET
		   title=COALESCE($1,title), kind=COALESCE($2,kind), command=COALESCE($3,command),
		   expected=COALESCE($4,expected), status=COALESCE($5,status),
		   last_result=COALESCE($6,last_result), last_evidence=COALESCE($7::jsonb,last_evidence),
		   last_run_at=CASE WHEN $6 IS NOT NULL OR $7::jsonb IS NOT NULL THEN NOW() ELSE last_run_at END,
		   updated_at=NOW() WHERE id=$8 AND feature_id=$9`,
		title, kind, command, expected, status, lastResult, lastEvidenceSQL,
		regressionId, featureID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeShipError(w, r, fail(http.StatusNotFound, "regression not found"))
		return
	}
	_ = recordEvent(r.Context(), s.DB, companyID, featureID, uid, "regression.updated",
		map[string]any{"id": regressionId, "status": status})
	detail, serr := detailFeature(r.Context(), s.DB, companyID, featureID)
	if serr != nil {
		writeShipError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}
