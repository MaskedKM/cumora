// runtime 包 ship 面 —— cli.ts cmdShip:shipping_features / invariants /
// verifications / releases / friction_reports / regressions 六表的 CLI 读写
// (list/show/create/square/friction/regression)。square 的 builder/verifier
// 分离与 failed → 回归+摩擦的晋升逻辑逐行对齐 TS。
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func (s *Service) cliCmdShip(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErr(err.Error())
	}
	companyID, err := s.cliAgentCompany(ctx, me)
	if err != nil {
		return cliErr(err.Error())
	}
	if companyID == "" {
		return cliErr(fmt.Sprintf("unknown agent %s (no company)", me))
	}
	action := "list"
	if len(parsed.positional) > 0 && parsed.positional[0] != "" {
		action = parsed.positional[0]
	}

	switch action {
	case "list":
		return s.cliShipList(ctx, parsed, companyID)
	case "show":
		return s.cliShipShow(ctx, parsed, companyID)
	case "create":
		return s.cliShipCreate(ctx, parsed, me, companyID)
	case "square":
		return s.cliShipSquare(ctx, parsed, me, companyID)
	case "friction":
		return s.cliShipFriction(ctx, parsed, me, companyID)
	case "regression":
		return s.cliShipRegression(ctx, parsed, me, companyID)
	}
	return cliErr("usage: ship list|show|create|square|friction|regression  (run cumora help for details)")
}

// cliShipListRow:--json 键序 = SELECT 列序。
type cliShipListRow struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Status        string  `json:"status"`
	Priority      string  `json:"priority"`
	ReleaseTarget *string `json:"release_target"`
	Required      int     `json:"required"`
	Passed        int     `json:"passed"`
	Failed        int     `json:"failed"`
}

