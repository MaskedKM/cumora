// Package webapp —— 静态托管(#69 切换日补齐):TS index.ts 在同端口兼供
// SPA(生产 dist/)与 /uploads 本地上传目录,Go 此前仅 API,全量切流前
// 必须补上同面,否则前端与上传图片全断。语义逐条对齐 index.ts 108–207:
//
//	/uploads:本地模式目录服务(1h 缓存、恒 nosniff、非预览位图一律
//	         attachment 防主动内容借源读令牌、缺文件 JSON 404 不下探)
//	SPA:生产且有 dist/index.html 时,未匹配 GET 先找 dist 静产(1h),
//	     否则回退 index.html(no-cache);/api|/runtime|/uploads|/ws 前缀
//	     永不回退。无 dist(开发)时 / 给信息 JSON,Vite 在 :5173 供页。
//
// Go 现仅支持本地上传模式(presign 501),故 /uploads 无条件挂载;
// 未来引入 R2 时须按存储模式对齐 TS 的 storage.mode 门。
package webapp

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func Mount(mux *http.ServeMux) {
	cwd, _ := os.Getwd()
	// #208:uploads 根统一走 config.UploadsDir()(CUMORA_UPLOADS_DIR >
	// 旧键 UPLOAD_DIR > cwd 相对 server/uploads)——与写侧同一解析点,
	// 设 env 后上传/读取不再精神分裂。
	uploadDir := config.UploadsDir()
	distDir := filepath.Join(cwd, "dist")
	indexPath := filepath.Join(distDir, "index.html")
	_, indexErr := os.Stat(indexPath)
	hasDist := config.IsProduction() && indexErr == nil

	mux.HandleFunc("GET /uploads/", serveUpload(uploadDir))

	// 根兜底必须注册为全方法 "/":若写 "GET /" 会与 "/api/" 这类
	// 全方法前缀模式形成"路径更具体 vs 方法更具体"的互不包含冲突
	// (ServeMux 直接 panic)。方法判定移入 handler。
	serveRoot := serveSPA(distDir, indexPath)
	if !hasDist {
		serveRoot = func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				httpx.WriteError(w, http.StatusNotFound, "not found")
				return
			}
			// TS 只挂 app.get('/'):开发形态仅根路径给信息 JSON,
			// 其余 GET 落 404(Express 默认),不整站 200。
			if r.URL.Path != "/" {
				httpx.WriteError(w, http.StatusNotFound, "not found")
				return
			}
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"name": "cumora", "instance": instanceID(),
				"spa": "dev (served by vite)",
			})
		}
	}
	mux.HandleFunc("/", serveRoot)
}

// instanceID:对齐 TS env.ts 的默认值形状 app-<rand5>(未设 INSTANCE_ID
// 时逐进程随机;仅信息 JSON 展示,无消费方依赖具体值)。
func instanceID() string {
	if id := config.InstanceIDEnv(); id != "" {
		return id
	}
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "app-" + hex.EncodeToString(b)[:5]
}

// serveUpload:/uploads/<name> 目录服务。路径安全:清洗后必须仍落在
// uploadDir 内(防 ../ 逃逸);ServeFile 的内建防线之外多一道显式校验。
// 缺文件走 fallthrough:false 语义直达 404;文案 "Not Found" 对齐 TS
// 错误处理器(err.message),非 API 面通用的 "not found"。
func serveUpload(uploadDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/uploads/")
		full := filepath.Join(uploadDir, filepath.FromSlash(name))
		if full != uploadDir && !strings.HasPrefix(full, uploadDir+string(filepath.Separator)) {
			httpx.WriteError(w, http.StatusNotFound, "Not Found")
			return
		}
		fi, err := os.Stat(full)
		if err != nil || fi.IsDir() {
			httpx.WriteError(w, http.StatusNotFound, "Not Found")
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// 只有可预览位图允许内联渲染;其余(HTML/SVG/PDF…)一律下载,
		// 防止借应用源执行主动内容偷 localStorage 会话令牌。
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(full), "."))
		switch ext {
		case "png", "jpg", "jpeg", "webp", "gif":
		default:
			w.Header().Set("Content-Disposition", "attachment")
		}
		http.ServeFile(w, r, full)
	}
}

// serveSPA:先按 dist 静产命中(哈希件 1h;index.html 直访走 no-cache),
// 未命中回退 index.html(no-cache,客户端路由刷新可用)。API 族前缀
// 到此说明域内未匹配——保持 404,绝不回退页面。仅 GET/HEAD(TS 的
// SPA 回退是 app.get;其余方法落到 JSON 404)。
func serveSPA(distDir, indexPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			httpx.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		for _, p := range []string{"api", "runtime", "uploads", "ws"} {
			if r.URL.Path == "/"+p || strings.HasPrefix(r.URL.Path, "/"+p+"/") {
				httpx.WriteError(w, http.StatusNotFound, "not found")
				return
			}
		}
		clean := filepath.Clean("/" + r.URL.Path)
		cand := filepath.Join(distDir, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
		if strings.HasPrefix(cand, distDir+string(filepath.Separator)) {
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				if strings.HasSuffix(cand, "index.html") {
					w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
				} else {
					w.Header().Set("Cache-Control", "public, max-age=3600")
				}
				http.ServeFile(w, r, cand)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		http.ServeFile(w, r, indexPath)
	}
}
