# 切换日 runbook:全量切 Go 与回切(#69)

> **#70 退役更新(2026-08-28)**:TS 回切床已拆除(见文末「退役后」)。
> 以下涉及 cumora-ts 的段落保留为切换日历史记录;现行回退=回退到
> 上一 commit 的 Go 二进制重建重启,数据不动。

> 形态(与用户确认):**直切 systemd**。Go 三件套直立,TS 留 stopped
> 回切单元,sidecar 两栈共用。生产为单机自托管 dev 形态
> (`NODE_ENV=development`、前端 :5180 走 Vite、RESEND 空=邮件静默、
> GitHub OAuth)。切换保持同形态,不引入新变量。

> **#211 部署收口更新(2026-08-31)**:二进制面(server/daemon)不再吃
> 仓库工作树手工构建,改走 go-release.yml 的 release 制品;三件套全
> enable;livez 扩 Redis 硬依赖。现行发版/回退见下方「现行部署流」;
> 本节以下保留切换日历史记录。

## 现行部署流(#211 起:release 制品衔接 systemd)

8-31 晨三件套全停的结构根因落纸于 #211(复盘见下节):systemd 直接
执行工作树手工构建产物(生产二进制落后 HEAD 96+ 提交)、三件套只
enable 一个、无健康联动。#211 起部署物与启动语义如下。

### 目录组织

```
~/.local/share/cumora/
├── releases/<vX.Y.Z>/            # deploy-release.sh 落盘的制品:
│   ├── cumora-server             #   与 cumora-daemon 同包(sha256 校验过)
│   ├── cumora-daemon
│   ├── migrations/               #   #211 起随制品走,二进制与 schema 同版本
│   └── VERSION                   #   内容=tag,版本核验用
├── current -> releases/<vX.Y.Z>  # systemd ExecStart 经此寻址(原子切换)
└── uploads/                      # #208/#248 生产上传数据,与部署物解耦
```

工作树剩余职责:sidecar(node/TS 源码)、webapp dist/、`.env`、
`daemon.env` —— 这些不在 Go 制品内。

### 发版步骤(下载制品 → 校验 → 落版本目录 → 切 symlink → restart)

```bash
# 1) 合入 my-custom 后打 tag(制品只从受保护谱系出,go-release.yml 有门)
git tag vX.Y.Z && git push origin vX.Y.Z

# 2) 等 go-release.yml 出制品(Actions 页;产物=cumora-<os>-<arch>.tar.gz
#    + SHA256SUMS,内含 server/daemon/migrations)

# 3) 部署(下载→sha256 校验→releases/<ver>/→原子切 current→重启三件套
#    →livez 核验;任一步失败清晰报错并保留旧 current)
bash scripts/deploy/deploy-release.sh vX.Y.Z     # 或 latest

# 4) 核验
systemctl --user status cumora-go                 # ExecStart 经 current 寻址
readlink ~/.local/share/cumora/current            # → releases/vX.Y.Z
cat ~/.local/share/cumora/current/VERSION         # → vX.Y.Z(与制品对得上)
curl -s localhost:5181/api/livez                  # 200
```

回退 = 同脚本指旧 tag(注意:脚本会重新下载并校验制品——离线/gh 不可达时
回退失败;旧版本目录虽仍在 releases/ 下,但脚本不复用本地副本,须联网):

```bash
bash scripts/deploy/deploy-release.sh v<旧版本>
```

### 启动/自愈语义(#211)

- 三件套全 enable(`install-units.sh`):**重启机器即自愈**,顺序由
  After=/Wants= 表达 —— sidecar(ExecStartPost 探 /internal/healthz,
  200/401 均算活)→ go(After sidecar,ExecStartPost 探 /api/livez,
  200/503 均算"进程就绪")→ daemon(After go)。
- go 探针的 503 算活是关键语义:Redis 红是依赖事实,重启修不了;livez
  保持可观测的红比打成 connection refused 循环更有用。
- **livez 扩 Redis 硬依赖**:Redis 不可达、或启动时降级 NoopPublisher
  → `/api/livez` 503(此前 Noop 只 Warn 一行、HTTP 面假绿)。降级判定
  是 boot 一次性的:Redis 中途恢复后 livez 转绿 ≠ 事件面恢复(Noop
  不自愈)—— 降级实例 livez 持续红并提示 restart cumora-go。
