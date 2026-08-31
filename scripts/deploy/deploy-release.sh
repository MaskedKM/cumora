#!/usr/bin/env bash
# #211 部署收口:server/daemon 部署物从"仓库工作树手工构建"切到
# go-release.yml 的 release 制品。流程(票面验收路径):
#
#   下载制品 → sha256 校验 → 落版本化目录 → 原子切 current symlink → 重启三件套
#
# 目录组织(~/.local/share/cumora/ 相邻子树,uploads 属 #208/#248,本脚本
# 永不触碰):
#
#   ~/.local/share/cumora/
#   ├── releases/<vX.Y.Z>/     # 制品解包:cumora-server + cumora-daemon + migrations/ + VERSION
#   ├── current -> releases/<vX.Y.Z>   # systemd ExecStart 经此寻址(symlink 内路径)
#   └── uploads/               # #208 迁出的生产数据,与部署物解耦
#
# 用法:
#   bash scripts/deploy/deploy-release.sh latest     # 最新 release
#   bash scripts/deploy/deploy-release.sh v0.4.0     # 指定 tag(裸 0.4.0 亦可,自动补 v)
#
# 回滚 = 同脚本指旧 tag(旧版本目录仍在 releases/ 下):
#   bash scripts/deploy/deploy-release.sh v0.3.9
#
# 环境覆盖:CUMORA_REPO(默认 MaskedKM/cumora)、CUMORA_SHARE_DIR
# (默认 ~/.local/share/cumora)。
set -euo pipefail

REPO="${CUMORA_REPO:-MaskedKM/cumora}"
SHARE_DIR="${CUMORA_SHARE_DIR:-$HOME/.local/share/cumora}"
RELEASES_DIR="$SHARE_DIR/releases"
CURRENT_LINK="$SHARE_DIR/current"

die() { echo "deploy-release: 错误:$*" >&2; exit 1; }
say() { echo "deploy-release: $*"; }

# ── 前置工具 ──────────────────────────────────────────────────────────
command -v gh >/dev/null 2>&1 || die "gh CLI 不在 PATH —— release 制品下载依赖 gh(gh auth login 后重试)"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum 不在 PATH —— 无法校验制品完整性,拒绝部署"
command -v tar >/dev/null 2>&1 || die "tar 不在 PATH"
command -v curl >/dev/null 2>&1 || die "curl 不在 PATH —— 部署后探活依赖"

# ── 版本解析(tag 规范化为 vX.Y.Z;latest 查实际 tag)────────────────
want="${1:-latest}"
[ -n "$want" ] || die "用法:bash scripts/deploy/deploy-release.sh <tag|latest>"
if [ "$want" != "latest" ] && ! [[ "$want" == v* ]]; then
  want="v$want"
fi
say "解析 release(仓库 $REPO,请求 $want)…"
tag="$(gh release view "$want" --repo "$REPO" --json tagName --jq .tagName 2>/dev/null)" \
  || die "查不到 release '$want'($REPO)。核对:gh release list --repo $REPO;gh auth status"
[ -n "$tag" ] || die "release '$want' 的 tagName 为空,异常形态"
[[ "$tag" == v* ]] || die "release tag '$tag' 不以 v 开头 —— 非本发布流谱系,拒绝部署"

# ── 平台探测(与 go-release.yml 的 cumora-<os>-<arch>.tar.gz 对齐)───
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "未支持的平台架构:$(uname -m)(制品矩阵为 amd64/arm64)" ;;
esac
asset="cumora-${os}-${arch}.tar.gz"

# ── 下载 + sha256 校验 ───────────────────────────────────────────────
work="$(mktemp -d "${TMPDIR:-/tmp}/cumora-deploy.XXXXXX")"
trap 'rm -rf "$work"' EXIT
say "下载 $tag 的 $asset 与 SHA256SUMS…"
if ! gh release download "$tag" --repo "$REPO" \
     --pattern "$asset" --pattern "SHA256SUMS" --dir "$work" --clobber; then
  die "下载失败:$REPO release $tag 缺 $asset 或 SHA256SUMS(网络问题先查:gh auth status / gh api repos/$REPO/releases/tags/$tag)"
fi
[ -s "$work/$asset" ] || die "下载产物缺失:$asset(gh 静默未报错,异常)"
[ -s "$work/SHA256SUMS" ] || die "下载产物缺失:SHA256SUMS"

