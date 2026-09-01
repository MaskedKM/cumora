#!/usr/bin/env bash
# 本机无 Go 工具链:借 docker golang:1.24 容器跑 go 命令(与 CI 同镜像)。
# 用法:./godocker.sh build|vet|test|fmt|mod [args...](如 ./godocker.sh mod tidy)
# doctor/status 的单测用 fake 探针,不需要宿主 pg/redis,但保持 --network host
# 与 server-go 脚本同构,便于将来加集成腿。代理透传同 server-go:Mihomo 场景
# export HTTPS_PROXY 后生效。
set -euo pipefail
cd "$(dirname "$0")"
cmd="${1:-build}"; shift || true
exec docker run --rm --network host \
  -v "$PWD":/app -w /app \
  -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
  -e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath \
  -e HTTPS_PROXY -e HTTP_PROXY -e NO_PROXY \
  -e DATABASE_URL -e CUMORA_GO_MIGRATIONS \
  golang:1.24-alpine go "$cmd" "$@"
