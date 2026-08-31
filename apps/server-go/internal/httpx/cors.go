// httpx/cors —— 跨源中间件,平价移植 TS index.ts 的 CORS 段(#70 退役
// 时随 server/src 删除、Go 侧一直未补;打包桌面端 app://cumora 全新
// 登录被浏览器 CORS 拦死后补齐,2026-08-31)。语义对齐:
//   - 仅当请求带 Origin 且命中允许集时加响应头;同源(Vite 代理/同源
//     静态部署)不带 Origin,零影响。
//   - 不启用 credentials:认证是 Bearer 头不是 cookie,`*` 通配时按
//     CORS 规范本就禁 credentials。
//   - OPTIONS 预检 204 直接收口,不进路由/认证。
//   - 允许集在中间件构造时求值一次(进程内),与 TS env 启动时求值平价。
package httpx

import (
	"net/http"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
)

// 桌面打包端(Electron app:// 协议,window.cjs 固定 host cumora)的源。
// 本产品自托管单机、桌面优先——自家客户端源内建放行,免得每次全新
// 部署都要在 env 里补一行才能登录(2026-08-31 事故根因之一)。
const desktopOrigin = "app://cumora"

const corsMethods = "GET,POST,PUT,PATCH,DELETE,OPTIONS"

// 与 TS 逐字平价(content-type/authorization 认证与媒体类型;x-company-id
// 租户切换;x-cumora-dev-mode 开发者模式)。
const corsHeaders = "content-type,authorization,x-company-id,x-cumora-dev-mode"

// CORS 中间件:全局挂载(TS 时代 app.use 在全部路由之前,覆盖 /api、
// /uploads、/runtime 等;非浏览器客户端不带 Origin,零影响)。
func CORS() func(http.Handler) http.Handler {
	allowed := map[string]bool{desktopOrigin: true}
	for _, o := range config.CORSOrigins() {
		allowed[o] = true
	}
	any := allowed["*"]
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && (any || allowed[origin]) {
				if any {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", corsMethods)
				w.Header().Set("Access-Control-Allow-Headers", corsHeaders)
				w.Header().Set("Access-Control-Max-Age", "600")
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
