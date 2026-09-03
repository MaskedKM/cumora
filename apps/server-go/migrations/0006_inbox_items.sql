-- 0006_inbox_items.sql —— #264 人侧 Inbox 分级:triage 输出接人的注意力
-- 管理。severity 三档:action_required(需要人裁决才打扰:推送+弹条)/
-- attention(值得看见,只落账+轻徽标)/info(纯落账)。生成面挂事件
-- (run 失败/看板流转/日历派发),type 是静音偏好(user_preferences
-- .prefs.inboxMutedTypes)的键。人类 participant 与 users 同 id,故
-- user_id 直通 push_devices 的收件定址。
CREATE TABLE public.inbox_items (
    id text NOT NULL PRIMARY KEY,
    company_id text NOT NULL REFERENCES public.companies(id) ON DELETE CASCADE,
    user_id text NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    severity text NOT NULL,
    type text NOT NULL,
    title text NOT NULL,
    body text,
    link_kind text,
    link_id text,
    read_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE INDEX idx_inbox_items_user_unread ON public.inbox_items (user_id, created_at DESC) WHERE (read_at IS NULL);
CREATE INDEX idx_inbox_items_company ON public.inbox_items (company_id, created_at DESC);
