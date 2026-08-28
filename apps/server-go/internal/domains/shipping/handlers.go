// shipping/handlers —— #125(#117-f):16 路由 HTTP 面。逐段对齐
// api/shipping-router.ts 307–880。
package shipping

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func Mount(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("GET /api/shipping/overview", overview(db))
	mux.HandleFunc("GET /api/shipping/features/{id}", featureGet(db))
	mux.HandleFunc("POST /api/shipping/features", featureCreate(db))
	mux.HandleFunc("PATCH /api/shipping/features/{id}", featurePatch(db))
	mux.HandleFunc("POST /api/shipping/features/{id}/transition", featureTransition(db))
	mux.HandleFunc("POST /api/shipping/features/{id}/invariants", invariantCreate(db))
	mux.HandleFunc("PATCH /api/shipping/features/{id}/invariants/{iid}", invariantPatch(db))
	mux.HandleFunc("POST /api/shipping/features/{id}/verifications", verificationCreate(db))
	mux.HandleFunc("PATCH /api/shipping/features/{id}/verifications/{vid}", verificationPatch(db))
	mux.HandleFunc("POST /api/shipping/features/{id}/releases", releaseCreate(db))
	mux.HandleFunc("POST /api/shipping/features/{id}/releases/{rid}/action", releaseAction(db))
	mux.HandleFunc("GET /api/shipping/friction", frictionList(db))
	mux.HandleFunc("POST /api/shipping/friction", frictionCreate(db))
	mux.HandleFunc("PATCH /api/shipping/friction/{id}", frictionPatch(db))
	mux.HandleFunc("POST /api/shipping/features/{id}/regressions", regressionCreate(db))
	mux.HandleFunc("PATCH /api/shipping/features/{id}/regressions/{gid}", regressionPatch(db))
}

func requireCompany(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, string, bool) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return "", "", false
	}
	companyID, ok := httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return "", "", false
	}
	return uid, companyID, true
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

func overview(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, companyID, ok := requireCompany(w, r, db)
		if !ok {
			return
		}
		features := []map[string]any{}
		rows, err := db.QueryContext(r.Context(), `
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
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
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
		frows, err := db.QueryContext(r.Context(), `
			SELECT id, feature_id, title, description, source, severity, frequency, status,
			       occurrence_count, last_seen_at
			  FROM shipping_friction_reports
			 WHERE company_id = $1 AND status IN ('open','triaged','planned')
			 ORDER BY CASE severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
			          last_seen_at DESC`, companyID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
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
		rrows, err := db.QueryContext(r.Context(), `
			SELECT r.id, r.feature_id, f.title, r.readback_due_at, r.readback_status
			  FROM shipping_releases r JOIN shipping_features f ON f.id = r.feature_id
			 WHERE f.company_id = $1 AND r.environment = 'production' AND r.status = 'succeeded'
			   AND r.readback_status IN ('pending','overdue')
			   AND (r.readback_status = 'overdue' OR r.readback_due_at <= NOW())
			 ORDER BY r.readback_due_at ASC`, companyID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
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
}

func featureGet(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, companyID, ok := requireCompany(w, r, db)
		if !ok {
			return
		}
		detail, serr := detailFeature(r.Context(), db, companyID, r.PathValue("id"))
		if serr != nil {
			writeShipError(w, serr)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, detail)
	}
}

/* ───────── features 写 ───────── */

func featureCreate(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireCompany(w, r, db)
		if !ok {
			return
		}
		body := readBody(r)
		title := body.text("title", 300)
		if title == "" {
			writeShipError(w, fail(http.StatusBadRequest, "title required"))
			return
		}
		builderIDs := body.stringArray("builderIds", 100)
		if serr := assertParticipants(r.Context(), db, companyID, builderIDs); serr != nil {
			writeShipError(w, serr)
			return
		}
		links := featureLinks{
			projectID: body.optText("projectId", 2000), conversationID: body.optText("conversationId", 2000),
			documentID: body.optText("documentId", 2000), boardCardID: body.optText("boardCardId", 2000),
		}
		if serr := assertFeatureLinks(r.Context(), db, companyID, links); serr != nil {
			writeShipError(w, serr)
			return
		}
		id := randID("ship")
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer tx.Rollback()
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
			nullStrPtr(body.optText("releaseTarget", 2000)), mustJSONString(builderIDs), uid); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
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
				randID("sv"), id, sq.title, "", sq.method, mustJSONString(builderIDs), sq.position, uid); err != nil {
				httpx.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO shipping_events (id, company_id, feature_id, actor_id, kind, data)
			VALUES ($1,$2,$3,$4,'feature.created',$5::jsonb)`,
			randID("se"), companyID, id, uid, mustJSONString(map[string]any{"title": title})); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		detail, serr := detailFeature(r.Context(), db, companyID, id)
		if serr != nil {
			writeShipError(w, serr)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, detail)
	}
}

