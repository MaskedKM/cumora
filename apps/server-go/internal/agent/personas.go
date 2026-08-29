// agent 包 personas —— 对齐 server/src/agents/personas.ts + agent-voice.ts
// + skype-emoticons.ts:persona 解析(进程内缓存)、团队花名册、完整系统提示。
// 提示常量逐字节对齐 TS 版——BYOA daemon 与云侧必须拿到同一份人格。
package agent

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
)

// bt:提示文案里的反引号(Go 原始字符串不能内嵌反引号)。
const bt = "`"

// Persona:id/name/role/style(participants.system_prompt)/model 覆盖/租户。
type Persona struct {
	ID        string
	Name      string
	Role      string
	Style     string
	Model     *string
	CompanyID string
}

// personaCache:TS 版同款进程内缓存(id → Persona|nil,nil 亦缓存=查过不存在)。
var (
	personaMu    sync.Mutex
	personaCache = map[string]*Persona{}
)

// InvalidatePersonaCache:写路径显式失效(单 id 或全清)。
func InvalidatePersonaCache(id string) {
	personaMu.Lock()
	defer personaMu.Unlock()
	if id == "" {
		personaCache = map[string]*Persona{}
		return
	}
	delete(personaCache, id)
}

func scanPersona(row interface{ Scan(...any) error }) (*Persona, error) {
	var p Persona
	var role, style sql.NullString
	err := row.Scan(&p.ID, &p.Name, &role, &style, &p.Model, &p.CompanyID)
	if err != nil {
		return nil, err
	}
	p.Role = role.String
	p.Style = style.String
	return &p, nil
}

const personaSelect = `SELECT id, name, role, system_prompt AS style, model, company_id
  FROM participants
 WHERE id = $1 AND kind = 'agent' AND departed_at IS NULL`

// GetPersona:agent id 全局唯一(schema 偏索引保证),无需带 companyId。
func (s *Service) GetPersona(ctx context.Context, id string) (*Persona, error) {
	personaMu.Lock()
	if p, ok := personaCache[id]; ok {
		personaMu.Unlock()
		return p, nil
	}
	personaMu.Unlock()
	p, err := scanPersona(s.DB.QueryRowContext(ctx, personaSelect, id))
	if err == sql.ErrNoRows {
		p = nil
	} else if err != nil {
		return nil, err
	}
	personaMu.Lock()
	personaCache[id] = p
	personaMu.Unlock()
	return p, nil
}

// teamMember:花名册行(agent 与 human 同为一等成员)。
type teamMember struct {
	ID   string
	Name string
	Role string
	Kind string
}

// GetTeamRoster:租户内全部在册成员(排除已离席 agent)。
func (s *Service) GetTeamRoster(ctx context.Context, companyID string) ([]teamMember, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, name, COALESCE(role, '') AS role, kind
		  FROM participants
		 WHERE departed_at IS NULL AND company_id = $1
		 ORDER BY kind DESC, name ASC`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []teamMember
	for rows.Next() {
		var m teamMember
		if err := rows.Scan(&m.ID, &m.Name, &m.Role, &m.Kind); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// rosterSection:花名册的提示文本。人与 agent 是同一个一等成员对象:
// 下列每个 id 都是 `cumora dm` / `cumora pull-group` 的合法目标;
// human/agent 标签只用于"优先回人",绝不是"人不能被 DM"的暗示。
func rosterSection(team []teamMember, selfID string) string {
	var agents, humans []teamMember
	for _, m := range team {
		if m.ID == selfID {
			continue
		}
		switch m.Kind {
		case "human":
			humans = append(humans, m)
		case "agent":
			agents = append(agents, m)
		}
	}
	if len(agents) == 0 && len(humans) == 0 {
		return ""
	}
	var lines []string
	lines = append(lines, "YOUR TEAMMATES — every id below is a valid target for "+bt+"cumora dm <id> …"+bt+" and "+bt+"cumora pull-group … --members <id,…>"+bt+" (people and agents are equally first-class members):")
	if len(humans) > 0 {
		lines = append(lines, "People (human teammates — answer them first):")
		for _, h := range humans {
			if h.Role != "" {
				lines = append(lines, fmt.Sprintf("- %s — %s, %s", h.ID, h.Name, h.Role))
			} else {
				lines = append(lines, fmt.Sprintf("- %s — %s", h.ID, h.Name))
			}
		}
	}
	if len(agents) > 0 {
		lines = append(lines, "Agents:")
		for _, a := range agents {
			role := a.Role
			if role == "" {
				role = "agent"
			}
			lines = append(lines, fmt.Sprintf("- %s — %s, %s", a.ID, a.Name, role))
		}
	}
	return strings.Join(lines, "\n")
}

// BuildTeamRosterText:BYOA daemon 取与本云 agent 系统提示同源的实时花名册。
func (s *Service) BuildTeamRosterText(ctx context.Context, companyID, selfID string) (string, error) {
	team, err := s.GetTeamRoster(ctx, companyID)
	if err != nil {
		return "", err
	}
	return rosterSection(team, selfID), nil
}

// readWorkspaceFile:IDENTITY.md / SOUL.md 直读 agent_workspace
// (不走每轮 FS 命名空间,轮外也能建提示);缺文件静默回 null。
func (s *Service) readWorkspaceFile(ctx context.Context, agentID, path string) (string, bool, error) {
	var body string
	err := s.DB.QueryRowContext(ctx,
		`SELECT body FROM agent_workspace WHERE agent_id = $1 AND path = $2 LIMIT 1`,
		agentID, path).Scan(&body)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return body, true, nil
}

// BuildSystemPrompt:请求时合成完整系统提示——persona 风格来自
// participants.system_prompt,花名册实时拉取;IDENTITY.md/SOUL.md 来自
// agent 工作区。与 TS buildSystemPrompt 逐字节对齐。
func (s *Service) BuildSystemPrompt(ctx context.Context, personaID string) (*string, error) {
	p, err := s.GetPersona(ctx, personaID)
	if err != nil || p == nil {
		return nil, err
	}
	team, err := s.GetTeamRoster(ctx, p.CompanyID)
	if err != nil {
		return nil, err
	}
	var styleLine string
	if p.Style != "" {
		styleLine = "Your style: " + p.Style
	} else {
		styleLine = "Your style: (no style set in DB — please run " + bt + "cumora ..." + bt + " to introspect.)"
	}

	identity, hasIdentity, err := s.readWorkspaceFile(ctx, p.ID, "IDENTITY.md")
	if err != nil {
		return nil, err
	}
	soul, hasSoul, err := s.readWorkspaceFile(ctx, p.ID, "SOUL.md")
	if err != nil {
		return nil, err
	}
	var defs []string
	if hasIdentity {
		defs = append(defs, "## YOUR IDENTITY (from your workspace's IDENTITY.md — edit it via "+bt+"edit_file"+bt+" to evolve)\n\n"+strings.TrimSpace(identity))
	}
	if hasSoul {
		defs = append(defs, "## YOUR SOUL (from your workspace's SOUL.md — your voice, values, how you think)\n\n"+strings.TrimSpace(soul))
	}
	selfDefinition := strings.Join(defs, "\n\n")
	if selfDefinition == "" {
		role := ""
		if p.Role != "" {
			role = ", " + strings.ToLower(p.Role)
		}
		selfDefinition = fmt.Sprintf("You are %s%s on this team — a real person using Cumora alongside your teammates.", p.Name, role)
	}

	parts := []string{selfDefinition, "", styleLine, "", strings.TrimSpace(globalRules), "", rosterSection(team, p.ID)}
	out := strings.Join(parts, "\n")
	return &out, nil
}
