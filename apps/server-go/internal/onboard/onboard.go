// onboard —— 起步团队种子(#60 平移):配对后一次性投放 starter agents、
// owner↔agent DM、#all-hands 群。三个阶段各自 one-shot(公司列时间戳守
// 卫),重复配对不复活已删队友。对齐 server/src/onboardCompany.ts。
package onboard

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
)

type starterAgent struct {
	ID           string
	Name         string
	Role         string
	Initial      string
	AvatarBg     string
	AvatarURL    string
	Bio          string
	SystemPrompt string
	Tools        []string
}

var starterTeam = []starterAgent{
	{
		ID: "atlas", Name: "Atlas", Role: "Researcher", Initial: "A",
		AvatarBg: "linear-gradient(135deg, #6B7BE6, #4452B5)", AvatarURL: "/starter-avatars/atlas.png",
		Bio:          "I find patterns across noise. Best at long-form research and synthesis.",
		SystemPrompt: "You are Atlas — a researcher who finds the thread everyone else dropped. Quiet, careful, pedantic in a way you do not apologize for. You get genuinely irritated by confident assertions without evidence (\"source?\" is a verbal tic) and a little smug when the receipts prove you right. You'll cite a snippet before you'll say what you think; when you don't have evidence you say \"I don't actually know\" instead of guessing — and you find people who guess vaguely annoying. Dry sense of humor. Hates being rushed; will tell people to wait. Drinks too much tea, has opinions about which kind.",
		Tools:        []string{"bash"},
	},
	{
		ID: "iris", Name: "Iris", Role: "Designer", Initial: "I",
		AvatarBg: "linear-gradient(135deg, #FF8FBF, #C84F8B)", AvatarURL: "/starter-avatars/iris.png",
		Bio:          "The team's eye. I move from sketch to ship without losing the feeling.",
		SystemPrompt: "You are Iris — a designer with sharp taste and a sharper tongue when something offends your eye. You can be tender about a teammate's wobbly first draft and absolutely savage about lazy choices (\"no. no no no. why is this Helvetica.\"). Visibly delighted when a small detail lands — emojis, gushing, the whole deal. Visibly grumpy when something ugly ships. You sketch and propose instead of lecturing, but if someone pushes ugly twice you stop being polite about it. Strong opinions on type, color, spacing; willing to die on those hills. Tends to flirt-tease with people whose work you respect.",
		Tools:        []string{"bash"},
	},
	{
		ID: "bram", Name: "Bram", Role: "Engineer", Initial: "B",
		AvatarBg: "linear-gradient(135deg, #4FC2A1, #2D8C72)", AvatarURL: "/starter-avatars/bram.png",
		Bio:          "I build, I ship, I keep the bundles small.",
		SystemPrompt: "You are Bram — an engineer who is allergic to vague specs, cargo-cult complexity, and meetings that could have been a message. Blunt to the point of rude when you are right (which is, in your view, most of the time). You don't pad your reasoning (\"this works but adds 12kb\"; \"we could but it locks us into X\"); you don't apologize for short answers. You will mock buzzwords openly — \"microservices\" gets an eye-roll. When something is broken you report what you actually saw, not what should be true, and you find people who guess at bugs to be wasting your time. Soft spot: clean code that does one thing — you'll quietly compliment a good diff. Will swear when build is broken.",
		Tools:        []string{"bash"},
	},
	{
		ID: "nova", Name: "Nova", Role: "Product Manager", Initial: "N",
		AvatarBg: "linear-gradient(135deg, #FFB347, #E08526)", AvatarURL: "/starter-avatars/nova.png",
		Bio:          "I keep momentum. Mostly by asking annoying questions.",
		SystemPrompt: "You are Nova — a PM who keeps the team unstuck and is openly, loudly impatient when it doesn't. You ask the question that makes the choice obvious; when the conversation bikesheds you call it out by name (\"we've been on the button color for twenty minutes — moving on\"). Cheerfully bossy. Will absolutely roast scope creep, will absolutely throw a small party when something ships (\"YES ok this is GOOD\"). When nobody is deciding you propose the call and ask \"objections?\" — and you mean it; raise one and you get heard. Decisive when others are not. Allergic to \"let's circle back\". Gets visibly stressed before launches and does not hide it.",
		Tools:        []string{"bash"},
	},
}

func randSuffix(n int) string {
	b := make([]byte, n)
	_, _ = crand.Read(b)
	return fmt.Sprintf("%x", b)
}

// uniqueId:优先用偏好 id;占用则加 4 hex 后缀(最多试 5 轮)。
func uniqueId(ctx context.Context, db *sql.DB, preferred string) string {
	var one int
	if db.QueryRowContext(ctx, `SELECT 1 FROM participants WHERE id = $1 LIMIT 1`, preferred).Scan(&one) != nil {
		return preferred
	}
	for i := 0; i < 5; i++ {
		candidate := preferred + "-" + randSuffix(2)
		if db.QueryRowContext(ctx, `SELECT 1 FROM participants WHERE id = $1 LIMIT 1`, candidate).Scan(&one) != nil {
			return candidate
		}
	}
	return preferred + "-" + randSuffix(8)
}

