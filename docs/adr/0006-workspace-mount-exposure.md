# 0006 — Workspace folder direct exposure via bind mount

## Status

Accepted (2026-09-03)

## Context

CONTEXT.md 定义 Workspace 为"绑定一个真实文件夹的团队协作面",但现状实现里
agent 的引擎进程根本看不到那个文件夹:唯一通道是 `cumora workspace
ls/read/write` 三个 CLI 子命令逐文件读写 JSON 字符串(纯文本、单文件 2MB);
persona 提示词承诺的 "run builds, tests, and git" 要求 operator 口头告知
folder path,产品层零支撑。多 agent 并发写是裸覆盖(last-write-wins),变更
零通知(UI 手动 reload),建区/成员/关联/解绑四个 mutation API 前端零调用。
换言之,功能面实为"带成员权限的树状文本存储服务"。

行业调研(2026-09-03):共享文件工作区的变更感知分三派——①写必经服务端
(Figma/Notion/yjs 文档面,广播是服务端副产品);②本地 watcher(Syncthing:
inotify 近实时 + fs watcher delay 去抖 + 全量扫描兜底,watcher 启用时 rescan
放宽到 60min 以防丢事件与 inotify watch 上限;Dropbox 同构);③隔离副本
(Devin/Manus/OpenHands/Claude Code worktrees,感知问题转化为合并问题)。
"直写共享盘却无变更感知"零先例——选直写,watcher 即标配。冲突哲学上,
Syncthing/Dropbox 一致:**永不丢数据,分叉保双份**(`.sync-conflict-*` /
conflicted copy 带编辑者+时间戳),不自动合并。

本 ADR 是 2026-09-03 Workspace grilling 九题共识的决策记录(逐题决策见各
战役票面);#265(工作交付面平台化)同场拍板并入本战役。

## Decision

1. **同机 bind 挂载为主通道**:computer=local 的 agent,其 home 下固定挂点
   `~/team/<wsId>/` bind-mount 对应 workspace folder,读写直落盘,native
   工具链(git/build/test/ripgrep)全解锁。单盒立场(ADR 0005,server 与
   daemon 恒同机)使同机是常态而非巧合。挂点布局为 #265 留位:母仓即挂载的
   workspace repo,任务级 worktree 另行挂载。
2. **CLI 为回退与审计路径**:vps computer 无挂载,走完整 CLI 面(补齐
   delete/mv/stat/edit/append/grep 子命令);persona 按 computer 类型生成两版
   (local=挂载版,vps=CLI 版),现 "NOT the team workspace" 措辞翻新。
3. **感知**:daemon 侧 inotify/watch 监听挂载点,≈2s 去抖批量上报 server →
   mtime 索引落地 + WS 事件 `workspace.files_changed`(区级粒度,带变更
   文件清单、不含内容);另保留 60min 级全量扫描兜底(watcher 丢事件/
   inotify 上限时接管)。
4. **防护三层,边界诚实**:
   - API/CLI 写路径:CAS(expected mtime,失配返回 412 + 最新内容,让 agent
     重读重判——与消息面 HELD 同构);
   - 写前快照:旧版进文件夹内 `.cumora/versions/`(留最近 10 版,同生命
     周期,不进 DB,server/daemon 专用);
   - 挂载直写路径:CAS 管不到,冲突检测后按业界哲学保双份(conflicted
     copy,带编辑者+时间戳后缀)。
   防护覆盖面在文档与票面明写:**CAS 不覆盖挂载直写**。
5. **传输**:multipart 正式上传面(契约生成器/客户端/CLI 全动);二进制
   单文件 25MB,文本维持 2MB。挂载落地后 agent 侧二进制自然走文件夹,API
   通道主要给人用。
6. **#265 并入本战役**(推翻票内缓行结论):三阶段不变(worktree-per-task
   → 失败保分支 → 卡片↔分支↔PR 关联门禁),母仓地基由本战役刀 1 就位。
7. **实施切分**:依赖纵切四刀,每刀一 PR 关一票、串行纪律——刀 1 能看能干
   (挂载+CLI 补齐+persona 双版)→ 刀 2 敢用(防护三件套+watcher+事件)→
   刀 3 好用(mutation UI 双向入口+multipart+文件名过滤)→ 刀 4 交付面
   (#265 三阶段)。术语(Mountpoint/Conflicted copy/Versions)已入
   CONTEXT.md,随刀 1 合入。

## Consequences

- **挂载即信任域**:直写绕过 server 的 member-scope 校权、写防护与审计。
  这与现状"scope 内全读写、无路径级 ACL"的权限模型一致,没有损失粒度,但
  意味着成员移除/解绑必须由 daemon 主动收回挂载(在飞进程持有文件句柄的
  收尾语义是实现期课题);`.cumora/` 内部目录 agent 侧不得写入。
- watcher 有丢事件与 inotify watch 上限的现实,扫描兜底不可裁剪。
- 影子目录与文件夹同生命周期:解绑自然带走、不占 DB;代价是不.unbind 不
  可见(与"folder belongs to at most one workspace"一致)。
- vps computer 能力面永久=API-only(无 git/build),persona 明示,不假装
  等价。
- 文件面防护与消息面协调哲学(claim/HELD/fresh)形成同构而非同一:文件
  面的"重读重判"由 412 信封承载,不新增跨面状态机。
- 失败语义链(#259/#262/#260)不为本战役前置;挂载引入的新失败面(watcher
  死/挂载丢失)接入既有 watchdog 体系即可。

Rejected alternatives:只读挂载+写走 API(防护完整但 build/test 写不了盘,
承诺只兑现读半边);daemon 双向同步(Syncthing 式镜像,同步延迟+双向冲突
面+双模式维护,工程量最大且与挂载收益重叠);纯 API 补强(零风险但
Workspace 永远是文本存储服务,persona 承诺永久落空);文件级 claim 锁(与
已废弃的泛化 claim 哲学相悖,锁残留是经典坑,且不防"读旧版后强写")。
