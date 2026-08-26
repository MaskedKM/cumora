// daemon 包 shim —— 引擎 PATH 上的 `cumora` 命令:daemon 写进 agent
// home/bin 的一个微型 Node 可执行(引擎经 bash 调用,POST argv 到服务端
// /runtime/cli,JWT 钉死 agent 身份;无 curl/jq 依赖)。文本逐字节对齐
// daemon.ts 的 CUMORA_SHIM / CUMORA_WINDOWS_SHIM(§ 为反引号占位)。
package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const cumoraShimRaw = `#!/usr/bin/env node
'use strict'
;(async () => {
  const url = process.env.CUMORA_AGENT_RUNTIME_URL
  // Prefer a token FILE (refreshed by the daemon) over the env token: a PERSISTENT
  // engine process is spawned once, so its env token goes stale on refresh — the
  // file is always current. Falls back to the env token (one-shot / codex path).
  var token = process.env.CUMORA_AGENT_RUNTIME_TOKEN
  var tokenFile = process.env.CUMORA_AGENT_RUNTIME_TOKEN_FILE
  if (tokenFile) { try { var ft = require('fs').readFileSync(tokenFile, 'utf8').trim(); if (ft) token = ft } catch (e) {} }
  if (!url || !token) { console.error('cumora: runtime env not set'); process.exit(70) }
  var argv = process.argv.slice(2)
  // Shell-safe body input. A reply written inline (cumora reply id "..text..")
  // is mangled by bash BEFORE this shim runs: backticks and $(...) get run as
  // commands and collapse to empty, quotes get eaten. So --file <path> /
  // --stdin let the body come from a file (written by the editor, no shell) or
  // a pipe; we read it LOCALLY and pass it as one argument that travels as
  // JSON and is never re-parsed by a shell, so code/quotes/$ survive verbatim.
  //
  // It goes LAST, behind a POSIX §--§, so the server takes it literally. Spliced
  // in place it was still read as argv: a body starting with §---§ (markdown
  // rule, front-matter fence, diff header) parsed as a FLAG and the message was
  // silently dropped, and escapes inside it were expanded a second time.
  var fs = require('fs')
  var body = null
  var fi = argv.indexOf('--file')
  if (fi >= 0 && argv[fi + 1] !== undefined) {
    try { body = fs.readFileSync(argv[fi + 1], 'utf8') }
    catch (e) { console.error('cumora: cannot read --file ' + argv[fi + 1]); process.exit(70) }
    argv.splice(fi, 2)
  }
  var si = argv.indexOf('--stdin')
  if (si >= 0) {
    argv.splice(si, 1)
    if (body === null) { try { body = fs.readFileSync(0, 'utf8') } catch (e) { body = '' } }
  }
  if (body !== null) argv.push('--', body)
  const res = await fetch(url + '/cli', {
    method: 'POST',
    headers: { 'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json' },
    body: JSON.stringify({ argv: argv }),
  })
  if (!res.ok) {
    const t = await res.text().catch(() => '')
    console.error('cumora: HTTP ' + res.status + ' ' + t)
    process.exit(70)
  }
  const data = await res.json()
  const code = typeof data.exitCode === 'number' ? data.exitCode : 0
  // Exit from the write CALLBACK, not the next statement: stdout on a PIPE is
  // ASYNC, so process.exit() kills us with the tail still buffered. The engine
  // always runs this shim with stdout piped, so a big result (cumora messages
  // --tail 30, cumora inbox --json) silently arrived truncated at the pipe
  // buffer — 64KB, or 8KB on the socketpair a stdio:'pipe' parent hands us —
  // with exit 0 and empty stderr, so nothing signalled the loss and --json
  // output simply failed to parse. Exiting IN the callback also keeps a reader
  // that closed early (| head) an exit-0 like before, instead of the unhandled
  // EPIPE crash a bare process.exitCode would produce.
  if (typeof data.text === 'string' && data.text) process.stdout.write(data.text + '\n', () => process.exit(code))
  else process.exit(code)
})().catch((e) => { console.error('cumora:', (e && e.message) || e); process.exit(70) })
`

// cumoraWindowsShim:PowerShell 只经 PATHEXT 解析 PATH 文件,无扩展名的
// POSIX shim 需要 .cmd 启动器;两个启动器跑的是同一个 Node 程序,参数/
// HTTP 路径完全一致。
const cumoraWindowsShimRaw = `@echo off
node "%~dp0cumora" %*
`

// writeShim:把 cumora(Windows 另加 cumora.cmd)写进 binDir 并加执行位。
func writeShim(binDir string) error {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	shim := filepath.Join(binDir, "cumora")
	if err := os.WriteFile(shim, []byte(strings.ReplaceAll(cumoraShimRaw, "§", "`")), 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if err := os.WriteFile(filepath.Join(binDir, "cumora.cmd"), []byte(strings.ReplaceAll(cumoraWindowsShimRaw, "§", "`")), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// prependAgentBinToPath:binDir 前置进 PATH(引擎子进程的环境)。
func prependAgentBinToPath(binDir, currentPath string) string {
	if currentPath == "" {
		return binDir
	}
	return binDir + string(os.PathListSeparator) + currentPath
}
