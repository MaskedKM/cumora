# yjs-sidecar

Yjs 协同文档 sidecar(#50 起填充):从(已退役 TS server 的)`documents/` 模块进程化平移(逻辑不动),TS 进程,
与 Go server 经进程边界协议协作。远期 yrs/y-sweet 替换为明确的后置决策(ADR 0004)。

#280 起分发形态变更:源码仍是 TS(CRDT 引擎不动),但随 go-release 制品分发的形态是
`bun build --compile` 的自包含单二进制 `cumora-sidecar`(ADR 0005 路线 A:Go/yrs 位级兼容重写被否,
Hocuspocus 生态主流即 TS 引擎);生产 unit 从 `node --import tsx` 工作树运行切到制品寻址。
CI(pr.yml)有产物冒烟腿:对编译后的二进制直接打 HTTP 契约。

#142 起自持:infra(env/pg 池/redis/对象存储)内联于 `src/infra/`,不再穿透引用
退役 server 树(运行时依赖归零;原树的集成测试 harness 已于 #206 迁至 `tests/integration/`)。workspace 成员
(`@cumora/yjs-sidecar`),依赖仍由根 hoist 提供;`npm run sidecar:typecheck` 全量 tsc。
