// daemon 包 failclass —— #262 失败重试分类学(里程碑一:daemon 本地)。
// 分类驱动两件事:① retryable 白名单(network / engine-crash /
// engine-timeout)→ 本地退避重试,attempt 上限默认 2;② ResumeSafe 二分
// —— context-overflow 类会话已不可续,强制弃 resume 换新会话;credential
// / bad-request 不重试,只带分类标上报(等操作者处置)。
// 里程碑二(服务端定期扫描失败未决的兜底重排)不在本文件,见 #262。
package daemon

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type failureClass string

const (
	fcNetwork         failureClass = "network"
	fcEngineCrash     failureClass = "engine-crash"
	fcEngineTimeout   failureClass = "engine-timeout"
	fcContextOverflow failureClass = "context-overflow"
	fcCredential      failureClass = "credential"
	fcBadRequest      failureClass = "bad-request"
	fcUnknown         failureClass = "unknown"
)

type turnFailure struct {
	Class      failureClass
	Retryable  bool // 白名单内 → 本地退避重试
	ResumeSafe bool // false = 下一尝试必须弃会话换新(engine-timeout 的杀进程
	// 路径本身 resume 安全——transcript 还在盘上;context-overflow 不安全)
}

// classifyTurnFailure:对失败 turn 的 Err/ExitCode 做标记匹配。匹配顺序
// 即优先级:具体类(凭证/上下文/坏请求)先于一般类(网络),engine-timeout
// (watchdog/墙钟的 124)先于网络(超时字样两义)。
//
// 误报方向纪律(评审 #276 定):分类输入混有引擎 stdout/stderr 尾巴与
// 模型错误正文——① 裸数字子串("401"/"503")会命中 "retry after 503ms"
// 之类正文,数字一律带语境锚定("http 5xx"/"error 5xx"/空格分词);
// ② 不可逆动作(resume-unsafe 弃会话)的类别(context-overflow)只认
// 高特异短语;③ 可重试类(network)误报方向是多一次无害重试,允许宽松词。
// 全不中 → engine-crash(进程非零退出)/unknown。成功态不该进来。
func classifyTurnFailure(res RunResult) turnFailure {
	s := strings.ToLower(res.Err)
	switch {
	case containsAny(s,
		// 凭证类:只认高特异形态(裸 401/403 会误报成正文数字)。
		"invalid api key", "api key not valid", "incorrect api key",
		"unauthorized", "authentication", "credential", "expired token",
		"forbidden", "http 401", "http 403", "error 401", "error 403",
		"status 401", "status 403", " 401 ", " 403 ", "(401)", "(403)"):
		return turnFailure{Class: fcCredential, Retryable: false, ResumeSafe: true}
	case containsAny(s,
		// 上下文溢出:唯一触发不可逆弃会话的类——只认引擎自述的上下文
		// 短语("exceeds the maximum" 这类泛语会误伤文件大小等正文)。
		"context window", "context length", "context_length_exceeded",
		"prompt is too long", "context overflow", "context limit",
		"too many tokens", "maximum context", "token limit",
		"conversation too long", "input too long", "compaction failed"):
		return turnFailure{Class: fcContextOverflow, Retryable: true, ResumeSafe: false}
	case containsAny(s,
		"invalid request", "invalid_request", "malformed",
		"not found (404)", "unknown parameter", "unexpected role",
		"unsupported", "bad request"):
		return turnFailure{Class: fcBadRequest, Retryable: false, ResumeSafe: true}
	case res.ExitCode == 124 || containsAny(s, "idle watchdog", "turn_timeout_ms"):
		return turnFailure{Class: fcEngineTimeout, Retryable: true, ResumeSafe: true}
	case containsAny(s,
		// 网络:误报方向=多一次无害重试,可宽松;数字仍带锚。
		"econnrefused", "connection refused", "connection reset",
		"connection error", "socket hang up", "etimedout", "timeout",
		"timed out", "deadline exceeded", "fetch failed", "network",
		"overloaded", "service unavailable", "bad gateway",
		"temporarily", "proxy", "tunnel", "unexpected eof", "epipe",
		"stream error", "rate limit", "http 5", "error 5", "status 5",
		" 502 ", " 503 ", " 504 ", " 529 ", "(502)", "(503)", "(504)", "(529)"):
		return turnFailure{Class: fcNetwork, Retryable: true, ResumeSafe: true}
	case res.ExitCode != 0:
		return turnFailure{Class: fcEngineCrash, Retryable: true, ResumeSafe: true}
	}
	return turnFailure{Class: fcUnknown, Retryable: false, ResumeSafe: true}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// maxTurnRetries:CUMORA_TURN_MAX_RETRIES,默认 1(总尝试 = 1 + 1 次)。
func maxTurnRetries() int {
	if v := os.Getenv("CUMORA_TURN_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 5 {
			return n
		}
	}
	return 1
}

// turnRetryBackoff:CUMORA_TURN_RETRY_BACKOFF_MS,默认 5s(按 attempt 线性
// 递增:5s、10s、…——聊天型 turn 的网络抖动典型秒级恢复)。
func turnRetryBackoff() time.Duration {
	def := 5 * time.Second
	if v := os.Getenv("CUMORA_TURN_RETRY_BACKOFF_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return def
}
