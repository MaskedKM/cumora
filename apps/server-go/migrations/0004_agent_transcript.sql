-- 0004_agent_transcript.sql —— #260 工具级执行转录:text/thinking/
-- tool_use/tool_result 带 seq 单调存档,回放"agent 当时干了什么"。
-- 保留走 db-gc(挂 DB_GC_AGENT_TRANSCRIPT_DAYS,默认 30 天,随 agent_runs
-- 同窗)。id 走 text + UUIDHex(与 agent_log/agent_events 同惯例——gc 两段
-- 删除的 pk 扫描按 text 定址,bigint 会让 Scan 直接报错键失效)。FK 级联
-- 随 agent_runs 删除清账。
CREATE TABLE public.agent_transcript (
    id text NOT NULL PRIMARY KEY,
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