want_sum="$(awk -v a="$asset" '$2 == a {print $1; exit}' "$work/SHA256SUMS")"
[ -n "$want_sum" ] || die "SHA256SUMS 里没有 $asset 的条目 —— 校验源不完整,拒绝部署"
got_sum="$(sha256sum "$work/$asset" | awk '{print $1}')"
if [ "$got_sum" != "$want_sum" ]; then
  die "sha256 不匹配($asset):期望 ${want_sum:0:16}… 实得 ${got_sum:0:16}… —— 制品损坏或被篡改,拒绝部署"
fi
say "sha256 校验通过(${want_sum:0:16}…)"

# ── 解包到版本化目录(staging 全成后再切,失败不伤 current)──────────
mkdir -p "$RELEASES_DIR"
staging="$RELEASES_DIR/.staging-${tag}.$$"
rm -rf "$staging"
trap 'rm -rf "$work" "$staging"' EXIT
mkdir -p "$staging"
tar xzf "$work/$asset" -C "$staging"
inner="$staging/cumora-${os}-${arch}"
[ -x "$inner/cumora-server" ] || die "制品缺可执行的 cumora-server —— $tag 不是本发布流的形态"
[ -x "$inner/cumora-daemon" ] || die "制品缺可执行的 cumora-daemon"
if [ ! -d "$inner/migrations" ]; then
  die "制品缺 migrations/ —— $tag 早于 #211(制品未自包含 schema)。请部署 #211 合入后新打的 tag"
fi
echo "$tag" > "$inner/VERSION"

target="$RELEASES_DIR/$tag"
# current 已指向同 tag 时拒绝裸重部署:rm→mv 窗口会把在跑版本的目录
# 打穿(ENOENT + Restart=always 循环)。livez 503 后原 tag 复验是现实
# 路径——此场景重启即可,无需重铺目录;确要重铺先切走再指回。
if [ -L "$CURRENT_LINK" ] && [ "$(readlink "$CURRENT_LINK" 2>/dev/null)" = "releases/$tag" ]; then
  die "current 已指向 releases/$tag —— 同 tag 重部署会打穿在跑版本目录。复验用 systemctl --user restart 三件套即可;确要重铺:先部署/切走别的 tag。"
fi
mv "$inner" "$target"
rm -rf "$staging"
trap 'rm -rf "$work"' EXIT
chmod 0755 "$target/cumora-server" "$target/cumora-daemon"
say "落盘 $target(server + daemon + migrations + VERSION)"

# ── 原子切 current symlink ───────────────────────────────────────────
rm -f "${CURRENT_LINK}.new"   # 清上次中断的残留(否则 ln 会建进目录内部)
ln -s "releases/$tag" "${CURRENT_LINK}.new"
mv -T "${CURRENT_LINK}.new" "$CURRENT_LINK"
say "current -> $(readlink "$CURRENT_LINK")(原子切换完成)"

# ── 重启三件套(After= 链:sidecar → go → daemon)───────────────────
if ! systemctl --user restart cumora-sidecar.service cumora-go.service cumora-daemon.service; then
  die "三件套 restart 失败 —— 单元未装则先:bash scripts/deploy/install-units.sh;再查 systemctl --user status cumora-go"
fi

# ── 部署后核验:livez(进程活 + Redis 硬依赖)────────────────────────
code=000
for _ in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 http://127.0.0.1:5181/api/livez 2>/dev/null || true)"
  [ -n "$code" ] && [ "$code" != "000" ] && break
  code=000
  sleep 1
done
case "$code" in
  200) say "livez 200(进程 + Redis 事件面均活)" ;;
  503) die "livez 503 —— 进程已活但 Redis 不可达/事件面降级 Noop(#211 起显性变红,不再假绿)。查:systemctl status redis(redis 为系统级服务,非 --user 域)或 journalctl --user -u cumora-go | grep redis" ;;
  *)   die "livez 探测失败(HTTP $code / 无响应)—— journalctl --user -u cumora-go -u cumora-sidecar 取证" ;;
esac

say "部署完成:版本 $(cat "$CURRENT_LINK/VERSION")"
say "核验:systemctl --user status cumora-go(ExecStart 应经 $CURRENT_LINK 寻址);readlink $CURRENT_LINK;cat $CURRENT_LINK/VERSION"
