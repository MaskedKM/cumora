// engdirs —— BYOA 引擎可执行文件的发现目录(#281 doctor 与 #282
// stackd 的 daemon 子进程 PATH 钉扎同源;fresh-boot 用户管理器不带
// nvm PATH,cumora-daemon.service 时代靠手钉 —— 现在两处共用这一份)。
package engdirs

import (
	"os"
	"path/filepath"
)

// Dirs —— 引擎发现目录:nvm 各版本 bin、npx 缓存 bin、常见用户级 bin。
// home 为空时用当前用户家目录。
func Dirs(home string) []string {
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return nil
	}
	var dirs []string
	for _, pattern := range []string{
		filepath.Join(home, ".nvm/versions/node/*/bin"),
		filepath.Join(home, ".npm/_npx/*/node_modules/.bin"),
	} {
		if matches, err := filepath.Glob(pattern); err == nil {
			dirs = append(dirs, matches...)
		}
	}
	return append(dirs,
		filepath.Join(home, ".local/bin"),
		filepath.Join(home, ".bun/bin"),
	)
}
