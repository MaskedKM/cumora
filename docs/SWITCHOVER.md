# 切换日 runbook:全量切 Go 与回切(#69)

> **#70 退役更新(2026-08-28)**:TS 回切床已拆除(见文末「退役后」)。
> 以下涉及 cumora-ts 的段落保留为切换日历史记录;现行回退=回退到
> 上一 commit 的 Go 二进制重建重启,数据不动。

> 形态(与用户确认):**直切 systemd**。Go 三件套直立,TS 留 stopped
> 回切单元,sidecar 两栈共用。生产为单机自托管 dev 形态
> (`NODE_ENV=development`、前端 :5180 走 Vite、RESEND 空=邮件静默、
> GitHub OAuth)。切换保持同形态,不引入新变量。

## 拓扑

```
systemd --user
├─ cumora-go.service      Go HTTP @ 127.0.0.1:5181(cumora-server)
├─ cumora-sidecar.service yjs-sidecar @ 5182(node/TS,共用)
├─ cumora-daemon.service  byoa-daemon(Go,agent computer --server :5181)
└─ cumora-ts.service      [stopped] ← 回切兜底(tsx server/src/index.ts)
```

## 前置(全部满足才可切)

1. **谱系**:#68(双跑 harness)、#109(OAuth 登录流)已合入 my-custom;
   本机 checkout 在 my-custom 且干净。
2. **构建二进制**(无本地 Go 工具链,走 docker 同 CI 镜像):
   ```bash
   cd apps/server-go && ./godocker.sh build -o cumora-server ./cmd/server
   cd ../byoa-daemon && ./godocker.sh build -o cumora ./cmd/cumora
   ```
3. **schema 补齐**:TS 迁移是 ensureSchema 幂等 DDL(advisory-lock、
   可重入),对生产库只前进不回滚:
   ```bash
   npm run migrate   # DATABASE_URL 指生产 cumora 库
   ```
4. **sidecar token**:.env 追加 `YJS_SIDECAR_TOKEN=<openssl rand -base64 24>`
   (两栈同源读取;TS 此前无 token 也可用,Go 未配 token 会告警)。
5. **安装单元**:`bash scripts/deploy/install-units.sh`(只装不启)。

## 切换步骤(逐条核验再前进)

```bash
systemctl --user start cumora-sidecar.service   # curl :5182 → 401 即活
systemctl --user start cumora-go.service        # curl :5181/api/livez → 200
systemctl --user start cumora-daemon.service    # journalctl 观察 pair/心跳
```

> 端口互斥:起 cumora-ts 前必须 stop cumora-go(反之亦然)。

## 观察清单(核心路径,全绿才算切换成立)

| # | 路径 | 验证 |
|---|------|------|
| 1 | 存活 | `GET :5181/api/livez` 200;`systemctl --user status` 三单元 active |
| 2 | 会话延续 | 浏览器旧 cookie 调 `GET /api/auth/me` 返回原用户(sessions 行同构) |
| 3 | 新登录 | :5180 前端走 GitHub OAuth(start→callback→#token 落地) |
| 4 | 消息 | 发一条群消息,双端可见(WS/SSE),`cumora:msg.new` 有发布 |
| 5 | agent 唤醒 | @agent 发消息 → daemon 领取 run → 回帖(wake-stream 链路) |
| 6 | 文档协同 | 打开一篇文档编辑,yjs-sidecar 房间总线活跃、双方同步 |
| 7 | 上传 | 本地模式传一张图,`/uploads/…` URL 可取回 |
| 8 | 邮件静默 | RESEND 空置确认;邮件任务组日志无 error |
| 9 | 认知辅 | scheduler/idle/rollup 无异常日志;agenda 卡片可领 |
| 10 | 日志纪律 | `journalctl --user -u cumora-go -f` 十分钟无 ERROR 洪水 |

## 回切演练(验收项,切换当日做一次)

```bash
systemctl --user stop cumora-go.service
systemctl --user start cumora-ts.service     # livez + auth/me + 发一条消息
systemctl --user stop cumora-ts.service
systemctl --user start cumora-go.service     # 回到 Go,复跑观察清单 1–4
```

真实回切同此(数据不动,Postgres/Redis 无需操作);两栈任意时刻
起其一即可恢复服务。

## 事故处置(切换日历史——cumora-ts 已随 #70 卸载,现行回退见文末「退役后」)

- Go 崩溃循环:`systemctl --user stop cumora-go && systemctl --user start cumora-ts`,留 journalctl 取证后处理。
- daemon 不领卡:查 `journalctl --user -u cumora-daemon`、`REDIS PUBSUB CHANNELS`(cumora:wake:* 订阅在),必要时回切 daemon 亦可(TS agent-cli 仍在)。

## 切换后

- 旧 TS daemon 单元(`cumora.service`,agent-cli)保持 **stopped** 且不
  enable——与新 Go daemon 单元并存但绝不同启(双 daemon 会双领 run);
  #70 退役时一并删除。
- 观察期(默认 ≥48h 或用户点头)→ #70 TS 退役门禁;退役前 TS 单元
  与 agent-cli 保留原样,不做删除。
- 挂起清尾(#68 遗留 F6/F7/F11/F16/F17 + #109 延后项)在观察期窗口内
  按 #90 先例轻量清尾。

## 退役后(#70,2026-08-28)

用户提前点头跳过观察期,TS 全套退役:

- **仓库**:server/src 运行时删除(保留 `__integration__` MIRROR-only
  套件 + harness 四件:db/pool、redis、env(裁)、agents/runtime/jwt、
  email 种子切片);`agent-cli/`、`bin/cumora`、`scripts/dual-backend/`
  删除;CI tests job 换 golang:1.24-bookworm + setup-node(runner 自建
  Go 服当 SUT);契约守卫提取腿换 Go `HandleFunc`(#117 豁免表)。
- **生产机**:卸载 `cumora-ts.service`;旧 `cumora.service`(TS daemon,
  agent-cli)disable + 删;daemon.env 保留(byoa-daemon 仍用)。
- **现行回退**:checkout 上一 commit → `godocker.sh build` 重建二进制 →
  `systemctl --user restart cumora-go`。Postgres/Redis 数据不动。
  TS 栈如需考古,git 历史完整留档(本 runbook 的切换日段落即其用法)。
- **已知缺口**:#117(missed-routes:polls/og/apple-native/autonomy/
  shipping/admin 子面)——切换日起即 404 的面,与退役无关,逐票补齐。
