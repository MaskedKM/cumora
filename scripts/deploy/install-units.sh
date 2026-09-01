#!/usr/bin/env bash
# #69 切换日:安装 systemd 用户单元(拷贝 + daemon-reload)。
# #282 起:这是三 unit(旧)形态的安装器,同时是 `cumora-stack
# uninstall` 的回滚目标;单 unit(cumora.service)形态经
# `cumora-stack install` 安装,不再经过本脚本。
# #211 部署收口:三件套全 enable —— 机器重启/手动 stop 后自愈(8-31
# 事故三病之二收口;此前只 enable sidecar,go/daemon 故意不 enable,
# 重启后全停不自愈)。启动顺序由单元间的 After=/Wants= 表达:
#   sidecar(ExecStartPost healthz 门)→ go(ExecStartPost livez 门)→ daemon
# 仍只装+enable 不 start —— 首次拉起与发版切换按 docs/SWITCHOVER.md
# (deploy-release.sh 会 restart 三件套)。
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
# 三件套全 enable(#211):重启机器即按 After= 链自愈;enable 失败要
# 大声失败(此前 || true 会把 enable 断裂静默吞掉 —— 又一个不自愈源)。
systemctl --user enable cumora-sidecar.service cumora-go.service cumora-daemon.service

# 二进制面已指 ~/.local/share/cumora/current(#211;#280 起三件全制品):
# 未部署 release 时提前指出,而非留到 start 时报 ExecStart 找不到文件。
# #280 特别注意:旧 release(< 含 sidecar 的 tag)配新 unit 会把 sidecar
# ExecStart 打穿 —— warn 同样覆盖,升级顺序=先 deploy-release 再 install-units。
miss=0
for bin in cumora-server cumora-daemon cumora-sidecar; do
  if [ ! -x "$HOME/.local/share/cumora/current/$bin" ]; then
    echo "warn: ~/.local/share/cumora/current/$bin 不存在/不可执行 ——" >&2
    miss=1
  fi
done
if [ "$miss" = 1 ]; then
  echo "  先跑 bash scripts/deploy/deploy-release.sh <tag>(制品需含三 binaries,#280 起)再 start 三件套" >&2
fi
echo "done — 三件套已 enable,重启机器按 sidecar → go → daemon 自愈;start/发版见 docs/SWITCHOVER.md"
