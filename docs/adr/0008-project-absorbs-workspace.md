# 0008 — Project absorbs Workspace (one container per body of work)

## Status

Accepted (2026-09-04). Implemented in the planned three cuts: #354
(data re-tracking + projects domain, PR #361) → #355 (contract / CLI /
daemon rename + retired endpoints, PR #365) → #356 (UI promotion,
settings-tab retirement, glossary finalization — CONTEXT.md Project entry
live, Workspace entry retired).

## Context

产品里长期并存两个"容器"概念:

- **Project(项目)**:`projects` 表(name/description/color/status/archived_at),产品面是
  设置页管理 + 聊天列表过滤 chip + 建群可选挂靠;无成员表(公司全员可见),
  改挂端点(`POST /conversations/{id}/project`)无 UI 调用,归档无任何级联。
  CONTEXT.md **没有它的词条**——领域模型层面它从未被正式定义。
- **Workspace(工作区)**:绑定真实文件夹的协作面,ADR 0006 四刀刚落地全部
  机制(挂载/防护/感知/交付面),是功能重心;显式成员 + 三向关联推导
  (project/board_card/document)。

两者的实质耦合只有一处:project 是 workspace 的成员推导源之一
(`workspace_associations.target_kind='project'`,项目下任一会话的成员 →
工作区成员)。换言之,**项目已经是工作区的从属挂靠点**——中间这层实体
的存在意义存疑。用户 2026-09-04 提出:"项目和工作区这两个概念应该是
同一个东西。"

现状裂缝(合并的旁证):工作区"隐式成员"推导用 legacy `members` jsonb、
访问检查用 `conversation_members` 表(两处分叉);归档项目仍经关联授予
盘访问;工作区详情页关联项目要手填裸 target id。

行业对照:ChatGPT Projects 无归档、只有删除(连删 chats);Claude Projects
归档(可逆)+删除(永久);coding agent 类(含 zcode)项目=工作目录、
无生命周期状态。两家的项目都无"盘"——cumora 的项目绑真实文件夹,删除的
级联面更重,须明确定义。

本 ADR 是 2026-09-04 概念整合 grilling 八题共识的决策记录。

## Decision

1. **Project 吸收 Workspace**(方向 B):项目成为"一摊工作"的唯一容器
   ——对话挂靠 + 文件夹 + 看板/文档关联 + 交付台账都在项目名下。
   Workspace 实体退役。选择 B 而非反向(A)的理由:会话挂靠
   (`conversations.project_id`)、agent 记忆按项目过滤链、shipping 的
   project 链**零迁移**;需要重构的只有 workspace 家族(表/关联/域/UI)。
2. **命名**:对外(UI/文档/词汇表)统一"项目 / Project";Workspace 一词
   从产品面退役(仅存于历史 ADR,ADR 0002 的"workspace owns the word"
   由本 ADR 交棒)。代码与 CLI 同步改名,**不留别名**——单一概念单一名字。
3. **文件夹强制必绑**:每项目必有盘。新建项目默认在受管目录自动建空盘
   (零额外输入),高级选项可自填已有路径(如代码 repo);存量无盘项目
   由 migration 自动补盘。"一文件夹至多一项目"唯一性保持。
4. **默认区转正为特殊项目**:每队一个 is_default 项目("团队文件"),
   置顶公共盘、全员可见、不可删除;agent 侧默认区消费链(CLI 面 cliEnsureDefaultWorkspace、
   挂载清单 EnsureDefault 两处)改锚到它。
5. **成员模型**:is_default 项目全员;普通项目 = 显式成员 ∪ 挂靠会话成员
   推导(群挂项目 → 群成员可见项目与盘;推导统一走 `conversation_members`,
   顺手消灭 legacy jsonb 分叉)。board_card/document 两类关联**保留**
   (看板/文档无项目外键,卡片 assignee、文档协作者经关联获得盘访问——
   刀 #338/#265 的地基);project-kind 关联退役(项目自带盘,语义被
   会话直推导覆盖,存量行 migration 清退;曾经 project-kind 关联成对的
   W[持盘]与 P 各自并列成项目、不自动合并——有意选择,要合并是人的
   动作)。
