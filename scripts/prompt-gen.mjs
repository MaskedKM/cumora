#!/usr/bin/env node
// prompt-gen —— 人格/规则文案的单一源生成器(#266)。
//
// canonical 源:packages/prompt/*.txt(真实反引号,人可直接编辑)。
// 生成物(勿手改,contract-check 守漂移):
//   apps/server-go/internal/agent/prompt_constants.gen.go
//   apps/byoa-daemon/internal/daemon/persona_prompts.gen.go
// Go 原始字符串字面量不能内嵌反引号 → 生成时把 ` 转回 § 占位,
// 消费侧 untick()/tick() 还原(与手写时代完全一致,字节往返无损)。
// TS 源退役(#70)后"以 TS 为同步基准"的注释已失效——本目录即唯一真相。
//
// 组装逻辑(globalRules 拼接、standingPrompt 顺序)仍住手写 Go:
// 生成器只做纯文本搬运,不承载业务。

import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const dir = (f) => join(root, f);
// CRLF 归一:编辑器/平台差异不得把 \r 烤进常量字节(§ 占位与内容一样逐字节
// 进入生成物,漂移只许来自显式编辑)。
const raw = (f) => readFileSync(dir("packages/prompt/" + f), "utf8").replaceAll("\r\n", "\n");
const esc = (s) => s.replaceAll("`", "§");

// 文件名 → [生成 Go 常量名, 所属目标]
const SERVER = [
  ["agent-voice.txt", "agentVoiceRulesRaw"],
  ["skype-emoticons-guide.txt", "skypeEmoticonsGuideRaw"],
  ["rules-head.txt", "rulesHeadRaw"],
  ["rules-tail.txt", "rulesTailRaw"],
];
const DAEMON = [
  ["glance-yield-rules.txt", "glanceYieldRulesRaw"],
  ["skype-emoticons-guide.txt", "skypeEmoticonsGuideRaw"],
  ["two-domain-privacy-rule.txt", "twoDomainPrivacyRuleRaw"],
];

const header = (pkg) => `// ${pkg} 包 prompt 文案生成物 —— 由 scripts/prompt-gen.mjs 从
// packages/prompt/*.txt 生成,勿手改(改文案=改 txt + npm run prompt:gen;
// contract-check 拦漂移)。§ 是反引号占位,消费侧 untick/tick 还原。
package ${pkg}

`;

function emit(entries) {
  return entries
    .map(([f, name]) => `// ${name} ← packages/prompt/${f}
const ${name} = \`${esc(raw(f))}\``)
    .join("\n\n");
}

// canonical 源(全部六份,不限双端共享的)不许出现字面 §——esc() 只做
// `→§ 单向替换,txt 里的字面 § 会原样进入生成物,被消费侧 untick/tick
// 还原成反引号,静默篡改 prompt 内容且无任何守卫会响(P1 修复:原
// assertShared 拦错了范围,只在双端共享文件上检查)。
function assertNoSectionSign(files) {
  for (const f of files) {
    if (raw(f).includes("§")) {
      throw new Error(`${f}: canonical 源不许出现 § 占位(直接写反引号)`);
    }
  }
}
assertNoSectionSign([...new Set([...SERVER, ...DAEMON].map(([f]) => f))]);

writeFileSync(
  dir("apps/server-go/internal/agent/prompt_constants.gen.go"),
  header("agent") + emit(SERVER) + "\n",
);
writeFileSync(
  dir("apps/byoa-daemon/internal/daemon/persona_prompts.gen.go"),
  header("daemon") + emit(DAEMON) + "\n",
);
console.log("[prompt-gen] 2 个生成物已写(server 4 常量 / daemon 3 常量)");