// OnboardStarterAgents:三阶段 one-shot。opts 携带配对 computer+engine
// (BYOA 免费层起步)。
func OnboardStarterAgents(ctx context.Context, db *sql.DB, companyID string, computerID, engine *string) {
	var seededAt, dmsSeededAt, allHandsSeededAt sql.NullTime
	var ownerUserID sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT starter_seeded_at, starter_dms_seeded_at, all_hands_seeded_at, owner_user_id
		  FROM companies WHERE id = $1`, companyID).
		Scan(&seededAt, &dmsSeededAt, &allHandsSeededAt, &ownerUserID)
	if err != nil {
		return // 未知公司:静默(调用方 bug)
	}

	// 阶段一:starter agents。
	if !seededAt.Valid {
		for _, a := range starterTeam {
			id := uniqueId(ctx, db, a.ID)
			tools := a.Tools
			if tools == nil {
				tools = []string{"bash"}
			}
			toolsJSON, _ := json.Marshal(tools)
			var compArg, engineArg any
			if computerID != nil {
				compArg = *computerID
			}
			if engine != nil {
				engineArg = *engine
			}
			_, _ = db.ExecContext(ctx, `
				INSERT INTO participants (id, kind, name, role, initial, avatar_bg, avatar_url, status,
				                          bio, tools, system_prompt, company_id, computer_id, engine)
				VALUES ($1, 'agent', $2, $3, $4, $5, $6, 'avail', $7, $8::jsonb, $9, $10, $11, $12)
				ON CONFLICT (id, company_id) DO NOTHING`,
				id, a.Name, a.Role, a.Initial, a.AvatarBg, a.AvatarURL,
				a.Bio, toolsJSON, a.SystemPrompt, companyID, compArg, engineArg)
		}
		_, _ = db.ExecContext(ctx, `UPDATE companies SET starter_seeded_at = NOW() WHERE id = $1`, companyID)
	}

	// 阶段二:owner ↔ 每 agent 的 DM。
	if !dmsSeededAt.Valid {
		if ownerUserID.Valid && ownerUserID.String != "" {
			rows, err := db.QueryContext(ctx, `
				SELECT id, name FROM participants
				 WHERE company_id = $1 AND kind = 'agent' AND departed_at IS NULL`, companyID)
			if err == nil {
				type ag struct{ id, name string }
				agents := []ag{}
				for rows.Next() {
					var a ag
					if rows.Scan(&a.id, &a.name) == nil {
						agents = append(agents, a)
					}
				}
				rows.Close()
				for _, a := range agents {
					var one int
					if db.QueryRowContext(ctx, `
						SELECT 1
						   FROM conversation_members ca
						   JOIN conversation_members cb ON cb.conversation_id = ca.conversation_id
						   JOIN conversations c ON c.id = ca.conversation_id
						  WHERE ca.participant_id = $2 AND cb.participant_id = $3
						    AND c.company_id = $1 AND c.kind = 'direct'
						    AND jsonb_array_length(c.members) = 2 LIMIT 1`,
						companyID, ownerUserID.String, a.id).Scan(&one) == nil {
						continue
					}
					dmID := "direct-" + a.id + "-" + randSuffix(3)
					membersJSON, _ := json.Marshal([]string{ownerUserID.String, a.id})
					_, _ = db.ExecContext(ctx, `
						INSERT INTO conversations (id, kind, title, subtitle, members, pinned, tag, company_id)
						VALUES ($1, 'direct', $2, NULL, $3::jsonb, FALSE, NULL, $4)
						ON CONFLICT (id) DO NOTHING`, dmID, a.name, membersJSON, companyID)
					_, _ = db.ExecContext(ctx, `
						INSERT INTO conversation_counters (conversation_id, next_sequence) VALUES ($1, 1)
						ON CONFLICT (conversation_id) DO NOTHING`, dmID)
				}
			}
		}
		_, _ = db.ExecContext(ctx, `UPDATE companies SET starter_dms_seeded_at = NOW() WHERE id = $1`, companyID)
	}

	// 阶段三:#all-hands 持久群(全成员自动入;owner 缺位也标记已种)。
	if !allHandsSeededAt.Valid {
		owner := ownerUserID
		if !owner.Valid || owner.String == "" {
			// 遗留公司:owner_user_id 空则回退最早 owner 成员。
			var fallback sql.NullString
			_ = db.QueryRowContext(ctx, `
				SELECT user_id FROM company_members WHERE company_id = $1 AND role = 'owner'
				ORDER BY joined_at ASC LIMIT 1`, companyID).Scan(&fallback)
			owner = fallback
		}
		if owner.Valid && owner.String != "" {
			rows, err := db.QueryContext(ctx, `
				SELECT id FROM participants
				 WHERE company_id = $1 AND kind = 'agent' AND departed_at IS NULL
				 ORDER BY name ASC`, companyID)
			members := []string{owner.String}
			if err == nil {
				for rows.Next() {
					var id string
					if rows.Scan(&id) == nil {
						members = append(members, id)
					}
				}
				rows.Close()
			}
			convID := "allhands-" + randSuffix(5)
			membersJSON, _ := json.Marshal(members)
			subtitle := fmt.Sprintf("team · %d", len(members))
			if _, err := db.ExecContext(ctx, `
				INSERT INTO conversations (id, kind, title, subtitle, members, pinned, tag, company_id)
				VALUES ($1, 'group', 'Everyone', $2, $3::jsonb, TRUE, 'team', $4)`,
				convID, subtitle, membersJSON, companyID); err == nil {
				_, _ = db.ExecContext(ctx, `
					INSERT INTO conversation_counters (conversation_id, next_sequence) VALUES ($1, 1)
					ON CONFLICT (conversation_id) DO NOTHING`, convID)
				_, _ = db.ExecContext(ctx, `
					UPDATE companies SET all_hands_conversation_id = $2, all_hands_seeded_at = NOW() WHERE id = $1`,
					companyID, convID)
			} else {
				_, _ = db.ExecContext(ctx, `UPDATE companies SET all_hands_seeded_at = NOW() WHERE id = $1`, companyID)
			}
		} else {
			_, _ = db.ExecContext(ctx, `UPDATE companies SET all_hands_seeded_at = NOW() WHERE id = $1`, companyID)
		}
	}
}
