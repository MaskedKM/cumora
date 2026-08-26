// contacts 核心的 DB 背书测试(#57)。共享 cumora_test(与 TS 集成套件
// 同库不同表);DATABASE_URL 未指到测试库时跳过,CI go job 不跑测试不受
// 影响。运行:DATABASE_URL=postgres://…/cumora_test go test ./internal/contacts/
package contacts

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" || !strings.Contains(url, "test") {
		t.Skip("DATABASE_URL 未指向测试库,跳过")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestListThreeBranchesAndFilter(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	const company = "c-contacts-go"
	const viewer = "u-contacts-viewer"
	cleanup := func() {
		for _, q := range []string{
			`DELETE FROM email_contacts WHERE company_id='` + company + `'`,
			`DELETE FROM participants WHERE company_id='` + company + `'`,
			`DELETE FROM company_members WHERE company_id='` + company + `'`,
			`DELETE FROM users WHERE id LIKE 'u-contacts-%'`,
			`DELETE FROM companies WHERE id='` + company + `'`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1,'Contacts Co','contacts-go',$2)`, company, viewer)
	mustExec(`INSERT INTO users (id, email, display_name) VALUES ($1,$2,$3)`, viewer, "viewer@test.local", "Viewer")
	mustExec(`INSERT INTO company_members (company_id, user_id, role) VALUES ($1,$2,'owner')`, company, viewer)
	mustExec(`INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status) VALUES ($1,$2,'human',$3,'V','#111111','offline')`, viewer, company, "Viewer")
	// agent:email 未铸 → 确定性地址;<id>.<slug>@EMAIL_DOMAIN
	t.Setenv("EMAIL_DOMAIN", "cumora.example")
	mustExec(`INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status, role) VALUES ('ag-contacts-1',$1,'agent','Aurora','A','#222222','offline','Designer')`, company)
	// 外部往来
	mustExec(`INSERT INTO email_contacts (company_id, address, display_name, message_count, last_seen_at)
		VALUES ($1,'wey@ext.example','Wey',3, NOW())`, company)

	all, err := List(ctx, db, company, viewer, "")
	if err != nil {
		t.Fatal(err)
	}
	var hasAgent, hasViewer, hasExt bool
	var agentAddr string
	for _, c := range all {
		switch c.Kind {
		case "agent":
			hasAgent = true
			agentAddr = c.Address
			if c.Role != "Designer" {
				t.Errorf("agent role = %v, want Designer", c.Role)
			}
			if c.ParticipantID != "ag-contacts-1" {
				t.Errorf("agent id = %v", c.ParticipantID)
			}
		case "human":
			hasViewer = true
			if c.Address != "viewer@test.local" {
				t.Errorf("human address = %s", c.Address)
			}
		case "external":
			hasExt = true
			if c.Name != "Wey" {
				t.Errorf("external name = %s", c.Name)
			}
		}
	}
	if !hasAgent || !hasViewer || !hasExt {
		t.Fatalf("branches missing: agent=%v human=%v external=%v", hasAgent, hasViewer, hasExt)
	}
	// agent id "ag-contacts-1" → safeLocalPart:下划线/点转 '-',此处原样
	if agentAddr != "ag-contacts-1.contacts-go@cumora.example" {
		t.Errorf("computed agent address = %s", agentAddr)
	}

	// 过滤:按外部地址子串
	filtered, err := List(ctx, db, company, viewer, "wey")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Kind != "external" {
		t.Fatalf("filter 'wey' = %+v", filtered)
	}
	// 过滤:按 agent role
	byRole, err := List(ctx, db, company, viewer, "designer")
	if err != nil {
		t.Fatal(err)
	}
	if len(byRole) != 1 || byRole[0].Kind != "agent" {
		t.Fatalf("filter 'designer' = %+v", byRole)
	}
}

func TestSanitizers(t *testing.T) {
	// 下划线在 local-part 白名单内;首尾的 -/_. 被剥
	if got := safeLocalPart("AG.Weird_X."); got != "ag-weird_x" {
		t.Errorf("safeLocalPart = %q", got)
	}
	// '.'→'-';尾部的 '!!'→'--' 随尾修剪剥掉
	if got := safeSlugPart("My.Slug!!"); got != "my-slug" {
		t.Errorf("safeSlugPart = %q", got)
	}
}
