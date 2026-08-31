// /runtime/cli 情感(关系温度)命令组(#89):climate read/note/forget;
// affinity/trust 为 float4,二进制解码经 float32 中转丢精度,::text +
// ParseFloat 对齐 node-pg 文本解析(#60 坑)(原 cli_private.go 拆出,
// 函数体零改动)。
package agent

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

/* ───────── climate(情感系统)───────── */

// clamp11:夹到 [-1,1];垃圾输入归 0。
func clamp11(v any) float64 {
	var n float64
	switch t := v.(type) {
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0
		}
		n = f
	case bool:
		if t {
			return 1 // TS Number(true)=1
		}
		return 0
	case float64:
		n = t
	default:
		return 0
	}
	if n > 1 {
		return 1
	}
	if n < -1 {
		return -1
	}
	return n
}

func fmtSigned2(n float64) string {
	if n >= 0 {
		return fmt.Sprintf("+%.2f", n)
	}
	return fmt.Sprintf("%.2f", n)
}

func (s *Service) cliCmdClimate(ctx context.Context, parsed cliParsed) cliResult {
	op := "read"
	if len(parsed.positional) > 0 && parsed.positional[0] != "" {
		op = parsed.positional[0]
	}
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	switch op {
	case "read":
		about := ""
		if len(parsed.positional) > 1 {
			about = parsed.positional[1]
		}
		args := []any{me}
		where := `agent_id = $1`
		if about != "" {
			args = append(args, about)
			where += fmt.Sprintf(` AND about_id = $%d`, len(args))
		}
		// affinity/trust 是 float4 —— 二进制解码经 float32 中转丢精度,
		// ::text + ParseFloat 对齐 node-pg 文本解析(#60 坑)。
		rows, err := s.DB.QueryContext(ctx,
			`SELECT about_id, affinity::text, trust::text, last_note, updated_at
			   FROM agent_climate WHERE `+where+`
			   ORDER BY updated_at DESC LIMIT 50`, args...)
		if err != nil {
			return cliErrThrow(err)
		}
		defer rows.Close()
		type row struct {
			AboutID   string     `json:"about_id"`
			Affinity  float64    `json:"affinity"`
			Trust     float64    `json:"trust"`
			LastNote  string     `json:"last_note"`
			UpdatedAt cliISOTime `json:"updated_at"`
		}
		var all []row
		for rows.Next() {
			var r row
			var affS, truS string
			if err := rows.Scan(&r.AboutID, &affS, &truS, &r.LastNote, &r.UpdatedAt); err != nil {
				return cliErrThrow(err)
			}
			r.Affinity, _ = strconv.ParseFloat(affS, 64)
			r.Trust, _ = strconv.ParseFloat(truS, 64)
			all = append(all, r)
		}
		if err := rows.Err(); err != nil {
			return cliErrThrow(err)
		}
		if parsed.flagTruey("json") {
			js, e := cliJSONList(all)
			if e != nil {
				return cliErrThrow(e)
			}
			return cliOK(js)
		}
		if len(all) == 0 {
			if about != "" {
				return cliOK(fmt.Sprintf("(no climate noted for %s → %s)", me, about))
			}
			return cliOK(fmt.Sprintf("(no climate notes saved yet for %s)", me))
		}
		plural := "s"
		if len(all) == 1 {
			plural = ""
		}
		lines := []string{fmt.Sprintf("Climate around %s (%d relationship%s):", me, len(all), plural), ""}
		for _, r := range all {
			t := nodeLocaleDate(time.Time(r.UpdatedAt))
			lines = append(lines, "  "+utf16PadEnd(r.AboutID, 10)+"  affinity="+fmtSigned2(r.Affinity)+
				"  trust="+fmtSigned2(r.Trust)+"  "+t+"\n      "+
				strings.ReplaceAll(utf16Slice(r.LastNote, 240), "\n", " \\n "))
		}
		return cliOK(strings.Join(lines, "\n"))
	case "note":
		return s.cliClimateNote(ctx, parsed, me)
	case "forget":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr("usage: climate forget <about_id>")
		}
		aboutID := parsed.positional[1]
		res, err := s.DB.ExecContext(ctx,
			`DELETE FROM agent_climate WHERE agent_id = $1 AND about_id = $2`, me, aboutID)
		if err != nil {
			return cliErrThrow(err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return cliErr("no climate to forget for " + me + " → " + aboutID)
		}
		return cliOK("forgot climate "+me+" → "+aboutID, CliSideEffect{
			"event":   "climate.deleted",
			"command": "climate forget",
			"agentId": me,
			"aboutId": aboutID,
		})
	}
	return cliErr("usage: climate <read|note|forget> [...]")
}