6. **生命周期:无终点状态,仅删除**(ChatGPT 形态)。归档概念退役
   (存量 archived 项目 migration 转 active),解绑(unbind)退役
   (强制盘下"无盘项目"是不允许态)。删除的级联:对话 SET NULL 保留
   (团队资产,外键已如此设计)、**盘文件原地保留**(平台不代删真实文件,
   受管目录提示可手动清理——分支/PR 的追溯根仍在盘与 git 里)、关联行由
   CASCADE 承担、**交付台账随卡片存活**(卡片无项目外键本就会活,
   card_deliveries 的项目引用置 NULL——沿用 0007 迁移"解绑不删行、历史
   交付可追溯"的意图,台账哲学是记录永不静默丢)、is_default 项目不可删。换盘(rebind)首版不做,记余量。
7. **改名范围**:人侧——文件视图升格"项目"视图(换数据源、管理并入)、
   设置页项目标签退役、聊天 chip/建群选项目保留;agent 侧——CLI
   `cumora workspace <九动词>` 改 `cumora project <动词>`,persona/
   SKILL/help 文案同步。挂载路径 `team/<id>` **不变**:migration 把
   workspace 行转项目行时**沿用原 id**,挂载锚、card_deliveries、
   关联表零迁移。
8. **实施三刀纵切**(一 PR 一票,串行):刀 1 模型并表+域重构
   (workspaces 路由族形状不变、底下换查 projects;并表后两族 API 自然
   返回同一集合——原项目列表混入原工作区是预期行为,刀 2 收敛为单族;
   数据迁移含 archived→active、无盘项目置 NULL 惰性补盘、project-kind
   关联清退、存量 unbound 行不迁移不复活[随死表留至刀 2 DROP])→
   刀 2 契约/CLI/daemon 改名+退役动作(归档端点、解绑端点、attachProject
   死端点的删除,表名/包名清理,前端调用点同步,UI 形状不变)→
   刀 3 UI 升格+文档(CONTEXT.md 词条改写随此刀)。

实现层自决细节(随刀 1 落地):并表后 `card_deliveries.workspace_id` 等
列名的去留以改动面最小为准;自动补盘位置 = 受管目录 `projects/<id>`。

## Consequences

- **概念减一,词让一处**:用户心智里"一摊工作"只有一个名字;代价是
  全线改名的机械量(契约路由族、CLI、前端调用点、文案、词条)。
- **成员域跟着盘走**:挂靠即授权(群成员可见盘)——与 ADR 0006
  "挂载即信任域"同构;显式成员补足"不在群里但要访问盘"的场景。
- **删除是不可逆操作但数据面克制**:对话保留、盘文件不代删——平台
  只清自己的账(关联/台账),不动用户资产。
- **与 HR 战役(#344+)并行不冲突**:HR 的岗位层不挂项目;两战役靠
  "开工前 gh pr list" 串行纪律防撞。
- 既有裂缝(推导分叉/归档不级联/死入口/手填 id)随合并自然消亡,
  不再单独修。
- ADR 0006 的机制(挂载/防护/感知/交付面)语义不变,宿主改名;
  "解绑须收回挂载"的触发点随删除语义走(daemon 每轮挂载清单同步天然
  覆盖)。删除项目后 agent_memory 的 project-scope 行成为不可见孤儿
  (不删,记忆面余量)。

Rejected alternatives:**A(Workspace 吸收 Project)**——功能面该活,但
会话挂靠/记忆/shipping 三条项目链全要迁移,改造量反超;**C(保留双概念
只修裂缝)**——治标不治本,用户的不适来自概念重复本身;**文件夹可选
(两态项目)**——轻了一档但概念分裂成两态;**归档+删除两轴(Claude
形态)**——多一个状态轴,且与"项目无终点"的业界形态不符。
