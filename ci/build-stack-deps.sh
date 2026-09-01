#!/usr/bin/env bash
# #283 PR-A:Stack 依赖物(pg16+pgvector / redis)钉版源码构建。
# 用法:build-stack-deps.sh <srcdir> <outdir>
#   <srcdir>:源 tarball 缓存目录(缺件/坏件自动经代理补拉并 sha 校验)。
#   <outdir>:产物目录 —— pg/(整套 bin+lib,含 pgvector 扩展)、
#             redis-server、redis-cli、MANIFEST.deps。
#
# 设计事实(#283 票面 + ADR 0005):
#   - 源码 release tarball 自带预生成的 parser 文件 → 免 bison/flex;
#     --without-readline/zlib/icu 且不开 openssl → 产物纯 glibc 依赖
#     (ldd 冒烟把关,AppImage 跨发行面不埋 .so 缺口)。
#   - 构建容器钉 golang-bookworm-ci(127.0.0.1:5500,runner-maintenance
#     bake 维护):gcc/make 即全部构建依赖;glibc 目标与生产/桌面一致
#     (#280 musl 烙印教训)。
#   - 版本与 sha 全在 stack-deps.lock(同目录);升级 = 改 lock 重跑。
set -euo pipefail

SRC="${1:?用法: build-stack-deps.sh <srcdir> <outdir>}"
OUT="${2:?用法: build-stack-deps.sh <srcdir> <outdir>}"
LOCK="$(cd "$(dirname "$0")" && pwd)/stack-deps.lock"

