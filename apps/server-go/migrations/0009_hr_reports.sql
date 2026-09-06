-- 0009 — #346 HR Agent 手动评估全链:评估轮次的报告台账。
--
-- 一行 = 一次评估轮(手动/周期/事件触发;刀 2 只落 manual,另两枚举
-- 为 #350 预留)。生命周期:pending(已触发)→ running(Brain 取输入即
-- 开跑,cliContext 翻转)→ done/failed(报告落库)。payload 存 HR Brain
-- 产出的结构化评估(每 agent 评分/发现/建议);input_snapshot 存触发时刻
-- 装配的客观观测快照(CLI hr context 原样回放给 Brain,复现可考)。
--
-- 在飞互斥:部分唯一索引保证每公司同时至多一轮 pending/running ——
-- 连点两次触发/周期撞上手动,数据库层面兜住(#346 幂等验收)。
-- target_agent_id 无 FK(0008 先例:观测/台账面纯文本键);run 归因走
-- agent_runs.agent_id = 'hr-<companyId>' 约定(评审 P1-3:不设 run_id
-- 列,归因键即关联)。

CREATE TABLE public.hr_reports (
    id text NOT NULL,
    company_id text NOT NULL,
    target_agent_id text,
    trigger_kind text NOT NULL,
    status text NOT NULL,
    payload jsonb,
    error text,
    input_snapshot jsonb,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone,
    CONSTRAINT hr_reports_pkey PRIMARY KEY (id),
    CONSTRAINT hr_reports_company_fk FOREIGN KEY (company_id)
        REFERENCES public.companies (id) ON DELETE CASCADE
);

-- 在飞互斥(partial unique 不能做内联 CONSTRAINT,独立部分唯一索引):
-- 每公司同时至多一轮 pending/running。
CREATE UNIQUE INDEX hr_reports_inflight_unique
    ON public.hr_reports (company_id)
    WHERE status IN ('pending', 'running');

CREATE INDEX hr_reports_company_created_idx
    ON public.hr_reports (company_id, created_at DESC);
