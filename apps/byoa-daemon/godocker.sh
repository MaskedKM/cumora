#!/usr/bin/env bash
# 本机无 Go 工具链:借 docker golang:1.24 容器跑 go 命令(与 CI 同镜像)。
# 用法:./godocker.sh build|vet|test [args...]
# 测试需要访问宿主 localhost 的 pg/redis,固走 --network host。
set -euo pipefail
cd "$(dirname "$0")"
cmd="${1:-build}"; shift || true
exec docker run --rm --network host \
  -v "$PWD":/app -w /app \
  -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
  -e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath \
  golang:1.24-alpine go "$cmd" "$@"
