package daemon

import "testing"

// 分类表钉死:匹配优先级(具体类先于一般类)与 retryable/resume 语义。
func TestClassifyTurnFailure(t *testing.T) {
	cases := []struct {
		name       string
		res        RunResult
		wantClass  failureClass
		wantRetry  bool
		wantResume bool
	}{
		{"watchdog 判死", RunResult{ExitCode: 124, Err: "engine idle watchdog: no engine output in window — aborted"}, fcEngineTimeout, true, true},
		{"墙钟超时", RunResult{ExitCode: 124, Err: "engine turn exceeded CUMORA_TURN_TIMEOUT_MS (60s) — aborted"}, fcEngineTimeout, true, true},
		{"网络拒连", RunResult{ExitCode: 1, Err: "fetch failed: dial tcp: connect: connection refused"}, fcNetwork, true, true},
		{"服务过载", RunResult{ExitCode: 1, Err: "API error 529 overloaded"}, fcNetwork, true, true},
		{"上下文溢出", RunResult{ExitCode: 1, Err: "Prompt is too long: context window exceeded"}, fcContextOverflow, true, false},
		{"压缩失败", RunResult{ExitCode: 1, Err: "compaction failed after retry"}, fcContextOverflow, true, false},
		{"凭证", RunResult{ExitCode: 1, Err: "401 unauthorized: invalid api key"}, fcCredential, false, true},
		{"坏请求", RunResult{ExitCode: 1, Err: "invalid_request_error: unknown parameter"}, fcBadRequest, false, true},
		{"进程崩", RunResult{ExitCode: 2, Err: "process exited with code 2"}, fcEngineCrash, true, true},
		{"无标志非零", RunResult{ExitCode: 1, Err: ""}, fcEngineCrash, true, true},
		// 优先级冲突(评审 #276 P2-3)。
		{"凭证先于上下文", RunResult{ExitCode: 1, Err: "401 unauthorized: context window would exceed"}, fcCredential, false, true},
		{"engine-timeout 先于 network", RunResult{ExitCode: 124, Err: "engine idle watchdog — connection timeout"}, fcEngineTimeout, true, true},
		// 误报免疫(评审 #276 P1-3):裸数字/泛语正文不得触发高特异类。
		{"正文 4013ms 非 credential", RunResult{ExitCode: 1, Err: "retry after 4013ms"}, fcEngineCrash, true, true},
		{"正文 503ms 非 network 裸数字", RunResult{ExitCode: 1, Err: "next poll in 503ms"}, fcEngineCrash, true, true},
		{"泛语 exceeds the maximum 非 overflow", RunResult{ExitCode: 1, Err: "file exceeds the maximum size"}, fcEngineCrash, true, true},
		// unknown 分支。
		{"零退出无错 → unknown 不重试", RunResult{ExitCode: 0, Err: ""}, fcUnknown, false, true},
	}
	for _, c := range cases {
		got := classifyTurnFailure(c.res)
		if got.Class != c.wantClass || got.Retryable != c.wantRetry || got.ResumeSafe != c.wantResume {
			t.Errorf("%s: got class=%s retry=%v resume=%v, want %s/%v/%v",
				c.name, got.Class, got.Retryable, got.ResumeSafe, c.wantClass, c.wantRetry, c.wantResume)
		}
	}
}

func TestMaxTurnRetriesDefault(t *testing.T) {
	t.Setenv("CUMORA_TURN_MAX_RETRIES", "")
	if got := maxTurnRetries(); got != 1 {
		t.Fatalf("默认应为 1(总尝试 2),got %d", got)
	}
	t.Setenv("CUMORA_TURN_MAX_RETRIES", "3")
	if got := maxTurnRetries(); got != 3 {
		t.Fatalf("显式 3 未生效,got %d", got)
	}
	t.Setenv("CUMORA_TURN_MAX_RETRIES", "-1")
	if got := maxTurnRetries(); got != 1 {
		t.Fatalf("越界值应回默认,got %d", got)
	}
}
