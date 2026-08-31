# 0004 — Server migrates to Go, Yjs stays a TS sidecar

## Status

Accepted (2026-08-25)

## Context

The server is a single TypeScript package: one root `package.json` shared
with the web/Electron/mobile clients, no services layer (`api/router.ts`
is ~6.2k lines), background jobs as in-process `setInterval` workers run
from source via tsx, hand-duplicated API types on both sides of the wire
(no zod, no codegen), and a large npm dependency tree. The stated pains
are structural boundaries, contract drift, and the TS runtime itself
(deployment artifact, memory footprint, dependency upkeep, language
discipline). Yjs collaborative-document sync (the TS tree's `documents/` subsystem, now `apps/yjs-sidecar/`)
is the one subsystem with no mature equivalent outside the JS ecosystem
(Rust's `yrs`/y-sweet aside).

## Decision

The server is rewritten in Go as a single binary for the single-box
deployment (ADR 0003). The Yjs document bus initially remains a small TS
sidecar process; whether it later moves to `yrs`/y-sweet is an open,
reversible follow-up. FCM push is reimplemented via firebase-admin Go.
Contracts become OpenAPI-first: `oapi-codegen` on the Go side,
`openapi-typescript` codegen for the web client, replacing the
hand-written types and fetch client. Postgres stays; migrations move to
goose over the existing schema, queries go through sqlc/pgx. Background
jobs become a supervised job group with graceful drain inside the one
binary — the sidecar is the only separate server process.

Migration mechanism: parallel rewrite against a frozen target. The
workspace file-dimension stream (#27–#34) finishes on TS first, then TS
features freeze; the Go version is written against the TS behavior and
the existing unit/integration suites as the acceptance mirror, validated
behind a reverse proxy, and cut over in one switch with the old TS server
retained as rollback.

The BYOA daemon joins the migration. Its source currently squats in
the TS server's `agents/computer/` (~5.9k LOC, bundled across the tree into
the `agent-cli` shim) and is deleted with the TS server, so "leave it in
TS" was never a zero-cost default. The daemon shape itself is retained —
an outbound push channel plus a local engine spawner is the
industry-standard pattern for server-triggered work on an operator
machine (GitHub Actions self-hosted runner's Listener/Worker;
cloudflared/ngrok-style outbound-only tunnels); the no-daemon
alternatives (timer-pull, in-app hosting, engines as direct server
children) were considered and rejected. It is rewritten in Go in the
same window as a separate binary in the same repo and release pipeline,
and distribution moves off npm — the `cumora` package name belongs to
upstream, so post-fork the npm channel would pull upstream daemons — to
single binaries on GitHub Releases (linux/darwin, amd64/arm64,
checksums) with self-update polling our own releases.

Rejected alternatives: full Rust including Yjs via `yrs` (strongest type
discipline, but slower iteration for an agent-maintained CRUD-heavy
server); strangler-style module-by-module cutover (spreads risk but
imposes months of dual-stack maintenance for the same features); staying
TS with workspaces + packaging (smallest effort, but only mitigates the
runtime and dependency pains).

## Consequences

- The rewrite starts only after the workspace stream closes; anything
  unfinished elsewhere (e.g. DM-voice #24/#25) ports by spec to the Go
  side instead of gating the migration.
- The TS daemon retires together with the TS server as the rollback
  pair; `checkForUpdate`'s npm-registry polling must not survive the
  migration. Until the window opens, daemon builds come from `main`
  only — the "built from the wrong branch, lost an engine" incident
  class is a build-discipline failure, not a language failure.
- The 59-file unit suite and integration suite are the rewrite's
  acceptance mirror and must keep passing against the Go surface before
  cutover.
- Repo layout moves to a light monorepo (apps/web, apps/server-go,
  apps/yjs-sidecar, apps/byoa-daemon, packages/contract); client shells
  (electron/, ios/, android/) keep building off the web bundle.
- Cloud-side workers `r2-gate` is deleted with the cloud path (files are
  served from local disk); `email-gate` (Cloudflare Worker) is kept.

## 退役落地(#70,2026-08-28 勾销)

- ✅ TS server/daemon/agent-cli 全套删除(git 留档);`bin/cumora` npm 壳
  随之退役——"leave it in place" 窗口以用户提前点头终结。
- ✅ 验收镜像转 MIRROR-only:harness 保留件(pool/redis/env 裁/jwt/
  email 种子切片)+ runner 自建 Go 服当 SUT;59 文件单测随 TS 运行时
  退役,镜像套件 25 文件继续作为 Go 面的验收基准。
- ✅ 回退语义从"TS 回切床"改为"上一 commit 的 Go 二进制重建重启"。
- ⚠️ 勾销时发现 #117:一组 TS 已实现而 Go 未移植的 HTTP 路由
  (polls/og/apple-native/autonomy/shipping/admin 子面)自切换日起
  404——契约守卫换 Go 腿后以豁免表显式记账,逐票补齐。

## 契约硬化:Go 真消费生成类型(#139,2026-08-29)

- 决策原文"oapi-codegen on the Go side"长期只兑现了一半:生成物
  (types.gen.go 2,680 行)落在 packages/contract/go 且**零 import**,
  Go 侧唯一约束是 coverage 脚本的正则路由对账——#117 正是从这条缝
  漏进生产的。
- 落地形态(分阶段):生成管线搬进 apps/server-go(`internal/contract`
  = 根包全量 types + 每 tag 独立子包的 std-http ServerInterface —— 同包
  多 tag 会重复声明共享符号,子包经一行生成式常量别名 glue 引根包;
  `contract-gen-go.sh` 的 `SERVER_TAGS` 清单即迁移进度表),域包实现
  接口后路由注册串由规范生成(pattern 即契约),`var _ …ServerInterface`
  编译期强制每个 operation 有实现。documents 为首个试点域。生成器双
  执行面:本机 docker 跑、CI checks 容器(无 docker)直跑 go run,Go 侧
  对账两侧都真生效。
- 请求体解码暂留手写:TS 的 `String(x ?? '')`/typeof-filter 强转与
  生成类型的指针/严格解码语义不兼容(CreateDocumentJSONBody 是
  `*string`、SetDocumentCollaboratorsJSONBody 是严格 `[]string`),
  typed 解码会改行为;全面类型化待 strict-server 评估。
- coverage 守卫升双形态:手写 `HandleFunc("GET /x")` 与生成
  `HandleFunc("GET "+options.BaseURL+"/x")` 同腿对账,零豁免表。
- 余 17 tag 机械迁移(闭包工厂 → 接口方法)渐进消化,不再豁免。

## WS 事件面契约化(#221,2026-08-31)

- HTTP 面(#187 收官)之外的第三份手写锁步:WS 事件联合在 apps/web
  client.ts、集成 harness 的 redis.ts、Go internal/events/publish.go +
  wsx 网关(yjs-sidecar 另有一份 doc.* 内联副本)各写一遍,漂移只能运
  行时发现——calendar.reminder/msg.delta 两个孤儿通道正是这样潜伏的。
- 选型:纯 JSON Schema 单文件(packages/contract/ws-events.json)+
  零依赖自写生成器(scripts/contract-gen-ws.mjs),不用 AsyncAPI——其
  Go 生成器在离线/弱出口环境不可靠,且事件载荷需交叉引用既有 OpenAPI
  组件(Message/Status/Poll…),JSON Schema 经 `openapi.yaml#/…` $ref
  直连两份生成物(openapi-typescript 的 Schemas / oapi-codegen 的
  contract 包语义)。生成器输出确定性:gofmt 字节形显式复刻(const `=`
  列与字段列对齐、结构体正文零注释),双跑 diff 为零。
- 双端产物:TS `packages/contract/src/ws-events.d.ts`(WsEvent 全量联合
  / WsBroadcastEvent 总线联合 / WsChannels 通道映射)+ Go
  `internal/events/ws.gen.go`(事件名与通道常量、CompanyChannels、逐
  事件载荷结构体)。三端手写退役:client.ts 的 union 改 import;harness
  与 sidecar 的接口改 re-export 且通道常量 `satisfies WsChannels[…]`
  钉值;publish.go 与 wsx 网关改组生成结构体,通道/事件字面量清零。
- 行为等价基线:逐事件字段与退役前一致(时间键 at 的 agent ISOms /
  用户 RFC3339Nano 双语义、actorId 等可空键、typing.companyId 恒带等
  以 x-go 注记逐键保真);Go 发布方深层载荷(message/poll 等)仍为
  map[string]any——发布方从 DB 行动态构形,深层类型化留给消费端。
- 守卫双卡:contract:check 再生对账(生成物漂移即红)+ 三端手写漂移
  grep(client.ts/harness/sidecar/publish.go/wsx 不得再内联事件形状)。
  域包 PublishRaw 发布点(domains/agent/sched)载荷字面量的收敛留后
  续票,通道与事件名引用已全部走生成常量。
