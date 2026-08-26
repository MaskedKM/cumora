// daemon 包 engines —— EngineAdapter 接口与注册表。骨架期不含真实引擎
// (#64 claude/codex、#65 zcode、#66 grok/cursor);协议测试经包内注册
// stub 引擎驱动。探测面对齐 TS:PATH 上找四个引擎可执行文件。
package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
)

// 已知引擎 id(对齐 TS ENGINE_IDS;线上契约用 id——服务端白名单、
// me/agents 的 engine 字段都是这四个值;适配器本体随 #64–#66 落位)。
var EngineIDs = []string{"claude", "codex", "grok", "cursor"}

// engineBin:引擎 id → PATH 可执行名(仅 cursor 的可执行叫 cursor-agent,
// 其余同名——评审 M1:id 与二进制名必须分离,否则 Cursor-only 机器配对
// 报 cursor-agent 被服务端白名单静默回退 claude)。
func engineBin(id string) string {
	if id == "cursor" {
		return "cursor-agent"
	}
	return id
}

// TurnInput:一次 turn 交给引擎的全部上下文(骨架面:会话恢复 + 提示)。
// #64 起扩展为完整提示组装(stream-json/standing prompt)。
type TurnInput struct {
	Agent          AgentInfo
	RuntimeBaseURL string
	RuntimeToken   string
	// ResumeSessionID:非空 = 恢复既有会话(跨轮/跨重启续跑)。
	ResumeSessionID string
	// WakeReason/Prompt:骨架期由 stub 引擎消费。
	WakeReason string
	Prompt     string
}

// TurnResult:引擎回合产物;SessionID 非空则落盘供下轮 resume。
type TurnResult struct {
	SessionID string
	Output    string
	Err       error
}

// EngineAdapter:一个本地引擎。骨架只要求一次性 Run(持久会话模式是
// #64 的 claude/codex 扩展点——接口预留 StartSession 即可平移)。
type EngineAdapter interface {
	ID() string
	Run(ctx context.Context, in TurnInput) TurnResult
}

var (
	enginesMu  sync.Mutex
	adapterReg = map[string]EngineAdapter{}
)

// RegisterAdapter:注册引擎适配器(测试注入 stub 用;真实引擎在
// 各自票里于 init/主函数注册)。
func RegisterAdapter(a EngineAdapter) {
	enginesMu.Lock()
	defer enginesMu.Unlock()
	adapterReg[a.ID()] = a
}

func getAdapter(id string) EngineAdapter {
	enginesMu.Lock()
	defer enginesMu.Unlock()
	return adapterReg[id]
}

// detectLocalEngines:PATH 探测(对齐 requireLocalEngine 的探测面)。
// 返回找到的引擎 id 列表(保 EngineIDs 序)。
func detectLocalEngines() []string {
	var out []string
	for _, id := range EngineIDs {
		if _, err := exec.LookPath(engineBin(id)); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// requireLocalEngine:无任何引擎时与 TS 同错(退出码 70 由调用方落)。
func requireLocalEngine() ([]string, error) {
	engines := detectLocalEngines()
	if len(engines) == 0 {
		return nil, fmt.Errorf("no supported local agent engine found on PATH")
	}
	return engines, nil
}
