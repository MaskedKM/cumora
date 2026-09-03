-- 0005_company_skills.sql —— #261 公司级 Skills 库(公司 SOP 手册):
-- 一次沉淀、全员复用。内容寻址分发:整包 bundle_hash(文件按 path 排序
-- 的长度前缀拼接体做 sha256)是 daemon 侧的缓存键——哈希不变不重拉,
-- 变更即整体重物化到各引擎原生 skills 目录。文件集存 jsonb(≤100 文件
-- /每文件 ≤256KB,SKILL.md 必在根),与 agent 私有 skills
-- (agent_workspace)分属两个命名空间:私有技能走 cumora CLI 渐进披露,
-- 公司技能走引擎原生加载器。
CREATE TABLE public.company_skills (
    id text NOT NULL PRIMARY KEY,
    company_id text NOT NULL REFERENCES public.companies(id) ON DELETE CASCADE,
    name text NOT NULL,
    description text NOT NULL,
    files jsonb NOT NULL,
    bundle_hash text NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT company_skills_company_name_unique UNIQUE (company_id, name)
);
CREATE INDEX idx_company_skills_company ON public.company_skills (company_id);
