#!/usr/bin/env bash
# #283 PR-C:桌面单制品的载荷 staging + MANIFEST 生成。
# 用法:build-desktop-payload.sh <payloadDir> <binDir> <depsDir> <repoRoot> <version>
#   binDir  —— 已编好的五栈二进制(cumora-server/daemon/sidecar/stack/stackd)
#   depsDir —— stack-deps 产物卷目录(/deps/<lockhash>:pg/ redis-* MANIFEST.deps)
#   repoRoot —— 仓库根(migrations/ 来源)
# 产物:payloadDir/{五二进制, redis-server, redis-cli, pg/, migrations/, MANIFEST}
# —— 即 PR-B absorb 的输入面(resources/bin 的内容;六可执行件契约门对齐)。
# MANIFEST.files 覆盖载荷内全部文件(排除 MANIFEST 自身);deps 从
# MANIFEST.deps 合成(postgresql/pgvector/redis 版本+源 sha,可复验)。
set -euo pipefail

PAYLOAD="${1:?用法: build-desktop-payload.sh <payloadDir> <binDir> <depsDir> <repoRoot> <version>}"
BINDIR="${2:?}"
DEPSDIR="${3:?}"
REPO="${4:?}"
VERSION="${5:?}"

for f in cumora-server cumora-daemon cumora-sidecar cumora-stack cumora-stackd; do
  test -x "$BINDIR/$f" || { echo "::error::binDir 缺可执行件 $f"; exit 1; }
done
for f in redis-server redis-cli MANIFEST.deps pg/bin/postgres pg/bin/initdb; do
  test -e "$DEPSDIR/$f" || { echo "::error::depsDir 缺件 $f(stack-deps 产物不完整?)"; exit 1; }
done
test -d "$REPO/apps/server-go/migrations" || { echo "::error::repoRoot 缺 apps/server-go/migrations"; exit 1; }

rm -rf "$PAYLOAD"
mkdir -p "$PAYLOAD"
cp "$BINDIR"/cumora-server "$BINDIR"/cumora-daemon "$BINDIR"/cumora-sidecar \
   "$BINDIR"/cumora-stack "$BINDIR"/cumora-stackd "$PAYLOAD/"
chmod 0755 "$PAYLOAD"/cumora-*
cp "$DEPSDIR"/redis-server "$DEPSDIR"/redis-cli "$PAYLOAD/"
chmod 0755 "$PAYLOAD"/redis-server "$PAYLOAD"/redis-cli
# -L 摊平 pg/lib 下的 8 个符号链接(libpq.so 等):find -type f 不列链接,
# 留链接=MANIFEST 覆盖面有缝;摊平后清单与 absorb 落盘端到端一致
#(#303 评审 P2)。
cp -rL "$DEPSDIR/pg" "$PAYLOAD/pg"
cp -r "$REPO/apps/server-go/migrations" "$PAYLOAD/migrations"

# files 清单:载荷内全部文件(排除 MANIFEST 自身),正斜杠相对路径。
# 临时清单必须放载荷目录之外——shell 重定向先于 find 建文件,放里面会把
# 半成品清单自己列进 MANIFEST,随后删除=悬空条目(absorb Verify 实证抓到)。
SHA_TMP="$(mktemp)"
trap 'rm -f "$SHA_TMP"' EXIT
( cd "$PAYLOAD" && find . -type f ! -name MANIFEST -print0 | sort -z | xargs -0 sha256sum ) > "$SHA_TMP"

# MANIFEST 组装(node;readFileSync+JSON.parse 纪律——require 不认自选后缀)。
node -e '
  const fs = require("fs");
  const [shaList, payload, depsDir, version] = process.argv.slice(1);
  const files = {};
  for (const line of fs.readFileSync(shaList, "utf8").split("\n")) {
    const m = line.match(/^([0-9a-f]{64})\s+\*?(.+)$/);
    if (!m) continue;
    files[m[2].replace(/^\.\//, "")] = m[1];
  }
  if (Object.keys(files).length === 0) throw new Error("files 清单为空");
  const depsRaw = JSON.parse(fs.readFileSync(depsDir + "/MANIFEST.deps", "utf8"));
  const deps = {};
  for (const [k, v] of Object.entries(depsRaw.components || {}))
    deps[k] = { version: v.version, sourceSha256: v.sourceSha256 };
  const m = { version, createdAt: new Date().toISOString(), files, deps };
  fs.writeFileSync(payload + "/MANIFEST", JSON.stringify(m, null, 2) + "\n");
  console.log("[payload] MANIFEST:", Object.keys(files).length, "件;deps:",
    Object.entries(deps).map(([k, v]) => k + "@" + v.version).join(" "));
' "$SHA_TMP" "$PAYLOAD" "$DEPSDIR" "$VERSION"
echo "[payload] 落盘 $PAYLOAD:"
du -sh "$PAYLOAD"
