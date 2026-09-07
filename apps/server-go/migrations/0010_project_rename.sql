-- 0010 — #355 概念整合刀 2:名字清理(ADR 0008 §7 全线改名不留别名)。
--
-- 0009 把数据与读取路径切到 projects 后,workspaces 成死表(回滚窗);
-- 本轮收尾:死表 DROP,成员/关联/台账的表与列名对齐"项目"概念。
-- 约束/索引名中残留的 workspace_* 前缀(PostgreSQL RENAME 不跟表名)为
-- 无害的历史命名,不逐个改名。

-- 1) 死表退役(内含 unbound 存量死行,读取路径自 0009 起已零引用)
DROP TABLE public.workspaces;

-- 2) 成员表:workspace_members → project_members(列 workspace_id → project_id)
ALTER TABLE public.workspace_members RENAME TO project_members;
ALTER TABLE public.project_members RENAME COLUMN workspace_id TO project_id;
ALTER TABLE public.project_members RENAME CONSTRAINT workspace_members_project_fk TO project_members_project_fk;
ALTER TABLE public.project_members RENAME CONSTRAINT workspace_members_pkey TO project_members_pkey;

-- 3) 关联表:workspace_associations → project_associations
ALTER TABLE public.workspace_associations RENAME TO project_associations;
ALTER TABLE public.project_associations RENAME COLUMN workspace_id TO project_id;
ALTER TABLE public.project_associations RENAME CONSTRAINT workspace_associations_project_fk TO project_associations_project_fk;
ALTER TABLE public.project_associations RENAME CONSTRAINT workspace_associations_pkey TO project_associations_pkey;
-- kind 收紧:project 值已随概念合并退役(#354 清退存量,应用层白名单已挡)
ALTER TABLE public.project_associations DROP CONSTRAINT workspace_associations_kind_check;
ALTER TABLE public.project_associations
    ADD CONSTRAINT project_associations_kind_check CHECK (target_kind IN ('board_card', 'document'));

-- 4) 交付台账列对齐:card_deliveries.workspace_id → project_id
ALTER TABLE public.card_deliveries RENAME COLUMN workspace_id TO project_id;
