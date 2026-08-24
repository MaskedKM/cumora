#!/usr/bin/env bash
# 同步 upstream(yetone/cumora) 新提交到 main,再合并进 my-custom。
# main 用 --ff-only,保证 main 分支永远是 upstream 的纯镜像。
# my-custom 冲突时不自动提交,退出让人工处理。
set -euo pipefail
cd "$(dirname "$0")/.."

git fetch upstream
git fetch origin

NEW=$(git rev-list HEAD..upstream/main --count 2>/dev/null || echo 0)
if [ "$NEW" -eq 0 ]; then
  echo "upstream 无新提交,无需同步"
  exit 0
fi

echo "upstream 有 $NEW 个新提交,开始同步..."
git checkout main
git merge --ff-only upstream/main
git push origin main

git checkout my-custom
if git merge main -m "sync: merge upstream/main"; then
  git push origin my-custom
  echo "同步完成,my-custom 已合并并推送"
else
  echo "合并冲突 —— 解决后执行: git add <文件> && git commit && git push origin my-custom"
  exit 1
fi
