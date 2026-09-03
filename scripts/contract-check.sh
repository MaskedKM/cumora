#!/usr/bin/env bash
# 契约防漂移守卫:#48(OpenAPI-first);#139 起对账面含 server-go 生成物
# (internal/contract = 全量 types + 已迁移 tag 的 ServerInterface);
# #221 起对账面含 WS 事件契约双端生成物(contract/src/ws-events.d.ts +
# server-go internal/events/ws.gen.go),并 grep 拦三端手写事件形状回流;
# #266 起对账面含 prompt 文案生成物(packages/prompt/*.txt → 双端 .gen.go);
# #261b 起含平台技能拷贝物(daemon skillsdata/,源 packages/prompt/skills/)。
# 自举 safe.directory —— CI job 容器内 root 的 git 会报 dubious ownership
# (actions/checkout 的 safe.directory 写在宿主用户配置里,容器 root 看不到)。
set -euo pipefail
cd "$(dirname "$0")/.."
git config --global --add safe.directory "$(pwd)" 2>/dev/null || true
npm run contract:gen
npm run prompt:gen
git add -A -- packages/contract apps/server-go/internal/contract apps/server-go/internal/events/ws.gen.go packages/prompt apps/server-go/internal/agent/prompt_constants.gen.go apps/byoa-daemon/internal/daemon/persona_prompts.gen.go apps/byoa-daemon/internal/daemon/skillsdata
if ! git diff --cached --exit-code HEAD -- packages/contract apps/server-go/internal/contract apps/server-go/internal/events/ws.gen.go packages/prompt apps/server-go/internal/agent/prompt_constants.gen.go apps/byoa-daemon/internal/daemon/persona_prompts.gen.go apps/byoa-daemon/internal/daemon/skillsdata; then
  echo "契约与生成物不同步 —— 跑 npm run contract:gen && npm run prompt:gen 并提交" >&2
  exit 1
fi

# #221 手写漂移守卫:WS 事件形状只许住在契约(packages/contract/ws-events.json)。
# 三端退役面禁止再内联事件 type 字面量/载荷形状 —— 新增事件改契约一处,
# npm run contract:gen 再生三端类型。守卫面与票面定位一致:
#   前端 apps/web/src/api/client.ts(WsEvent union 退役)、
#   TS harness tests/integration/harness/redis.ts(事件接口退役)、
#   Go internal/events/publish.go + internal/wsx(载荷/通道字面量退役)。
# 域包经 PublishRaw 广播的发布点(domains/agent/sched 等)不在本守卫面 ——
# 通道常量与事件名已全部走生成物,载荷字面量的域内收敛留后续票。
ws_handwritten_drift() {
  local file="$1" pattern="$2"
  if grep -nE "$pattern" "$file" >/dev/null; then
    echo "[contract] $file 出现手写 WS 事件形状(改契约 ws-events.json 再生,勿内联):" >&2
    grep -nE "$pattern" "$file" >&2
    return 1
  fi
  return 0
}
drift=0
# TS 侧:事件判别字面量只允许出现在生成物里(比较 e.type === 'x' 的消费窄化
# 不在此形态,不受限)。
# 单双引号都拦(TS 两者皆合法);事件判别字面量只允许出现在生成物里。
# 事件名清单经生成器从契约派生(单词名精确+点分通配)——契约外新事件名
# (dummy.ping 之类)也拦,这正是当年 calendar.reminder/msg.delta 孤儿的成因。
WS_TS_EVENT_LITERALS="$(node scripts/contract-gen-ws.mjs --print-guard-pattern)"
ws_handwritten_drift apps/web/src/api/client.ts "$WS_TS_EVENT_LITERALS" || drift=1
ws_handwritten_drift tests/integration/harness/redis.ts "$WS_TS_EVENT_LITERALS" || drift=1
# yjs-sidecar 的 redis.ts 同面(doc.update/doc.awareness 发布方,#142 内联副本已退役)
ws_handwritten_drift apps/yjs-sidecar/src/infra/redis.ts "$WS_TS_EVENT_LITERALS" || drift=1
# Go 侧:发布面/网关不得再内联事件载荷 map 键或通道/事件名字面量
# (通道引用一律走生成常量 Ch*;msg["type"] 读取形态不匹配本模式,不受限)。
ws_handwritten_drift apps/server-go/internal/events/publish.go '"(type|companyId|conversationId|messageId|participantId|documentId|eventId|actorId)":' || drift=1
for f in apps/server-go/internal/wsx/*.go; do
  case "$f" in *_test.go) continue ;; esac
  ws_handwritten_drift "$f" '"type":' || drift=1
done
if [ "$drift" -ne 0 ]; then
  echo "[contract] WS 事件手写漂移 —— 见上" >&2
  exit 1
fi
echo "[contract] 规范 ↔ 生成物与 HEAD 一致;WS 事件面无手写回流"
