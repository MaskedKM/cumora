-- 0007 — #265 卡片交付台账(Workspace 战役刀 4)。
--
-- worktree-per-task 的记录半边:`cumora card start` 物化 worktree 的同时
-- 落一行(分支可追溯、失败保分支的可见性 —— 任务失败时人打开卡片就能
-- 看到 agent 走到了哪个分支);`cumora card deliver` 补 PR 链接与状态
-- (open|merged|closed)。一卡可多行(重做/多分支交付),按 (card_id,
-- branch) 幂等 upsert。
--
-- 删卡 CASCADE 带走交付行(卡片是交付的存在意义);workspace 解绑不删
-- 行(unbound_at 只软标记,历史交付仍可追溯)。

CREATE TABLE public.card_deliveries (
    id text NOT NULL,
    card_id text NOT NULL,
    workspace_id text NOT NULL,
    branch text NOT NULL,
    pr_url text,
    pr_state text,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT card_deliveries_pkey PRIMARY KEY (id),
    CONSTRAINT card_deliveries_card_fk FOREIGN KEY (card_id)
        REFERENCES public.board_cards (id) ON DELETE CASCADE,
    CONSTRAINT card_deliveries_workspace_fk FOREIGN KEY (workspace_id)
        REFERENCES public.workspaces (id),
    CONSTRAINT card_deliveries_unique UNIQUE (card_id, branch)
);

-- (无独立 card_id 索引:UNIQUE (card_id, branch) 的隐式索引已覆盖其
-- 前缀,再建是纯写放大 —— #343 评审 P2。)
