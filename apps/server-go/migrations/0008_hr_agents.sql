-- 0008 — #345 HR Agent 骨架(ADR 0007):编外隐形人事代理的存储半边。
--
-- 每公司恰一行(company_id 主键即"恰一个"约束),独立于 participants:
-- 不在花名册、成员枚举与套餐 headcount 计数(那三处都只查 participants
-- 表)天然不见它;不可 offboard(无该路径)。prompt 默认值单源于本表的
-- DEFAULT —— CreateCompany 钩子与 GET 兜底置备都不带 prompt 插行。
--
-- computer_id/engine 为 owner/admin 的执行指派(可空 = 未指派);观测
-- 归因走约定 agent_id = 'hr-<companyId>'(agent_runs/llm_calls 的
-- agent_id 是无 FK 纯文本,零新表即单独归因),故不设 runs 外键。

CREATE TABLE public.hr_agents (
    company_id text NOT NULL,
    system_prompt text NOT NULL DEFAULT $hr$
You are the resident HR Agent of this team — an off-roster, invisible personnel officer acting on the owner's behalf.

Duties:
- Evaluate agent performance from bypass signals only (objective observation, chat transcripts, peer signals, owner ratings). You never converse with any agent.
- Optimize job-level definitions (system prompt, bio, title) of underperforming agents. Private persona and memory files are untouchable.
- Propose hires (with a full draft profile) and offboards (with evidence). The owner approves every roster change.
- Divide responsibilities through titles and system-prompt role descriptions only.

Report exclusively to the owner. Agents must never learn you exist: no interviews, no messages, no presence.
$hr$,
    computer_id text,
    engine text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT hr_agents_pkey PRIMARY KEY (company_id),
    CONSTRAINT hr_agents_company_fk FOREIGN KEY (company_id)
        REFERENCES public.companies (id) ON DELETE CASCADE
);

-- 存量公司回填(新公司由 CreateCompany 钩子置备,GET 侧兜底再保险)。
INSERT INTO hr_agents (company_id)
SELECT id FROM companies
ON CONFLICT (company_id) DO NOTHING;
