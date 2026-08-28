# yjs-sidecar

Yjs 协同文档 sidecar(#50 起填充):从 `server/src/documents/` 进程化平移(逻辑不动),TS 进程,
与 Go server 经进程边界协议协作。远期 yrs/y-sweet 替换为明确的后置决策(ADR 0004)。

#142 起自持:infra(env/pg 池/redis/对象存储)内联于 `src/infra/`,不再穿透引用
`server/src`(运行时依赖归零——`server/src` 只剩集成测试 harness)。workspace 成员
(`@cumora/yjs-sidecar`),依赖仍由根 hoist 提供;`npm run sidecar:typecheck` 全量 tsc。
