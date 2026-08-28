#!/usr/bin/env bash
# #69 切换日:安装 systemd 用户单元(拷贝 + daemon-reload)。
# 只装不启——启动顺序与观察按 docs/SWITCHOVER.md runbook 编排。
# 用法:bash scripts/deploy/install-units.sh
set -euo pipefail

here="$(cd "$(dirname "$0")/systemd" && pwd)"
dest="${HOME}/.config/systemd/user"
mkdir -p "$dest"

for unit in cumora-go cumora-sidecar cumora-daemon; do
  install -m 0644 "$here/${unit}.service" "$dest/${unit}.service"
  echo "installed ${dest}/${unit}.service"
done

systemctl --user daemon-reload
# sidecar 常开;daemon 随切换日拉起;go 由 runbook 显式编排,不 enable
systemctl --user enable cumora-sidecar.service >/dev/null 2>&1 || true
echo "done — start order per docs/SWITCHOVER.md"