- 手工构建(`godocker.sh build`)仍可用,定位是开发/取证,不再是部署面。

## 8-31 事故复盘(#211)

晨间生产三件套全停且不自愈,结构性三病:

1. **部署物与发布流零衔接**:systemd 直接执行仓库工作树手工构建产物
   (`ExecStart=apps/server-go/cumora-server`),生产二进制 8-29 构建、
   落后 HEAD 96+ 提交;go-release.yml 的制品发了没人用、部署没走发布。
2. **三件套只 enable 一个**:`install-units.sh` 只 enable sidecar,
   go/daemon 故意留给 runbook 编排 —— 机器重启/手动 stop 后不能自愈,
   正是 8-31 形态。
3. **无健康联动**:`/api/livez` 与 sidecar `/internal/healthz` 存在但
   没人调;livez 不覆盖 Redis 硬依赖,NoopPublisher 降级只 Warn 后吞
   全部事件,HTTP 面看起来正常。

处置:release 制品衔接(部署流如上)+ 三件套全 enable(After= 链)+
ExecStartPost 探针 + livez 扩 Redis ping(Go 侧单测钉两态)。详见 #211。

## 拓扑

```
systemd --user
├─ cumora-go.service      Go HTTP @ 127.0.0.1:5181(cumora-server)
├─ cumora-sidecar.service yjs-sidecar @ 5182(node/TS,共用)
├─ cumora-daemon.service  byoa-daemon(Go,agent computer --server :5181)
└─ cumora-ts.service      [stopped,已随 #70 删除] ← 回切兜底(原 TS 入口 index.ts)
```

## 前置(全部满足才可切)

1. **谱系**:#68(双跑 harness)、#109(OAuth 登录流)已合入 my-custom;
   本机 checkout 在 my-custom 且干净。
2. **构建二进制**(#211 起仅为开发/取证路径,部署走 release 制品;
   无本地 Go 工具链,走 docker 同 CI 镜像):
   ```bash
   cd apps/server-go && ./godocker.sh build -o cumora-server ./cmd/server
   cd ../byoa-daemon && ./godocker.sh build -o cumora ./cmd/cumora
   ```
3. **schema 补齐**:Go 服启动时自动应用 goose 迁移(db.Migrate,advisory
   lock,可重入,只前进不回滚);#211 起 migrations 随制品落在
   `current/migrations`,启动即应用对应版本。需手工前置时用
   `cd apps/server-go && ./godocker.sh run ./cmd/migrate`(DATABASE_URL
   指生产 cumora 库)。旧 TS 的 `npm run migrate` 已随 #70 退役。
4. **sidecar token**:.env 追加 `YJS_SIDECAR_TOKEN=<openssl rand -base64 24>`
   (两栈同源读取;TS 此前无 token 也可用,Go 未配 token 会告警)。
5. **安装单元**:`bash scripts/deploy/install-units.sh`(#211 起三件套
   全 enable,重启机器自愈;仍不自动 start,首次拉起/发版走
   deploy-release.sh)。

## 切换步骤(切换日历史;现行发版/启动见「现行部署流」)

> #211 起:发版 = `bash scripts/deploy/deploy-release.sh <tag>`(内含
> 三件套 restart);首次手动拉起同下,但三件套已 enable,重启机器不再
> 需要人工 anything。

```bash
systemctl --user start cumora-sidecar.service   # curl :5182 → 401 即活
systemctl --user start cumora-go.service        # curl :5181/api/livez → 200
systemctl --user start cumora-daemon.service    # journalctl 观察 pair/心跳
```

> 端口互斥:起 cumora-ts 前必须 stop cumora-go(反之亦然)。

## #208 上传数据迁出工作树(存活期一次性,先于或随任意发版做)

背景:uploads 根此前默认 `server/uploads/`(gitignore 的工作树内,重新
clone / 换机即丢且无备份);且 `CUMORA_UPLOADS_DIR` 只被写侧认,读侧
(静态服务/OAuth 头像镜像)、email 域(入站附件/GC)与 workspaces 默认区
不认——设 env 会精神分裂。#208 起六处 + sidecar 统一走
`config.UploadsDir()`(`CUMORA_UPLOADS_DIR` > 旧键 `UPLOAD_DIR` >
cwd 相对 `server/uploads`),两个单元注入仓外路径。迁移步骤:

