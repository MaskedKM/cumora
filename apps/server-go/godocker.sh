#!/usr/bin/env bash
# 本机无 Go 工具链:借 docker golang:1.24 容器跑 go 命令(与 CI 同镜像)。
# 用法:./godocker.sh build|vet|test [args...]
# 测试需要访问宿主 localhost 的 pg/redis,固走 --network host。
# 代理透传:`-e VAR`(无值)把调用方环境的值带进容器;未设置时容器内
# 得到空串变量(非 unset)—— 对 Go 等价于无代理。直连 proxy.golang.org
# 被掐时,export HTTPS_PROXY 后本脚本与 tests/integration/run.mjs 的
# 内置构建腿即可走 Mihomo(容器不读宿主 shell env,docker 也不透传)。
set -euo pipefail
cd "$(dirname "$0")"
cmd="${1:-build}"; shift || true
exec docker run --rm --network host \
  -v "$PWD":/app -w /app \
  -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
  -e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath \
  -e HTTPS_PROXY -e HTTP_PROXY -e NO_PROXY \
  golang:1.24-alpine go "$cmd" "$@"
