-- 0009 — #354 概念整合刀 1:workspaces 数据并入 projects(ADR 0008)。
--
-- workspace 行转项目行时**沿用原 id** —— 挂载锚 team/<id>、board_card/
-- document 关联、card_deliveries 全部零迁移。表与列的改名/DROP 留给
-- 刀 2(workspaces 死表本轮保留为回滚窗,读取路径全部切走)。
--
-- 数据语义(ADR 0008 §5/§6/§8):
--   * projects 升格:folder_path(存量无盘项目 = NULL,运行时惰性补盘)
--     + is_default(默认区转正,每队至多一个);
--   * 归档概念退役:存量 archived → active(status/archived_at 列与
--     归档端点活体至刀 2,值归一后不再产生新归档态以外的值);
--   * unbound 存量行不迁移不复活(随死表留至刀 2 DROP);
--   * project-kind 关联清退(语义被"挂靠会话成员推导"覆盖);
--   * 交付台账随卡片存活:引用 nullable 化 + FK 改指 projects 且
--     ON DELETE SET NULL(0007"解绑不删行、历史交付可追溯"的延续)。

-- 1) projects 升格列与唯一性(folder 唯一 = 一文件夹至多一项目;
--    PG 默认 NULLS DISTINCT,多个 NULL[存量无盘]互不冲突)
ALTER TABLE public.projects
    ADD COLUMN folder_path text,
    ADD COLUMN is_default boolean NOT NULL DEFAULT false;

CREATE UNIQUE INDEX idx_projects_folder_unique
    ON public.projects (folder_path);

CREATE UNIQUE INDEX idx_projects_default_one_per_company
    ON public.projects (company_id) WHERE is_default;

-- 2) 归档退役:存量归一(此后值恒 'active'/NULL,直至刀 2 删列)
UPDATE public.projects
   SET status = 'active', archived_at = NULL
 WHERE status <> 'active' OR archived_at IS NOT NULL;

-- 3) workspace 行转项目行(id 沿用;folder/成员/关联值零变)
INSERT INTO public.projects (id, company_id, name, description, folder_path,
                             is_default, created_at)
SELECT w.id, w.company_id, w.name, '', w.folder_path, w.is_default, w.created_at
  FROM public.workspaces w
 WHERE w.unbound_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM public.projects p WHERE p.id = w.id);

-- 4) project-kind 关联清退(board_card/document 保留)
DELETE FROM public.workspace_associations WHERE target_kind = 'project';

-- 5) 成员/关联表 FK 重挂 projects(原指 workspaces;数据已在 3) 迁入,
--    行值不变;ON DELETE CASCADE 语义原样保留 —— 删项目清账)
ALTER TABLE public.workspace_members
    DROP CONSTRAINT workspace_members_workspace_id_fkey;
ALTER TABLE public.workspace_members
    ADD CONSTRAINT workspace_members_project_fk FOREIGN KEY (workspace_id)
        REFERENCES public.projects (id) ON DELETE CASCADE;

ALTER TABLE public.workspace_associations
    DROP CONSTRAINT workspace_associations_workspace_id_fkey;
ALTER TABLE public.workspace_associations
    ADD CONSTRAINT workspace_associations_project_fk FOREIGN KEY (workspace_id)
        REFERENCES public.projects (id) ON DELETE CASCADE;

-- 6) 交付台账:引用 nullable 化 + FK 改指 projects(删项目 → SET NULL)
ALTER TABLE public.card_deliveries ALTER COLUMN workspace_id DROP NOT NULL;

ALTER TABLE public.card_deliveries
    DROP CONSTRAINT card_deliveries_workspace_fk;

ALTER TABLE public.card_deliveries
    ADD CONSTRAINT card_deliveries_project_fk FOREIGN KEY (workspace_id)
        REFERENCES public.projects (id) ON DELETE SET NULL;