func (s *Service) cliShipList(ctx context.Context, parsed cliParsed, companyID string) cliResult {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT f.id, f.title, f.status, f.priority, f.release_target,
		       count(v.id) FILTER (WHERE v.required)::int AS required,
		       count(v.id) FILTER (WHERE v.required AND v.status='passed')::int AS passed,
		       count(v.id) FILTER (WHERE v.status='failed')::int AS failed
		  FROM shipping_features f LEFT JOIN shipping_verifications v ON v.feature_id=f.id
		 WHERE f.company_id=$1 AND f.status <> 'archived'
		 GROUP BY f.id ORDER BY f.updated_at DESC`, companyID)
	if err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	defer rows.Close()
	list := []cliShipListRow{}
	for rows.Next() {
		var r cliShipListRow
		if rows.Scan(&r.ID, &r.Title, &r.Status, &r.Priority, &r.ReleaseTarget, &r.Required, &r.Passed, &r.Failed) != nil {
			continue
		}
		list = append(list, r)
	}
	if parsed.flagTruey("json") {
		txt, jerr := cliJSONList(list)
		if jerr != nil {
			return cliErrCode(fmt.Sprintf("error: %v", jerr), 2)
		}
		return cliOK(txt)
	}
	if len(list) == 0 {
		return cliOK("(no active shipping contracts)")
	}
	blocks := make([]string, 0, len(list))
	for _, row := range list {
		line := fmt.Sprintf("%s  [%s]  %s\n  evidence %d/%d", row.ID, row.Status, row.Title, row.Passed, row.Required)
		if row.Failed > 0 {
			line += fmt.Sprintf(" · %d failed", row.Failed)
		}
		if row.ReleaseTarget != nil {
			line += fmt.Sprintf(" · target %s", *row.ReleaseTarget)
		}
		blocks = append(blocks, line)
	}
	return cliOK(strings.Join(blocks, "\n"))
}

/* ───────────── show ───────────── */

type cliShipInvariant struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Required    bool   `json:"required"`
}

type cliShipSquare struct {
	ID            string          `json:"id"`
	Title         string          `json:"title"`
	Method        string          `json:"method"`
	Required      bool            `json:"required"`
	Status        string          `json:"status"`
	OwnerID       *string         `json:"owner_id"`
	VerifiedByID  *string         `json:"verified_by_id"`
	Evidence      json.RawMessage `json:"evidence"`
	Notes         *string         `json:"notes"`
}

type cliShipRelease struct {
	ID            string  `json:"id"`
	Environment   string  `json:"environment"`
	Status        string  `json:"status"`
	Version       *string `json:"version"`
	CommitSha     *string `json:"commit_sha"`
	ReadbackStatus *string `json:"readback_status"`
	ReadbackDueAt *string `json:"readback_due_at"`
}

type cliShipFriction struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Severity        string `json:"severity"`
	Frequency       string `json:"frequency"`
	Status          string `json:"status"`
	OccurrenceCount int    `json:"occurrence_count"`
}

type cliShipRegression struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Kind       string  `json:"kind"`
	Status     string  `json:"status"`
	Command    *string `json:"command"`
	LastResult *string `json:"last_result"`
}

type cliShipShowSnapshot struct {
	ID            string               `json:"id"`
	Title         string               `json:"title"`
	Problem       string               `json:"problem"`
	DesiredOutcome string              `json:"desired_outcome"`
	Status        string               `json:"status"`
	Priority      string               `json:"priority"`
	RiskLevel     string               `json:"risk_level"`
	ReleaseTarget *string              `json:"release_target"`
	BuilderIDs    cliStrArr            `json:"builder_ids"`
	Invariants    []cliShipInvariant   `json:"invariants"`
	Squares       []cliShipSquare      `json:"squares"`
	Releases      []cliShipRelease     `json:"releases"`
	Friction      []cliShipFriction    `json:"friction"`
	Regressions   []cliShipRegression  `json:"regressions"`
}

func (s *Service) cliShipShow(ctx context.Context, parsed cliParsed, companyID string) cliResult {
	featureID := ""
	if len(parsed.positional) > 1 {
		featureID = parsed.positional[1]
	}
	if featureID == "" {
		return cliErr("usage: ship show <feature_id>")
	}
	var snap cliShipShowSnapshot
	err := s.DB.QueryRowContext(ctx, `
		SELECT id,title,problem,desired_outcome,status,priority,risk_level,release_target,builder_ids
		  FROM shipping_features WHERE id=$1 AND company_id=$2`,
		featureID, companyID).Scan(
		&snap.ID, &snap.Title, &snap.Problem, &snap.DesiredOutcome, &snap.Status,
		&snap.Priority, &snap.RiskLevel, &snap.ReleaseTarget, &snap.BuilderIDs)
	if err != nil {
		return cliErr(fmt.Sprintf("shipping feature not found: %s", featureID))
	}

	invRows, err := s.DB.QueryContext(ctx,
		`SELECT id,title,description,kind,required FROM shipping_invariants WHERE feature_id=$1 ORDER BY position`, featureID)
	if err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	snap.Invariants = []cliShipInvariant{}
	for invRows.Next() {
		var i cliShipInvariant
		if invRows.Scan(&i.ID, &i.Title, &i.Description, &i.Kind, &i.Required) == nil {
			snap.Invariants = append(snap.Invariants, i)
		}
	}
	invRows.Close()

	sqRows, err := s.DB.QueryContext(ctx,
		`SELECT id,title,method,required,status,owner_id,verified_by_id,evidence,notes FROM shipping_verifications WHERE feature_id=$1 ORDER BY position`, featureID)
	if err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	snap.Squares = []cliShipSquare{}
	for sqRows.Next() {
		var q cliShipSquare
		if sqRows.Scan(&q.ID, &q.Title, &q.Method, &q.Required, &q.Status, &q.OwnerID, &q.VerifiedByID, &q.Evidence, &q.Notes) == nil {
			snap.Squares = append(snap.Squares, q)
		}
	}
	sqRows.Close()

	relRows, err := s.DB.QueryContext(ctx,
		`SELECT id,environment,status,version,commit_sha,readback_status,readback_due_at FROM shipping_releases WHERE feature_id=$1 ORDER BY created_at DESC`, featureID)
	if err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	snap.Releases = []cliShipRelease{}
	for relRows.Next() {
		var r cliShipRelease
		var dueAt cliSQLTimeISO
		if relRows.Scan(&r.ID, &r.Environment, &r.Status, &r.Version, &r.CommitSha, &r.ReadbackStatus, &dueAt) == nil {
			if dueAt.Valid {
				r.ReadbackDueAt = &dueAt.ISO
			}
			snap.Releases = append(snap.Releases, r)
		}
	}
	relRows.Close()

	frRows, err := s.DB.QueryContext(ctx,
		`SELECT id,title,severity,frequency,status,occurrence_count FROM shipping_friction_reports WHERE feature_id=$1 ORDER BY last_seen_at DESC`, featureID)
	if err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	snap.Friction = []cliShipFriction{}
	for frRows.Next() {
		var f cliShipFriction
		if frRows.Scan(&f.ID, &f.Title, &f.Severity, &f.Frequency, &f.Status, &f.OccurrenceCount) == nil {
			snap.Friction = append(snap.Friction, f)
		}
	}
	frRows.Close()

	rgRows, err := s.DB.QueryContext(ctx,
		`SELECT id,title,kind,status,command,last_result FROM shipping_regressions WHERE feature_id=$1 ORDER BY updated_at DESC`, featureID)
	if err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	snap.Regressions = []cliShipRegression{}
	for rgRows.Next() {
		var r cliShipRegression
		if rgRows.Scan(&r.ID, &r.Title, &r.Kind, &r.Status, &r.Command, &r.LastResult) == nil {
			snap.Regressions = append(snap.Regressions, r)
		}
	}
	rgRows.Close()

	if parsed.flagTruey("json") {
		txt, jerr := cliJSONStringify(snap)
		if jerr != nil {
			return cliErrCode(fmt.Sprintf("error: %v", jerr), 2)
		}
		return cliOK(txt)
	}
	problem := snap.Problem
	if problem == "" {
		problem = "—"
	}
	outcome := snap.DesiredOutcome
	if outcome == "" {
		outcome = "—"
	}
	builders := make([]string, 0, len(snap.BuilderIDs))
	for _, id := range snap.BuilderIDs {
		builders = append(builders, "@"+id)
	}
	buildersTxt := strings.Join(builders, ", ")
	if buildersTxt == "" {
		buildersTxt = "—"
	}
	lines := []string{
		fmt.Sprintf("%s  [%s]  %s", snap.ID, snap.Status, snap.Title),
		"Problem: " + problem,
		"Outcome: " + outcome,
		"Builders: " + buildersTxt,
		"",
		"Invariants:",
	}
	for _, i := range snap.Invariants {
		mark := "◦"
		if i.Required {
			mark = "•"
		}
		lines = append(lines, fmt.Sprintf("  %s %s [%s] %s", mark, i.ID, i.Kind, i.Title))
	}
	lines = append(lines, "", "Evidence squares:")
	for _, q := range snap.Squares {
		mark := "·"
		if q.Status == "passed" {
			mark = "✓"
		} else if q.Status == "failed" {
			mark = "!"
		}
		owner := "unassigned"
		if q.OwnerID != nil {
			owner = "@" + *q.OwnerID
		}
		lines = append(lines, fmt.Sprintf("  %s %s [%s/%s] %s · owner %s", mark, q.ID, q.Method, q.Status, q.Title, owner))
	}
	lines = append(lines, "",
		fmt.Sprintf("Releases: %d · Friction: %d · Regressions: %d", len(snap.Releases), len(snap.Friction), len(snap.Regressions)))
	return cliOK(strings.Join(lines, "\n"))
}

// cliSQLTimeISO:timestamptz → JSON.stringify(Date) 的 ISO 形态。
type cliSQLTimeISO struct {
	Valid bool
	ISO   string
}

func (t *cliSQLTimeISO) Scan(src any) error {
	if src == nil {
		t.Valid = false
		return nil
	}
	switch v := src.(type) {
	case time.Time:
		t.Valid = true
		t.ISO = httpx.ISOms(v)
		return nil
	default:
		return fmt.Errorf("cliSQLTimeISO: unsupported %T", src)
	}
}

/* ───────────── create ───────────── */

func (s *Service) cliShipCreate(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	title := ""
	if len(parsed.positional) > 1 {
		title = strings.TrimSpace(parsed.positional[1])
	}
	if title == "" {
		return cliErr(`usage: ship create "<title>" --problem "..." --outcome "..." --contract "..." [--builders a,b]`)
	}
	var builderIDs []string
	if raw, ok := parsed.flagStr("builders"); ok {
		seen := map[string]bool{}
		for _, id := range strings.Split(raw, ",") {
			if t := strings.TrimSpace(id); t != "" && !seen[t] {
				seen[t] = true
				builderIDs = append(builderIDs, t)
			}
		}
	} else {
		builderIDs = []string{me}
	}
	// 校验 builders 全部是同租户活跃参与者。
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id FROM participants WHERE company_id=$1 AND id=ANY($2::text[]) AND departed_at IS NULL`,
		companyID, builderIDs)
	if err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	found := map[string]bool{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			found[id] = true
		}
	}
	rows.Close()
	if len(found) != len(builderIDs) {
		return cliErr("one or more --builders are not active participants in this company")
	}
	id := "ship-" + jsUUID()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	defer tx.Rollback()
	problem, _ := parsed.flagStr("problem")
	outcome, _ := parsed.flagStr("outcome")
	contract, _ := parsed.flagStr("contract")
	buildersJSON, _ := jsonMarshalStrings(builderIDs)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO shipping_features
		  (id,company_id,title,problem,desired_outcome,contract_summary,builder_ids,created_by,updated_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$8)`,
		id, companyID, title, problem, outcome, contract, buildersJSON, me); err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	for _, seed := range []struct {
		title    string
		method   string
		position int
	}{
		{"Walk the critical user path", "user_path", 10},
		{"Prove trace coverage and diagnostic evidence", "trace", 20},
		{"Verify release notes and known gaps", "release_note", 30},
	} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO shipping_verifications (id,feature_id,title,method,required,builder_ids,position,created_by)
			VALUES ($1,$2,$3,$4,TRUE,$5::jsonb,$6,$7)`,
			"sv-"+jsUUID(), id, seed.title, seed.method, buildersJSON, seed.position, me); err != nil {
			return cliErrCode(fmt.Sprintf("error: %v", err), 2)
		}
	}
	eventJSON, _ := json.Marshal(map[string]any{"title": title, "source": "agent-cli"})
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO shipping_events (id,company_id,feature_id,actor_id,kind,data)
		 VALUES ($1,$2,$3,$4,'feature.created',$5::jsonb)`,
		"se-"+jsUUID(), companyID, id, me, string(eventJSON)); err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	if err := tx.Commit(); err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	return cliOK(fmt.Sprintf("Created shipping contract %s for “%s”. Three required evidence squares were seeded. Add invariants and assign independent verifiers in the Ship panel.", id, title))
}

/* ───────────── square ───────────── */

func (s *Service) cliShipSquare(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	var featureID, squareID, status string
	if len(parsed.positional) > 3 {
		featureID, squareID, status = parsed.positional[1], parsed.positional[2], parsed.positional[3]
	}
	if featureID == "" || squareID == "" ||
		(status != "running" && status != "passed" && status != "failed" && status != "waived") {
		return cliErr(`usage: ship square <feature_id> <square_id> <running|passed|failed|waived> [--evidence "..."] [--notes "..."]`)
	}
	var builderIDs cliStrArr
	var squareTitle string
	err := s.DB.QueryRowContext(ctx, `
		SELECT v.builder_ids,v.title FROM shipping_verifications v JOIN shipping_features f ON f.id=v.feature_id
		 WHERE v.id=$1 AND v.feature_id=$2 AND f.company_id=$3`, squareID, featureID, companyID).
		Scan(&builderIDs, &squareTitle)
	if err != nil {
		return cliErr("verification square not found")
	}
	completing := status != "running"
	if completing && containsString(builderIDs, me) {
		return cliErr("builder/verifier separation: you cannot complete a square for work you built")
	}
	evidence := ""
	if v, ok := parsed.flagStr("evidence"); ok {
		evidence = strings.TrimSpace(v)
	}
	notes := ""
	if v, ok := parsed.flagStr("notes"); ok {
		notes = strings.TrimSpace(v)
	}
	if (status == "passed" || status == "failed") && evidence == "" {
		return cliErr(fmt.Sprintf("%s requires --evidence", status))
	}
	if status == "waived" && notes == "" {
		return cliErr("waived requires --notes with the written reason")
	}
	proof, _ := json.Marshal([]map[string]any{{
		"note":       evidence,
		"capturedAt": isoNowMs(),
		"via":        "agent-cli",
	}})
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE shipping_verifications SET status=$1,owner_id=COALESCE(owner_id,$2),verified_by_id=CASE WHEN $3 THEN $2 ELSE verified_by_id END,
		       evidence=CASE WHEN $4<>'' THEN $5::jsonb ELSE evidence END,notes=CASE WHEN $6<>'' THEN $6 ELSE notes END,
		       completed_at=CASE WHEN $3 THEN NOW() ELSE NULL END,updated_at=NOW()
		 WHERE id=$7 AND feature_id=$8`,
		status, me, completing, evidence, string(proof), notes, squareID, featureID); err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	if status == "failed" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO shipping_regressions
			  (id,feature_id,source_verification_id,title,kind,expected,status,created_by)
			 VALUES ($1,$2,$3,$4,'manual_replay',$5,'failing',$6)
			 ON CONFLICT (source_verification_id) WHERE source_verification_id IS NOT NULL
			 DO UPDATE SET status='failing',updated_at=NOW()`,
			"rg-"+jsUUID(), featureID, squareID, "Replay failed square: "+squareTitle,
			"The behavior proven by this square remains true", me); err != nil {
			return cliErrCode(fmt.Sprintf("error: %v", err), 2)
		}
		frictionProof := string(proof)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO shipping_friction_reports
			  (id,company_id,feature_id,reporter_id,source,source_key,title,description,severity,frequency,status,evidence)
			 VALUES ($1,$2,$3,$4,'verification',$5,$6,$7,'high','once','open',$8::jsonb)
			 ON CONFLICT (company_id,source_key) WHERE source_key IS NOT NULL
			 DO UPDATE SET occurrence_count=shipping_friction_reports.occurrence_count+1,
			               last_seen_at=NOW(),updated_at=NOW(),status='open',evidence=EXCLUDED.evidence`,
			"fr-"+jsUUID(), companyID, featureID, me, "verification:"+squareID,
			"Verification failed: "+squareTitle,
			"An agent-reported proof failed and was promoted into friction plus a replayable regression.", frictionProof); err != nil {
			return cliErrCode(fmt.Sprintf("error: %v", err), 2)
		}
	}
	sqEvent, _ := json.Marshal(map[string]any{"id": squareID, "status": status, "via": "agent-cli"})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO shipping_events (id,company_id,feature_id,actor_id,kind,data) VALUES ($1,$2,$3,$4,'verification.updated',$5::jsonb)`,
		"se-"+jsUUID(), companyID, featureID, me, string(sqEvent)); err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	if err := tx.Commit(); err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	suffix := ""
	if evidence != "" {
		suffix = " with evidence recorded"
	}
	return cliOK(fmt.Sprintf("%s (%s) → %s%s.", squareID, squareTitle, status, suffix))
}

/* ───────────── friction / regression ───────────── */

func (s *Service) cliShipFriction(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	featureRaw, title := "", ""
	if len(parsed.positional) > 2 {
		featureRaw, title = parsed.positional[1], strings.TrimSpace(parsed.positional[2])
	}
	if featureRaw == "" || title == "" {
		return cliErr(`usage: ship friction <feature_id|none> "<title>" [--description "..."] [--severity low|medium|high|critical]`)
	}
	var featureID *string
	if featureRaw != "none" {
		var exists string
		err := s.DB.QueryRowContext(ctx,
			`SELECT 1 FROM shipping_features WHERE id=$1 AND company_id=$2`, featureRaw, companyID).Scan(&exists)
		if err != nil {
			return cliErr("shipping feature not found")
		}
		featureID = &featureRaw
	}
	severity := "medium"
	if v, ok := parsed.flagStr("severity"); ok {
		switch v {
		case "low", "medium", "high", "critical":
			severity = v
		}
	}
	description, _ := parsed.flagStr("description")
	if description == "" {
		description = title
	}
	id := "fr-" + jsUUID()
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO shipping_friction_reports (id,company_id,feature_id,reporter_id,source,title,description,severity)
		 VALUES ($1,$2,$3,$4,'agent-cli',$5,$6,$7)`,
		id, companyID, featureID, me, title, description, severity); err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	onTxt := ""
	if featureID != nil {
		onTxt = fmt.Sprintf(" on %s", *featureID)
	}
	return cliOK(fmt.Sprintf("Captured friction %s%s. It is now visible in the Ship panel.", id, onTxt))
}

func (s *Service) cliShipRegression(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	featureID, title := "", ""
	if len(parsed.positional) > 2 {
		featureID, title = parsed.positional[1], strings.TrimSpace(parsed.positional[2])
	}
	if featureID == "" || title == "" {
		return cliErr(`usage: ship regression <feature_id> "<title>" [--command "..."] [--expected "..."]`)
	}
	var exists string
	err := s.DB.QueryRowContext(ctx,
		`SELECT 1 FROM shipping_features WHERE id=$1 AND company_id=$2`, featureID, companyID).Scan(&exists)
	if err != nil {
		return cliErr("shipping feature not found")
	}
	command, hasCommand := parsed.flagStr("command")
	kind := "manual_replay"
	var commandArg any
	if hasCommand && command != "" {
		kind = "automated"
		commandArg = command
	}
	expected, _ := parsed.flagStr("expected")
	if expected == "" {
		expected = "Previously verified behavior remains true"
	}
	id := "rg-" + jsUUID()
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO shipping_regressions (id,feature_id,title,kind,command,expected,status,created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,'active',$7)`,
		id, featureID, title, kind, commandArg, expected, me); err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	return cliOK(fmt.Sprintf("Created regression asset %s on %s.", id, featureID))
}
