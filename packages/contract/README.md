# @cumora/contract

OpenAPI 单一契约源(ADR 0004 · 票 #48)。

- `openapi.yaml` — 全部存活路由的唯一事实源;WS/SSE 事件面的描述策略见 info.description
- `src/schema.d.ts` — openapi-typescript 生成(web 消费;`npm run contract:gen`)
- Go 生成物在 `apps/server-go/internal/contract/`(#139:全量 types + 已迁移 tag 的 std-http ServerInterface;`scripts/contract-gen-go.sh` 生成,Docker 运行,无需本机 Go)

`npm run contract:check` = 双侧重生成 + git diff --exit-code,防契约漂移(CI quick job 执行)。

## Go 侧说明

Go 消费 = 域包实现 ServerInterface(`SERVER_TAGS` 清单即迁移进度);本目录只承载规范与 TS 侧产物。