func featurePatch(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireCompany(w, r, db)
		if !ok {
			return
		}
		featureID := r.PathValue("id")
		feature, serr := requireFeature(r.Context(), db, companyID, featureID)
		if serr != nil {
			writeShipError(w, serr)
			return
		}
		if feature.status == "archived" {
			writeShipError(w, fail(http.StatusConflict, "archived features are immutable"))
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
				writeShipError(w, fail(http.StatusBadRequest, "title cannot be empty"))
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
				writeShipError(w, fail(http.StatusBadRequest, "invalid priority"))
				return
			}
			add("priority", p)
		}
		if body.has("riskLevel") {
			rl := body.enumValue("riskLevel", riskLevels, "\x00")
			if rl == "\x00" {
				writeShipError(w, fail(http.StatusBadRequest, "invalid risk level"))
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
		if serr := assertFeatureLinks(r.Context(), db, companyID, links); serr != nil {
			writeShipError(w, serr)
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
			if serr := assertParticipants(r.Context(), db, companyID, ids); serr != nil {
				writeShipError(w, serr)
				return
			}
			add("builder_ids", mustJSONString(ids))
			nextBuilderIDs = ids
		}
		if len(sets) == 0 {
			detail, serr := detailFeature(r.Context(), db, companyID, featureID)
			if serr != nil {
				writeShipError(w, serr)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, detail)
			return
		}
		values = append(values, uid, featureID, companyID)
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE shipping_features SET `+strings.Join(sets, ", ")+`, updated_by = $`+itoa(len(values)-2)+`, updated_at = NOW()
			  WHERE id = $`+itoa(len(values)-1)+` AND company_id = $`+itoa(len(values)), values...); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if nextBuilderIDs != nil {
			if _, err := tx.ExecContext(r.Context(),
				`UPDATE shipping_verifications SET builder_ids = $1::jsonb, updated_at = NOW() WHERE feature_id = $2`,
				mustJSONString(nextBuilderIDs), featureID); err != nil {
				httpx.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		if err := tx.Commit(); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = recordEvent(r.Context(), db, companyID, featureID, uid, "feature.updated", body)
		detail, serr := detailFeature(r.Context(), db, companyID, featureID)
		if serr != nil {
			writeShipError(w, serr)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, detail)
	}
}

func featureTransition(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireCompany(w, r, db)
		if !ok {
			return
		}
		feature, serr := requireFeature(r.Context(), db, companyID, r.PathValue("id"))
		if serr != nil {
			writeShipError(w, serr)
			return
		}
		body := readBody(r)
		to := body.enumValue("status", featureStatuses, "")
		if to == "" {
			writeShipError(w, fail(http.StatusBadRequest, "valid status required"))
			return
		}
		if serr := assertTransitionReady(r.Context(), db, feature.id, feature.status, to); serr != nil {
			writeShipError(w, serr)
			return
		}
		if _, err := db.ExecContext(r.Context(), `
			UPDATE shipping_features SET status = $1, updated_by = $2, updated_at = NOW(),
			       archived_at = CASE WHEN $1 = 'archived' THEN NOW() ELSE archived_at END
			  WHERE id = $3 AND company_id = $4`, to, uid, feature.id, companyID); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = recordEvent(r.Context(), db, companyID, feature.id, uid, "feature.transitioned",
			map[string]any{"from": feature.status, "to": to})
		detail, serr := detailFeature(r.Context(), db, companyID, feature.id)
		if serr != nil {
			writeShipError(w, serr)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, detail)
	}
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