func (s *Service) cliClimateNote(ctx context.Context, parsed cliParsed, me string) cliResult {
	aboutID := ""
	if len(parsed.positional) > 1 {
		aboutID = parsed.positional[1]
	}
	note := strings.TrimSpace(cliUnescapeChat(strings.Join(positionalFrom(parsed, 2), " ")))
	if aboutID == "" || note == "" {
		return cliErr(`usage: climate note <about_id> "<note>" [--affinity -1..1] [--trust -1..1]`)
	}
	affinityFlag, hasAffinity := parsed.flags["affinity"]
	trustFlag, hasTrust := parsed.flags["trust"]
	var prevAffinity, prevTrust float64
	var prevHistory cliRawJSON
	err := s.DB.QueryRowContext(ctx,
		`SELECT affinity::text, trust::text, history FROM agent_climate WHERE agent_id = $1 AND about_id = $2`,
		me, aboutID).Scan(&affinityTextScan{&prevAffinity}, &affinityTextScan{&prevTrust}, &prevHistory)
	if err != nil && err != sql.ErrNoRows {
		return cliErrThrow(err)
	}
	nextAffinity := prevAffinity
	if hasAffinity {
		nextAffinity = clamp11(affinityFlag)
	}
	nextTrust := prevTrust
	if hasTrust {
		nextTrust = clamp11(trustFlag)
	}
	// 追加历史并截到最近 20 条。
	var prevList []any
	if prevHistory != nil && string(prevHistory) != "null" {
		_ = jsonUnmarshal(prevHistory, &prevList)
	}
	if len(prevList) > 19 {
		prevList = prevList[len(prevList)-19:]
	}
	newHistory := append(prevList, map[string]any{
		"at":       isoNowMs(),
		"affinity": nextAffinity,
		"trust":    nextTrust,
		"note":     utf16Slice(note, 400),
	})
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO agent_climate (agent_id, about_id, affinity, trust, last_note, history, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, NOW())
		 ON CONFLICT (agent_id, about_id) DO UPDATE
		   SET affinity = EXCLUDED.affinity,
		       trust    = EXCLUDED.trust,
		       last_note = EXCLUDED.last_note,
		       history   = EXCLUDED.history,
		       updated_at = NOW()`,
		me, aboutID, nextAffinity, nextTrust, utf16Slice(note, 400), compactJSON(newHistory)); err != nil {
		return cliErrThrow(err)
	}
	return cliOK(fmt.Sprintf("climate updated: %s → %s  affinity=%.2f  trust=%.2f", me, aboutID, nextAffinity, nextTrust), CliSideEffect{
		"event":    "climate.updated",
		"command":  "climate note",
		"agentId":  me,
		"aboutId":  aboutID,
		"affinity": nextAffinity,
		"trust":    nextTrust,
	})
}

type affinityTextScan struct{ dst *float64 }

func (a affinityTextScan) Scan(src any) error {
	switch t := src.(type) {
	case nil:
		return nil
	case []byte:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil {
			return err
		}
		*a.dst = f
		return nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return err
		}
		*a.dst = f
		return nil
	default:
		return fmt.Errorf("affinityTextScan: unsupported %T", src)
	}
}
