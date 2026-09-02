-- 0004_agent_transcript.sql —— #260 工具级执行转录:text/thinking/
-- tool_use/tool_result 带 seq 单调存档,回放"agent 当时干了什么"。
-- 保留走 db-gc(挂 DB_GC_AGENT_TRANSCRIPT_DAYS,默认 30 天,随 agent_runs
-- 同窗);bigserial id 供两段式删除批定址。FK 级联随 agent_runs 删除清账。
CREATE TABLE public.agent_transcript (
    id bigserial PRIMARY KEY,
    run_id text NOT NULL REFERENCES public.agent_runs(id) ON DELETE CASCADE,
    agent_id text NOT NULL,
    company_id text,
    seq integer NOT NULL,
    type text NOT NULL,
    tool text,
    content text,
    input jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT agent_transcript_run_seq_unique UNIQUE (run_id, seq)
);
CREATE INDEX idx_agent_transcript_created ON public.agent_transcript (created_at);
