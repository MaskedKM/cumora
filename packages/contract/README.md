# @cumora/contract

OpenAPI 单一契约源(ADR 0004 · 票 #48)。

- `openapi.yaml` — 全部存活路由的唯一事实源;WS/SSE 事件面的描述策略见 info.description
- `src/schema.d.ts` — openapi-typescript 生成(web 消费;`npm run contract:gen`)
- `go/gen/types.gen.go` — oapi-codegen 生成(Go server/daemon 消费;Docker 运行,无需本机 Go)

`npm run contract:check` = 双侧重生成 + git diff --exit-code,防契约漂移(CI quick job 执行)。

## Go 侧说明

`go/gen/` 无 go.mod —— 类型包由 #51 起 vendor 进 apps/server-go 的 Go module 使用(本目录只承载生成物)。
