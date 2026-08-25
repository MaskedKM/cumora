#!/usr/bin/env bash
# Generate Go types from the OpenAPI contract via containerized oapi-codegen
# (no local Go toolchain needed — Docker required, per self-host CI).
set -euo pipefail
cd "$(dirname "$0")/.."
if ! command -v docker >/dev/null 2>&1; then
  echo "[contract] docker 不可用 —— 跳过 Go 侧生成(生成物保持现状)" >&2
  exit 0
fi
mkdir -p packages/contract/go/gen
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD/packages/contract:/contract" -w /contract \
  -e GOCACHE=/tmp/gocache \
  golang:1.24-bookworm \
  go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.5.0 \
  -package gen -generate types -o go/gen/types.gen.go openapi.yaml
echo "[contract] go types → packages/contract/go/gen/types.gen.go"
