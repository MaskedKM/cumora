// agent 包 prompt_constants —— GLOBAL_RULES 的组装层。文案常量本体在
// prompt_constants.gen.go:由 packages/prompt/*.txt 经 scripts/prompt-gen.mjs
// 生成,勿手改(改文案 = 改 txt + npm run prompt:gen;contract-check 拦漂移)。
// TS 源已退役(#70),packages/prompt 即唯一 canonical 源(#266)。
// § 是反引号占位(Go 原始字符串不能内嵌反引号);untick 在此还原。
package agent

import "strings"

func untick(s string) string { return strings.ReplaceAll(s, "§", "`") }

var (
	agentVoiceRules = untick(agentVoiceRulesRaw)

	// globalRules:GLOBAL_RULES 成品。head 尾部与 tail 头部各自携带模板里
	// 的 \n\n 定界,不再额外加分隔。#261b:skype 表情包指南技能化
	// (skills/cumora-conversation-style),不再逐唤醒内联。
	globalRules = untick(rulesHeadRaw) + agentVoiceRules + "\n\n" +
		untick(rulesTailRaw)
)