# lock 是 JSON;脚本只跑在 bookworm-ci(自带 node),零额外依赖取值。
# (readFileSync+JSON.parse 而非 require —— require 不认 .lock 后缀,
# 会把 JSON 当 JS 解析,实测翻车点。)
val() { node -e '
  const l = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
  console.log(l[process.argv[2]][process.argv[3]]);
' "$LOCK" "$1" "$2"; }
PG_VER="$(val postgresql version)"; PG_URL="$(val postgresql url)"; PG_SHA="$(val postgresql sha256)"
PV_VER="$(val pgvector version)";    PV_URL="$(val pgvector url)";    PV_SHA="$(val pgvector sha256)"
RD_VER="$(val redis version)";       RD_URL="$(val redis url)";       RD_SHA="$(val redis sha256)"

fetch() { # <url> <dest> <sha256> — 命中缓存跳过;下载后强校验
  local url="$1" dest="$2" sha="$3"
  if [ -s "$dest" ] && echo "$sha  $dest" | sha256sum -c - >/dev/null 2>&1; then
    echo "[deps] cache hit: $(basename "$dest")"
    return 0
  fi
  echo "[deps] 下载 $(basename "$dest") …"
  for i in 1 2 3; do
    curl -fsSL --http1.1 --connect-timeout 15 --retry 3 --retry-delay 3 \
      -o "$dest" "$url" && break
    rm -f "$dest"; [ "$i" = 3 ] && { echo "::error::$(basename "$dest") 三次下载失败"; exit 1; }
    sleep 5
  done
  echo "$sha  $dest" | sha256sum -c - || { echo "::error::$(basename "$dest") sha256 不符(lock 钉扎失败)"; exit 1; }
}

command -v make gcc node >/dev/null 2>&1 || {
  for tool in make gcc node; do
    command -v "$tool" >/dev/null 2>&1 || echo "::error::缺构建工具:$tool(bookworm-ci 镜像坏?)"
  done
  exit 1
}
mkdir -p "$SRC" "$OUT"
BUILD="$(mktemp -d)"
trap 'rm -rf "$BUILD"' EXIT

fetch "$PG_URL" "$SRC/pg.tar.bz2" "$PG_SHA"
fetch "$PV_URL" "$SRC/pgvector.tar.gz" "$PV_SHA"
fetch "$RD_URL" "$SRC/redis.tar.gz" "$RD_SHA"

echo "[deps] 构建 postgresql $PG_VER(最小依赖面)…"
tar xf "$SRC/pg.tar.bz2" -C "$BUILD"
( cd "$BUILD/postgresql-$PG_VER" \
  && ./configure --prefix="$OUT/pg" --without-readline --without-zlib \
       --without-icu --disable-nls >configure.log 2>&1 \
  && make -j"$(nproc)" -s >build.log 2>&1 \
  && make install -s >install.log 2>&1 ) \
  || { tail -20 "$BUILD"/postgresql-*/{configure,build,install}.log 2>/dev/null; echo "::error::postgresql 构建失败"; exit 1; }

echo "[deps] 构建 pgvector $PV_VER …"
# 上游 Makefile 默认 OPTFLAGS=-march=native —— 在带 AVX-512 的构建机上
# 会产出仅该机可跑的指令(实测:CREATE EXTENSION 即 SIGILL on 无 AVX-512
# 机器)。制品是发行物,钉回 x86-64 基线(SSE2,全 x64 可跑)。
tar xf "$SRC/pgvector.tar.gz" -C "$BUILD"
( cd "$BUILD/pgvector-$PV_VER" \
  && make OPTFLAGS="-march=x86-64" PG_CONFIG="$OUT/pg/bin/pg_config" >build.log 2>&1 \
  && make install OPTFLAGS="-march=x86-64" PG_CONFIG="$OUT/pg/bin/pg_config" >install.log 2>&1 ) \
  || { tail -25 "$BUILD"/pgvector-*/{build,install}.log 2>/dev/null; echo "::error::pgvector 构建失败"; exit 1; }
# 旗标belt(#308):命令行 OPTFLAGS 覆盖生效与否,看真实 gcc 行 ——
# 任何 -march=native 残留都当场红(闸比下游宏探测更贴源头)。
if grep -q -- "-march=native" "$BUILD"/pgvector-*/build.log "$BUILD"/pgvector-*/install.log; then
  echo "::error::pgvector 构建出现 -march=native(OPTFLAGS 覆盖未生效)"
  exit 1
fi
# 正断言(#309 评审):配方完整性 —— 基线旗标必须在场,不只"没毒"。
grep -q -- "-march=x86-64" "$BUILD"/pgvector-*/build.log || {
  echo "::error::pgvector 构建缺 -march=x86-64(配方漂移?)"; exit 1; }

echo "[deps] 构建 redis $RD_VER(server+cli)…"
tar xf "$SRC/redis.tar.gz" -C "$BUILD"
# redis 全串行,三个原因(全部实测翻车点):
#   1. 默认 MALLOC=jemalloc,不预编 deps 直接编 server 缺 jemalloc.h;
#   2. deps 目标并行不安全(jemalloc/lua 竞态,-j 偶发链接缺件);
#   3. redis-server 与 redis-cli 共享 .o,两目标并行编译互相践踏
#      (server 链接成功、cli 无声失败,make rc=2 且无错误行)。
# 串行共 ~2 分钟,确定性优先。错误分支必须带 make 日志——静默吞掉
# = 排障盲飞。
( cd "$BUILD/redis-$RD_VER" \
  && make -s deps >deps.log 2>&1 \
  && make -s redis-server >server.log 2>&1 \
  && make -s redis-cli >cli.log 2>&1 ) \
  || { tail -25 "$BUILD"/redis-*/{deps,server,cli}.log 2>/dev/null; echo "::error::redis 构建失败"; exit 1; }
cp "$BUILD/redis-$RD_VER/src/redis-server" "$BUILD/redis-$RD_VER/src/redis-cli" "$OUT/"

# 动态依赖冒烟:纯 glibc 承诺的把关(缺 .so = AppImage 跨发行面翻车点)。
for bin in "$OUT/pg/bin/postgres" "$OUT/pg/bin/initdb" "$OUT/redis-server"; do
  bad="$(ldd "$bin" | grep 'not found' || true)"
  [ -z "$bad" ] || { echo "::error::$(basename "$bin") 动态依赖缺口: $bad"; exit 1; }
done

"$OUT/pg/bin/postgres" --version
"$OUT/redis-server" --version

# MANIFEST.deps:#283 AC(制品内版本清单)。源 sha 可复验;构建产物非
# 位级可复现,不承诺二进制 sha(记录件数与体积供体积面审计)。
node -e '
  const fs = require("fs");
  const l = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  const out = process.argv[2];
  const m = {
    builtAt: new Date().toISOString(),
    components: {
      postgresql: {version: l.postgresql.version, sourceSha256: l.postgresql.sha256},
      pgvector:   {version: l.pgvector.version,   sourceSha256: l.pgvector.sha256},
      redis:      {version: l.redis.version,      sourceSha256: l.redis.sha256},
    },
  };
  fs.writeFileSync(out + "/MANIFEST.deps", JSON.stringify(m, null, 2) + "\n");
  console.log(fs.readFileSync(out + "/MANIFEST.deps", "utf8"));
' "$LOCK" "$OUT"

echo "[deps] 产物落盘 $OUT:"
du -sh "$OUT" "$OUT/pg" "$OUT/redis-server" || true
