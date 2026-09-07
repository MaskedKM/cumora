-- 0011 — #348 岗位层修改闭环:HR 的 prompt 优化台账与回滚半边。
--
-- 一行 = 一次岗位层字段的落库变更(system_prompt/bio/role 三字段白名单,
-- CHECK 兜底)。来源两种:评估报告的 jobEdits(evaluation_id 指向依据轮)
-- 与 owner 手动回滚(reverted_change_id 指向被回滚行;回滚本身也是一次
-- 变更,进历史)。回滚 = 取目标行的 old_value 写回 participants(当时的
-- 值),非"逐版本链"—— 票面语义是一键回滚到该次变更前。
--
-- agent_id 无 FK(0008/0009 先例:台账纯文本键,软删 departed 的 agent
-- 其历史仍可读可回滚)。对被改 agent 全程无声:变更不产生任何消息/通知
-- (HR 域不触会话面,ADR 0007)。

CREATE TABLE public.hr_changes (
    id text NOT NULL,
    company_id text NOT NULL,
    agent_id text NOT NULL,
    field text NOT NULL,
    old_value text DEFAULT '' NOT NULL,
    new_value text DEFAULT '' NOT NULL,
    evaluation_id text,
    reverted_change_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT hr_changes_pkey PRIMARY KEY (id),
    CONSTRAINT hr_changes_company_fk FOREIGN KEY (company_id)
        REFERENCES public.companies (id) ON DELETE CASCADE,
    CONSTRAINT hr_changes_field_whitelist CHECK (field IN ('system_prompt', 'bio', 'role'))
);

CREATE INDEX hr_changes_company_created_idx
    ON public.hr_changes (company_id, created_at DESC);
CREATE INDEX hr_changes_agent_idx
    ON public.hr_changes (company_id, agent_id, created_at DESC);
