-- 0013 — #350 HR Agent 自动运行:周期例行 + 事件钩子的每公司配置。
--
-- 全部挂在 hr_agents 上(每公司一行的既有形态),默认值即 SQL 单源:
--   auto_interval_hours 周期例行间隔(168=每周;0=关)
--   auto_last_run_at    上次例行入队时刻(下次到期 = 它 + interval;
--                       DEFAULT now() ⇒ 新公司/存量公司自置备起算一个
--                       周期,不会服务器一启动就全员开评)
--   event_overdue_days  事件钩子①:已指派看板卡停更超阈(0=关)
--   event_spend_usd     事件钩子②:近 24h LLM spend 超阈(0=关)
--   event_error_rate    事件钩子③:近 24h 错误率超阈[0,1](0=关)
-- 阈值即开关:置 0 关单个钩子,免另设 enable 列。触发轮落既有
-- hr_reports(trigger_kind='periodic'/'event',0009 已预留枚举),在飞
-- 互斥与 24h 同目标去抖由查询承担,零新表零新索引。
ALTER TABLE public.hr_agents
    ADD COLUMN auto_interval_hours int NOT NULL DEFAULT 168
        CONSTRAINT hr_agents_auto_interval_check CHECK (auto_interval_hours BETWEEN 0 AND 2160),
    ADD COLUMN auto_last_run_at timestamp with time zone NOT NULL DEFAULT now(),
    ADD COLUMN event_overdue_days int NOT NULL DEFAULT 3
        CONSTRAINT hr_agents_overdue_check CHECK (event_overdue_days BETWEEN 0 AND 365),
    ADD COLUMN event_spend_usd double precision NOT NULL DEFAULT 5
        CONSTRAINT hr_agents_spend_check CHECK (event_spend_usd >= 0),
    ADD COLUMN event_error_rate double precision NOT NULL DEFAULT 0.5
        CONSTRAINT hr_agents_error_rate_check CHECK (event_error_rate BETWEEN 0 AND 1);
