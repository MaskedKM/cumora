#!/usr/bin/env bash
# 契约 Go 生成(#139):全量 types + 已迁移域 tag 的 std-http ServerInterface,
# 输出 apps/server-go/internal/contract(包内零手写、整体再生成)。
# 容器化 oapi-codegen —— 无需本地 Go 工具链(docker 必需,自托管 CI 同)。
# 模块拉取走宿主 Mihomo —— 默认 ser8 形态(allow-lan *:7890,经
# host.docker.internal,与 pr.yml 出口惯例同款)。本机(无 allow-lan)覆盖:
#   CONTRACT_GEN_NETWORK=host CONTRACT_GEN_PROXY=http://127.0.0.1:7897
GEN_NETWORK="${CONTRACT_GEN_NETWORK:-}"
GEN_PROXY="${CONTRACT_GEN_PROXY:-http://host.docker.internal:7890}"
NET_FLAG=(--add-host=host.docker.internal:host-gateway)
if [ -n "$GEN_NETWORK" ]; then NET_FLAG=(--network "$GEN_NETWORK"); fi
set -euo pipefail
cd "$(dirname "$0")/.."
if ! command -v docker >/dev/null 2>&1; then
  echo "[contract] docker 不可用 —— 跳过 Go 侧生成(生成物保持现状)" >&2
  exit 0
fi

# 已迁移到 ServerInterface 的域 tag(扩域 = 此处加一行;contract-check.sh
# 对账路径守卫天然覆盖新生成物)。
SERVER_TAGS=(documents)

GEN_DIR=apps/server-go/internal/contract
mkdir -p "$GEN_DIR"

docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD/packages/contract:/contract" -v "$PWD/apps/server-go:/out" -w /contract \
  -e GOCACHE=/tmp/gocache \
  "${NET_FLAG[@]}" \
  -e HTTPS_PROXY="$GEN_PROXY" -e HTTP_PROXY="$GEN_PROXY" \
  golang:1.24-bookworm \
  go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.5.0 \
  -package contract -generate types -o "/out/internal/contract/types.gen.go" openapi.yaml

for tag in "${SERVER_TAGS[@]}"; do
  docker run --rm --user "$(id -u):$(id -g)" \
    -v "$PWD/packages/contract:/contract" -v "$PWD/apps/server-go:/out" -w /contract \
    -e GOCACHE=/tmp/gocache \
  "${NET_FLAG[@]}" \
  -e HTTPS_PROXY="$GEN_PROXY" -e HTTP_PROXY="$GEN_PROXY" \
    golang:1.24-bookworm \
    go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.5.0 \
    -package contract -generate std-http-server "--include-tags=$tag" \
    -o "/out/internal/contract/server-$tag.gen.go" openapi.yaml
done

echo "[contract] go 生成物 → $GEN_DIR(types 全量 + server: ${SERVER_TAGS[*]})"