```bash
# 0) 前置:本 checkout 已含 #208,重装单元拿注入的
#    Environment=CUMORA_UPLOADS_DIR=%h/.local/share/cumora/uploads
bash scripts/deploy/install-units.sh

# 1) 建目标目录(与单元注入值一致)
mkdir -p ~/.local/share/cumora

# 2) 停写侧:go(上传/OAuth 头像/email 附件)与 sidecar(文档快照/
#    内联图片)两栈都写;daemon 不写 uploads,但三件套一并重启最稳
systemctl --user stop cumora-go.service cumora-sidecar.service

# 3) 存量数据整体搬家(mv 同盘原子;跨盘先 rsync -a 再删源)
mv ~/Code/cumora/server/uploads ~/.local/share/cumora/uploads

# 4) workspaces 存量行修路径:folder_path 落库时是绝对路径(以 cwd
#    钉死),搬家后必须同构改写,否则默认区文件列表指向旧位置:
psql "$DATABASE_URL" -c "
UPDATE workspaces
   SET folder_path = replace(folder_path,
        '$HOME/Code/cumora/server/uploads',
        '$HOME/.local/share/cumora/uploads')
 WHERE folder_path LIKE '$HOME/Code/cumora/server/uploads/%';"

# 4b) 改写核验:应返回 0(历史上若有非本机 cwd 的异构实例产生的行,
#     LIKE 模式会漏改,漏网行在文件列表处 400 且静默——核验兜住它;
#     返回非 0 时逐行人工改写)
psql "$DATABASE_URL" -tAc "
SELECT count(*) FROM workspaces
 WHERE folder_path LIKE '$HOME/Code/cumora/server/uploads/%';"

# 4c) email 附件例外:历史上若给 Go 设过 UPLOAD_DIR 指到别处,那份
#     email-attachments 不在上述 mv 覆盖内,需单独 mv 到新根(未设过
#     则忽略本条)
# 5) 重启三件套并核验
systemctl --user start cumora-sidecar.service cumora-go.service cumora-daemon.service
curl -s localhost:5181/api/livez   # 200
```

核验(观察清单 7 的加强版):传一张图 → `/uploads/…` URL 取回 200 →
`ls ~/.local/share/cumora/uploads/attachments/` 出现新文件,且
`~/Code/cumora/server/uploads` 不再存在/不新增。回退 = 反向 mv + 反向
UPDATE + 还原单元(数据本身与代码解耦,任一方向都只动文件系统与
workspaces 行)。

## 观察清单(核心路径,全绿才算切换成立)

| # | 路径 | 验证 |
|---|------|------|
| 1 | 存活 | `GET :5181/api/livez` 200(#211 起 503=Redis 红或降级 Noop,即事故信号);`systemctl --user status` 三单元 active |
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

- **仓库**:TS server 运行时整体删除(当时保留的 `__integration__`
  MIRROR-only 套件 + harness 四件:db/pool、redis、env(裁)、
  agents/runtime/jwt、email 种子切片,已于 #206 迁至
  `tests/integration/`);`agent-cli/`、`bin/cumora`、`scripts/dual-backend/`
  删除;CI tests job 换 golang:1.24-bookworm + setup-node(runner 自建
  Go 服当 SUT);契约守卫提取腿换 Go `HandleFunc`(#117 豁免表)。
- **生产机**:卸载 `cumora-ts.service`;旧 `cumora.service`(TS daemon,
  agent-cli)disable + 删;daemon.env 保留(byoa-daemon 仍用)。
- **现行回退(#211 起)**:`bash scripts/deploy/deploy-release.sh v<上一版本>`
  —— 重新下载旧 tag 制品、重铺其 releases/ 目录并重启,数据不动。
  注意两点:①需联网(不复用本地既有版本目录);②migrations 只前进无
  down——旧二进制跑在新 schema 上,属既有前进式策略,回退不回滚 schema。手工重建二进制
  仅作取证兜底(`godocker.sh build` 产物须落版本目录并自管 symlink)。
  Postgres/Redis 数据不动。
  TS 栈如需考古,git 历史完整留档(本 runbook 的切换日段落即其用法)。
- **已知缺口**:#117(missed-routes:polls/og/apple-native/autonomy/
  shipping/admin 子面)——切换日起即 404 的面,与退役无关,逐票补齐。
