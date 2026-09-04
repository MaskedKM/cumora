# 0007 — HR Agent: off-roster invisible personnel officer

## Status

Accepted (2026-09-04)

## Context

现状盘点(2026-09-04 grilling):agent 侧已有一套迷你人事核心——hire(CreateAgent
过 owner/admin 闸+套餐人数闸、自动入 #all-hands、生成 IDENTITY/SOUL 档案)→ 档案
编辑 → 自治授权(autonomy/战绩)→ offboard(软删保档)→ rehire——但每个环节
都由 owner 手动驱动;无绩效评估,无 prompt 优化闭环,agent 表现差只有"人肉看
转录然后手改 prompt"一条路。用户要的不是给人用的人事管理页面,而是一个**会干活
的人事职能**:招人、职责拆分、绩效评估(人员淘汰、prompt 优化)——即 AI 公司
的人事工作本身也由 agent 承担。

本 ADR 是 2026-09-04 HR grilling 十二题共识的决策记录(逐题决策随 spec 票面落)。

## Decision

1. **形态:专职 HR Agent,编外隐形实体**。不在花名册、对其他 agent 完全不可见
   不可交互不可召唤、无约谈环节;每公司恰好一个、随公司常驻、不可 offboard;
   其自身 prompt 仅 owner 可改。破"每个 agent 都是花名册一行 participants"的
   现有不变量;不占套餐 headcount 名额(free=10/pro=20/max=50 不计);执行配置
   与普通 agent 同构(owner 指派 Computer+Engine,消耗入 observability)。
2. **交互面:独立 HR 页**,不碰聊天面与人侧 Inbox——报告归档、提案队列、
   prompt 变更历史都在该页;手动触发(立即评估某 agent/全员)也在该页。
3. **触发:周期例行 + 手动 + 事件钩子**(看板逾期/spend 超阈/错误率异常自动
   加评;具体阈值 spec 定)。
4. **权力闸分级**:岗位层修改(system_prompt/bio/头衔)自主执行,HR 页留变更
   历史可一键回滚;**招人与淘汰必须 owner 批准**——淘汰复用现有 offboardAgent
   (软删、可 rehire),不是硬删;headcount 由 HR Agent 建议、owner 拍板。
5. **作用域边界:只动岗位层**。Private Area(IDENTITY.md/SOUL.md/memory)保持
   词表语义绝对私有,不可见不可改——领域故事:HR 管岗位,agent 拥有自我。
6. **绩效输入全谱四路**:客观观测(runs/spend/triage 经济学/看板交付/calendar
   履约)+ 全部聊天转录(含 agent 间私聊)+ 同侪信号(agent_climate 的
   affinity/trust)+ owner 主观评分;配方与权重 spec 定。
7. **职责拆分停在 prompt 层**:头衔字段 + system_prompt 分工描述即全部,不建
   部门/汇报线/职级实体,零新表。

## Consequences

- agents 域权限模型必须扩出"agent 代行 owner 人事权"通道:今日 CreateAgent/
  UpdateAgent/OffboardAgent 全部 requireRole 只认人类 owner/admin。
- 花名册不变量破口需要系统性防泄漏:getParticipants 不含它之外,agent 侧一切
  枚举面(成员选择器、@ 补全、Whisper、拉群)都不得暴露它的存在。
- 旁路观测面=owner 单线审计权的延伸而非新权限模型:它可读全部会话转录,但
  永不进入任何会话。
- 无约谈 ⇒ 绩效沟通零面:被评估者对评估全程无感知,prompt 变更静默生效,
  变更历史仅向 owner 开放。
- human 成员生命周期缺口(移除/改角色)是独立运营小票,不归 HR Agent 职能。

Rejected alternatives:花名册内 HR 成员+授权(进入社交面、可被召唤,被评估者
可影响评估者,治理倒挂);server 定时例程无人格(四职能是分析+起草+判断的
活,需要 Brain 级推理,不是 cron 能产的报告);人侧向导/工具页(退化成手动
运维,"人事"本身不干活,与需求原意相悖)。
