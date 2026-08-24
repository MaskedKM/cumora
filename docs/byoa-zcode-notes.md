# BYOA zcode — POC 结论(issue #1)

实测环境:Linux (Ubuntu), ZCode 桌面版 3.8.1(AppImage),CLI runtime `resources/glm/zcode.cjs` 版本 0.16.3,Node v22。测试日期 2026-08-24。

## 无头入口与配置引导

- PATH 上的 `zcode` 是 Electron GUI,无头调用会拉起窗口(Linux 下另有 chrome-sandbox SUID 崩溃)。无头入口 = `node <appdir>/resources/glm/zcode.cjs …`。
- CLI 配置固定读 `~/.zcode/cli/config.json`(`zcode login` 会自动写入)。schema:
  ```jsonc
  {
    "model": { "main": "<providerId>/<modelId>", "lite": "<providerId>/<modelId>" },  // 字符串引用
    "provider": {                                                        // 注意:单数
      "<providerId>": {
        "kind": "anthropic | openai | openai-compatible",
        "options": { "apiKey": "…", "baseURL": "…", "apiKeyRequired": true },
        "models": { /* 模型能力表,可从桌面版 ~/.zcode/v2/config.json 的 provider 条目整段复制 */ }
      }
    }
  }
  ```
- 桌面版已登录 ≠ CLI 就绪:桌面版在进程内传模型配置,不落盘。引导方式二选一:
  1. `zcode login`(OAuth,`--no-browser` 打印 URL);
  2. 从桌面版 `~/.zcode/v2/config.json` 把启用的 provider 条目(含 apiKey/baseURL/models)复制进 `~/.zcode/cli/config.json`,`model.main` 指向该 provider。

## 验证点结论

| # | 验证点 | 结论 |
|---|--------|------|
| 1 | `-p` + `--continue` 组合 | ✅ 跨进程上下文延续(--cwd 相同即恢复该目录最新会话) |
| 2 | 首次运行即带 `--continue` | ❌ 报错 `No resumable session found for <cwd>`,exit 1(已被 `--resume` 方案取代,见下方"补充实测"与修订 2) |
| 3 | `-p` stdout 形态 | ✅ 纯回复文本、无噪音、ANSI 可用 `--no-color` 压制 |
| 4 | persona 文件拾取 | ✅ AGENTS.md 与 CLAUDE.md **都被读取** → seedHome 写 AGENTS.md 即可 |
| 5 | 只读模式 | ✅ `--mode plan` 单独即阻止写文件;`--disallowed-tools "Bash Edit Write"` 亦生效(双保险) |
| 6 | `ZCODE_HOME` 隔离 | ❌ **不隔离**:config 固定读 `~/.zcode/cli/config.json`(移走真实 config 后,设与不设 ZCODE_HOME 均报 Model config is missing)→ 0.16.3 无 per-agent 模型钉死 |
| 7 | `--json` 输出 | ✅✅ **关键升级**:`-p --json` 在 stdout 输出单个 JSON 信封,含 `sessionId` / `response` / `usage`(inputTokens、outputTokens、cacheReadTokens、cacheWriteTokens、reasoningTokens)/ `projection`(上下文水位) |
| 8 | 思考控制 | ❌ 无 CLI flag;`~/.zcode/cli/config.json` 也没有已验证的等价键(thought_level 仅出现在应用内会话配置 schema,CLI config 的 zod schema 无此键)→ 接受默认行为 |

## 补充实测(适配器设计的关键输入)

- `--resume <real-id> --json`:sessionId **跨轮保持不变**;上下文延续;usage 逐轮报告。→ daemon 现成的 `resumeSessionId` 管线原生可用,无需 `--continue` 标记文件。
- `--resume <stale-id>`:干净报错 `Session not found: <id>`,exit 1 → 适配器可检测该错误并**自愈重试**(去掉 `--resume` 重跑,对齐 CodexSession 的 resume→fresh-thread 兜底)。
- 会话存储在操作者级 `~/.zcode/cli/rollout/`,按 cwd 关联——每 agent 独立 home/cwd 保证隔离。
- 帮助文本超前于 arg parser:`--output-format`、`--settings`、`--max-turns` 均为"帮助有、解析器拒"(0.16.3)。适配器要对 `Unknown option` 退出留明确诊断。

## 对适配器方案的修订(相对原 issue 假设)

1. `run()` 用 `-p --json`:解析信封取 `response`(文本)、`sessionId`(续会话)、`usage`(映射到 EngineUsage;**`inputTokens` 已含 cacheRead 部分**——证据:POC 轮 input 21295 / cacheRead 17600 / output 39,`projection.totalTokenCount` = 21334 = 21295+39,即 total = input+output、input 含 cache,与 Anthropic 协议语义一致;故 `input_tokens = inputTokens − cacheReadTokens`,`cacheReadTokens→cache_read_input_tokens`,`cacheWriteTokens→cache_creation_input_tokens`,`outputTokens+reasoningTokens→output_tokens`)。**usage 从 unmeasured 升级为逐轮真实计量**。
2. 会话连续性走 `--resume <sessionId>`(daemon 既有管线),不用 `--continue`;陈旧 id 自愈重试。
3. model 钉死维持 ❌(ZCODE_HOME 不隔离);ledger 的 model 字段如实取 `~/.zcode/cli/config.json` 的 `model.main` 值。
4. classify/probe:`--mode plan` + `--disallowed-tools "Bash Edit Write"`,同样可用 `--json` 拿 usage;小模型不可切(ZCODE_HOME 方案否决)→ 跑默认模型、如实上报。
5. seedHome:`ensureCommonHome` + AGENTS.md(总是重写,persona 编辑要生效)。

## 环境引导备忘(本机复现)

```sh
# 无头入口(AppImage 挂载点随版本漂移)
node /tmp/.mount_ZCode.*/resources/glm/zcode.cjs --cwd <dir> --mode yolo --no-color --json -p "<prompt>"

# 写 CLI 配置(从桌面版 v2 配置复制启用的 provider;apiKey 只在文件间搬运,不打印)
python3 - <<'EOF'
import json, os
home = os.path.expanduser('~')
v2 = json.load(open(os.path.join(home, '.zcode/v2/config.json')))
entry = v2['provider']['builtin:bigmodel-coding-plan']  # 本机启用项,按需替换
cli_path = os.path.join(home, '.zcode/cli/config.json')
cli = json.load(open(cli_path)) if os.path.exists(cli_path) else {}
cli['provider'] = {'bigmodel-coding-plan': entry}
cli['model'] = {'main': 'bigmodel-coding-plan/GLM-5.2', 'lite': 'bigmodel-coding-plan/GLM-5-Turbo'}
with open(cli_path, 'w') as f:
    json.dump(cli, f, indent=2)
EOF
```
