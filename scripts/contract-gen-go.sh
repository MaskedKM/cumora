#!/usr/bin/env bash
# 契约 Go 生成(#139):全量 types + 已迁移域 tag 的 std-http ServerInterface,
# 输出 apps/server-go/internal/contract(包内零手写、整体再生成)。
# 双执行面:本机/有 docker → 容器跑(零工具链依赖,出口走宿主 Mihomo,
#   默认 ser8 形态 allow-lan *:7890;本机无 allow-lan 用
#   CONTRACT_GEN_NETWORK=host CONTRACT_GEN_PROXY=http://127.0.0.1:7897 覆盖);
# CI checks 容器(golang 镜像、无 docker CLI)→ 直接 go run(#186 评审 P2:
#   否则 Go 侧对账在 CI 恒跳过,规范漂移照绿)。
set -euo pipefail
cd "$(dirname "$0")/.."

GEN_NETWORK="${CONTRACT_GEN_NETWORK:-}"
GEN_PROXY="${CONTRACT_GEN_PROXY:-http://host.docker.internal:7890}"
NET_FLAG=(--add-host=host.docker.internal:host-gateway)
if [ -n "$GEN_NETWORK" ]; then NET_FLAG=(--network "$GEN_NETWORK"); fi

# 已迁移到 ServerInterface 的域 tag(扩域 = 此处加一行,生成物落
# $GEN_DIR/<tag>/ 独立子包 —— 同包多 tag 会重复声明 ServerInterface/
# Handler 等共享符号;types 留根包单文件,server 生成物不引用它)。
SERVER_TAGS=(documents push uploads projects)

GEN_DIR=apps/server-go/internal/contract
mkdir -p "$GEN_DIR"

# run_oapi <输出路径(相对仓库根)> <oapi-codegen 参数…>
run_oapi() {
  local out="$1"; shift
  if command -v docker >/dev/null 2>&1; then
    docker run --rm --user "$(id -u):$(id -g)" \
      -v "$PWD/packages/contract:/contract" -v "$PWD/apps/server-go:/out" -w /contract \
      -e GOCACHE=/tmp/gocache "${NET_FLAG[@]}" \
      -e HTTPS_PROXY="$GEN_PROXY" -e HTTP_PROXY="$GEN_PROXY" \
      golang:1.24-bookworm \
      go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.5.0 \
      "$@" -o "/out/${out#apps/server-go/}" openapi.yaml
  elif command -v go >/dev/null 2>&1; then
    (cd packages/contract &&
      go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.5.0 \
        "$@" -o "../../$out" openapi.yaml)
  else
    echo "[contract] docker/go 均不可用 —— 跳过 Go 侧生成(生成物保持现状)" >&2
    exit 0
  fi
}

run_oapi "$GEN_DIR/types.gen.go" -package contract -generate types

for tag in "${SERVER_TAGS[@]}"; do
  mkdir -p "$GEN_DIR/$tag"
  run_oapi "$GEN_DIR/$tag/server-$tag.gen.go" \
    -package "$tag"'contract' -generate std-http-server "--include-tags=$tag"
  # oapi-codegen 的 std-http 输出假设与 types 同包;子包只缺安全域常量,
  # 此别名文件随生成物一起产出(幂等覆写,勿手改)。
  cat > "$GEN_DIR/$tag/alias.go" <<GOEOF
// ${tag}contract —— 别名 glue:server 生成物引用的安全域常量在根包
// types 中。本文件由 contract-gen-go.sh 生成,勿手改。
package ${tag}contract

import "github.com/MaskedKM/cumora/apps/server-go/internal/contract"

const SessionBearerScopes = contract.SessionBearerScopes
GOEOF
done

echo "[contract] go 生成物 → $GEN_DIR(types 全量 + server 子包: ${SERVER_TAGS[*]})"
