-- 0010 — #347 评估输入补全:owner 主观评分的存储半边。
--
-- 每公司 × 每 agent 恰一行(复合主键即唯一性),upsert 即"当前评分"——
-- 可反复改,编辑语义=替换(#347 票面"打分/评语并可编辑";历史轨迹不留
-- 表,评估轮的 input_snapshot 自带当时快照可追溯)。score 钳 1..5
-- (CHECK 兜底,API 层同样校验)。评分进入下一轮评估装配(assembleInputs
-- 的 ratings 路),是四路输入里唯一的主观校准信号。

CREATE TABLE public.hr_ratings (
    company_id text NOT NULL,
    agent_id text NOT NULL,
    score integer NOT NULL,
    comment text DEFAULT '' NOT NULL,
    updated_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT hr_ratings_pkey PRIMARY KEY (company_id, agent_id),
    CONSTRAINT hr_ratings_company_fk FOREIGN KEY (company_id)
        REFERENCES public.companies (id) ON DELETE CASCADE,
    CONSTRAINT hr_ratings_score_range CHECK (score >= 1 AND score <= 5)
);
