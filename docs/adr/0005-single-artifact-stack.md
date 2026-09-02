# 0005 — Single-artifact desktop distribution with the Cumora Stack

## Status

Accepted (2026-09-01)

## Context

ADR 0003 定下"单盒自托管"的部署目标,但今天的获得方式是三件套:三个手写的
systemd user unit(`cumora-go` / `cumora-daemon` / `cumora-sidecar`)+ 手动
`deploy-release.sh` 发版 + 桌面 AppImage 单独分发。装配知识散落在
`install-units.sh`、unit 文件的大段注释和运维 runbook 里;桌面端对后端状态完全
无感知(2026-08-31 三件套被手动停摆、仅 sidecar 存活的事故即此形态);sidecar
仍以 TS 源码形态从工作树运行,不是可分发单元。用户诉求:一个 AppImage 包办。

行业调研(2026-09-01,同类 = 桌面 UI + 本地常驻服务 + 数据库 + 后台 agent):
该类产品的判别问题是"后台工作是否必须在 UI 关闭后继续"。cumora 属于"必须"
阵营(agent 单轮推理数分钟、日历提醒、看板 triage、定时任务)。该阵营的全部
成熟产品(Syncthing、Ollama、Docker Desktop、Tailscale)采用同一形态——
**服务优先**:一个制品包办"获得",OS supervisor 包办"常驻",UI 是客户端 +
控制台;无一例外把后端留在 GUI 进程树里,Home Assistant 甚至弃用了应用层自管
生命周期的装法。"GUI 进程树托管全部栈"零先例。

## Decision

采纳服务优先形态,发行与运行分离:

1. **单一 AppImage 制品**内含:Electron 客户端 + 管理面 + bootstrap,以及整个
   Stack 的运行件(server/daemon 为 Go 二进制,sidecar 为 bun 编译的自包含
   二进制——见 Consequences 的路线修订;另含 pg16+pgvector 预编译二进制、
   redis-server)。一个制品 = 一套互相验证过的版本(契约天然对齐)。
2. **拓扑**:一个 ~10 行的 systemd user unit 只保证 `cumora-stackd` 存在
   (Restart=always + 开机入口);stackd 进程内拉起并守护
   pg→redis→sidecar→server→daemon,健康门、依赖链、重启退避全部是可单测的
   Go 代码。子进程日志以 `svc=<name>` 结构化行进 journal,`cumora-stack
   logs -f <svc>` 补偿查询面。watchdog 职责互补不重叠:stackd 管"进程死了
   拉起",server 侧 watchdog 管"活着但失联判死"。
3. **数据与状态**全部在制品外 `~/.local/share/cumora/`(pgdata、redis unix
   socket——本机 6379 已被占用、uploads、`releases/<ver>/` + `current`
   原子 symlink,#211 发版链语义原样继承)。AppImage 是纯发行载体,装后可删。
4. **配置**:`~/.config/cumora/stack.toml` 管机器事实(路径/socket/端口/引擎
   发现路径,schema 校验、设置页可见);凭据(token/key)留 env 文件。首启
   向导一次性导入 `.env`/`daemon.env`:机器事实转 toml,GITHUB OAuth 缺失为
   红线,R2_* 标可选(本部署未启用)。
5. **存量数据**:`stack migrate-pg` 一次性迁入内置 pg(本机系统 pg 与内置同
   为 16 major,dump/restore 无版本坎):幂等、迁移前自动备份、旧库不动。
   > 勘误(2026-09-02,#316 甄别):所称"系统 pg"实为 docker 容器
   > `cumora-pg`(host 网络 5432)—— 本机从无 systemd 系统 postgres。
   > 迁移语义不受影响,退役路径见 DEPLOY-STACK.md(docker 实态)。
6. **登录链不动**:桌面连本机栈仍走完整 OAuth(系统浏览器 → 服务端回跳 →
   47823 回环/深链,#271/#272 修稳的链路),不新增本地信任捷径。
7. **升级**:桌面更新下载后,设置页手动确认按钮触发栈升级——停链 → 迁移 →
   原子切 symlink → 滚动重启;保留最近 3 份 releases 供一键回滚。桌面更新
   永不静默触发栈迁移。
8. **退场与卸载**:bootstrap 检测到旧三 unit 即停用禁用(不删文件);
   `stack uninstall` 保留数据,`--purge` 才删。
9. **术语**:受管本地栈的 canonical 术语为 **Stack**(见 CONTEXT.md glossary;
   二进制名 `cumora-stack` / 守护 `cumora-stackd`)。

## Consequences

- 前置:yjs-sidecar 必须先成为可分发单元(阶段 0,独立票 #280)。**路线拍板
  (2026-09-01):保留 TS CRDT 引擎,用 bun `build --compile` 产出自包含单二进制,
  不做 Go/yrs 重写**——勘察实锤 sidecar 是有状态的完整 Yjs 引擎
  (`mergeUpdates`/`applyUpdate`/`encodeStateAsUpdate` + Y.Xml 结构手术),Go 移植
  需与 yjs 13.x 线格式位级兼容否则存量快照重放即发散;而生态主流(Hocuspocus,
  Tiptap 官方)正是 TS 引擎且官方支持 Bun 运行时,y-sweet(Rust/yrs)是支线。
  本 ADR"单制品内全部运行件为纯 Go 二进制"的表述相应放宽为"自包含可分发二
  进制";bun 兼容性验证并入 #280 落地 PR 的验收(编译产物在 CI 跑现有
  rooms/http 测试)。
- 桌面发版节奏从此门控栈发版(每次制品升级含 schema 迁移)——以"手动确认 +
  可回滚"承接;换来契约永不漂移。
- pg 大版本升级永远手动(dump/restore),与所有内嵌数据库应用同例。
- journalctl 不再按服务分 unit;以 svc 标签结构化日志 + `stack logs` 补偿。
- 受众定位:自用优先(本机 Ubuntu 24.04 x64 定死目标),但实现不写死本机
  路径/凭据;将来分发补兼容矩阵而非重构。mac/win 打包不在本期范围。
- 实施分四个阶段开票(0 骨架与前置 / 1 守护引擎 / 2 打包与向导 / 3 桌面管理
  面),开工时机另议(本 ADR 只定方向与拆票)。

Rejected alternatives:瘦 AppImage + 首启联网拉制品(GitHub 出口弱、双通道契约
漂移);自带 supervisor 无 systemd(顶层无人看管、非图形会话不跑、HA 前车之
鉴);Docker/Podman compose 栈(引入常驻容器运行时,违背单制品);sudo 安装
向导(破免 root);进程内一体(关窗腰斩 agent,watchdog 误判,一票否决)。
