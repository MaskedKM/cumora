-- 0012 — #349 招人/淘汰提案审批:花名册进出的人审批台账。
--
-- 一行 = 一条提案。来源:评估报告 payload 的 proposals(hire=完整档案
-- 草稿+依据;offboard=目标 agent+依据),cliReport 收轮时校验落库
-- (status open),不执行 —— 执行权在 owner 批准(approveHrProposal)。
-- 批准 hire 走 agents 域 createAgentCore 同源路径(入职副作用齐全);
-- 批准 offboard 走 OffboardAgentCore(软删可 rehire)。result_agent_id
-- 记 hire 的执行产物;拒绝只留 decided 痕迹。
--
-- agent_id/profile/evaluation_id 均无 FK(0008 起台账纯文本键先例)。

CREATE TABLE public.hr_proposals (
    id text NOT NULL,
    company_id text NOT NULL,
    kind text NOT NULL,
    agent_id text,
    profile jsonb,
    reason text DEFAULT '' NOT NULL,
    evaluation_id text,
    status text DEFAULT 'open' NOT NULL,
    decided_by text,
    decided_at timestamp with time zone,
    result_agent_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT hr_proposals_pkey PRIMARY KEY (id),
    CONSTRAINT hr_proposals_company_fk FOREIGN KEY (company_id)
        REFERENCES public.companies (id) ON DELETE CASCADE,
    CONSTRAINT hr_proposals_kind_check CHECK (kind IN ('hire', 'offboard')),
    CONSTRAINT hr_proposals_status_check CHECK (status IN ('open', 'approved', 'rejected'))
);

CREATE INDEX hr_proposals_company_status_idx
    ON public.hr_proposals (company_id, status, created_at DESC);
