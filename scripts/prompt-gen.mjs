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
const raw = (f) => readFileSync(dir("packages/prompt/" + f), "utf8");
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

// 共享文件(当前仅 skype 表情指南)双端内容必须一致 —— 生成前断言。
function assertShared() {
  const shared = SERVER.filter(([f]) => DAEMON.some(([g]) => g === f)).map(([f]) => f);
  for (const f of shared) {
    const s = raw(f);
    if (s.includes("§")) throw new Error(`${f}: canonical 源不许出现 § 占位(直接写反引号)`);
  }
}
assertShared();

writeFileSync(
  dir("apps/server-go/internal/agent/prompt_constants.gen.go"),
  header("agent") + emit(SERVER) + "\n",
);
writeFileSync(
  dir("apps/byoa-daemon/internal/daemon/persona_prompts.gen.go"),
  header("daemon") + emit(DAEMON) + "\n",
);
console.log("[prompt-gen] 2 个生成物已写(server 4 常量 / daemon 3 常量)");
