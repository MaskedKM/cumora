#!/usr/bin/env bash
# 契约防漂移守卫:#48(OpenAPI-first);#139 起对账面含 server-go 生成物
# (internal/contract = 全量 types + 已迁移 tag 的 ServerInterface)。
# 自举 safe.directory —— CI job 容器内 root 的 git 会报 dubious ownership
# (actions/checkout 的 safe.directory 写在宿主用户配置里,容器 root 看不到)。
set -euo pipefail
cd "$(dirname "$0")/.."
git config --global --add safe.directory "$(pwd)" 2>/dev/null || true
npm run contract:gen
git add -A -- packages/contract apps/server-go/internal/contract
if ! git diff --cached --exit-code HEAD -- packages/contract apps/server-go/internal/contract; then
  echo "契约与生成物不同步 —— 跑 npm run contract:gen 并提交" >&2
  exit 1
fi
echo "[contract] 规范 ↔ 生成物与 HEAD 一致"
