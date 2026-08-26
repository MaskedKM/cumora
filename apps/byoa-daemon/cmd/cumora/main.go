// cumora —— BYOA agent-daemon CLI(Go 单二进制,#63 骨架)。
// 入口对齐 agent-cli/src/cli.ts:只认 `agent computer …` 子命令面;
// 其余用法打印帮助并以非零退出。
package main

import (
	"fmt"
	"os"

	"github.com/MaskedKM/cumora/apps/byoa-daemon/internal/daemon"
)

func main() {
	argv := os.Args[1:]
	if len(argv) >= 2 && argv[0] == "agent" && argv[1] == "computer" {
		daemon.RunComputerDaemon(argv[2:])
		return
	}
	fmt.Fprint(os.Stderr, daemon.HelpText())
	if len(argv) > 0 {
		os.Exit(1)
	}
}
