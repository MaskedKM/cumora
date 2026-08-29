// ci/fetch-tree — 源码拉取的最终兜底(#166)。
//
// 背景:ser8 出口上游对长连接/持续大传输不友好——git upload-pack 流
// (无界)和 14MB tarball 单流都会中途停滞(2026-08-29 06:51 UTC 取证:
// 每条连接送 1–3MB 后死,h2 framing 层报错),但短连接小 GET 稳定。
// 本工具把源码拉取切成 714 条短连接:一次 trees API 调用列出全树,
// 再经 raw.githubusercontent.com(CDN,无 API 限流)并行逐文件取回。
// 仓库形态:714 blob / 中位 7.5KB / P95 76KB / 最大 1.4MB——全部
// 落在短连接安全区内。
//
// 设计要点:
//   - 仅 stdlib;两个容器(golang:1.24-alpine / bookworm)都有 go。
//   - 强制 HTTP/1.1(该代理的 h2 framing 实测损坏);代理取小写
//     https_proxy 等环境变量(net/http 默认行为)。
//   - 每文件 3 次重试、45s 整体超时;失败文件计数,>0 即退出非零。
//   - 保留可执行位(100755)与符号链接(120000,raw 返回目标路径)。
//   - 调用方负责先清空工作区;本工具只写不擦。
package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type entry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type tree struct {
	Tree      []entry `json:"tree"`
	Truncated bool    `json:"truncated"`
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[fetch-tree] "+format+"\n", a...)
	os.Exit(1)
}

func client() *http.Client {
	tr := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: false,
		// 置空 NextProto 协商 = 锁死 HTTP/1.1
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		DialContext:  (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
	}
	return &http.Client{Timeout: 45 * time.Second, Transport: tr}
}

// get 带 3 次重试;返回 nil 表示放弃。
func get(c *http.Client, u string) []byte {
	for try := 1; try <= 3; try++ {
		resp, err := c.Get(u)
		if err == nil {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr == nil && resp.StatusCode == 200 {
				return body
			}
			err = fmt.Errorf("status=%d read=%v", resp.StatusCode, rerr)
		}
		fmt.Fprintf(os.Stderr, "[fetch-tree] %s 第%d次失败: %v\n", u, try, err)
		time.Sleep(time.Duration(try) * time.Second)
	}
	return nil
}

// 大文件(>512KB)是微缩版单流问题:整文件 GET 同样会被掐。
// raw CDN 支持 Range(实测 206),按 256KB 分块并行取,每块独立重试。
const (
	chunkThreshold = 512 << 10
	chunkSize      = 256 << 10
)

func getBig(c *http.Client, u string, total int64) []byte {
	buf := make([]byte, total)
	n := (total + chunkSize - 1) / chunkSize
	var wg sync.WaitGroup
	var failed int64
	sem := make(chan struct{}, 6)
	for i := int64(0); i < n; i++ {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			lo := i * chunkSize
			hi := lo + chunkSize - 1
			if hi > total-1 {
				hi = total - 1
			}
			var err error
			for try := 1; try <= 3; try++ {
				req, rerr := http.NewRequest("GET", u, nil)
				if rerr != nil {
					err = rerr
					break
				}
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", lo, hi))
				resp, derr := c.Do(req)
				if derr == nil {
					b, rderr := io.ReadAll(resp.Body)
					resp.Body.Close()
					if rderr == nil {
						if resp.StatusCode == 206 && int64(len(b)) == hi-lo+1 {
							copy(buf[lo:], b)
							return
						}
						// 服务器无视 Range 回了整文件:整块接受
						if resp.StatusCode == 200 && int64(len(b)) == total {
							copy(buf, b)
							return
						}
						derr = fmt.Errorf("chunk#%d status=%d len=%d", i, resp.StatusCode, len(b))
					} else {
						derr = fmt.Errorf("chunk#%d read=%v", i, rderr)
					}
				}
				err = derr
				time.Sleep(time.Duration(try) * time.Second)
			}
			fmt.Fprintf(os.Stderr, "[fetch-tree] %s chunk#%d 放弃: %v\n", u, i, err)
			atomic.AddInt64(&failed, 1)
		}()
	}
	wg.Wait()
	if failed > 0 {
		return nil
	}
	return buf
}

// urlPath 逐段转义,保留 / 结构。
func urlPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func main() {
	if len(os.Args) != 4 {
		die("用法: fetch-tree <owner/repo> <sha> <dest>")
	}
	repo, sha, dest := os.Args[1], os.Args[2], os.Args[3]
	c := client()

	rawBase := envOr("FETCH_TREE_RAW_BASE", "https://raw.githubusercontent.com")
	apiBase := envOr("FETCH_TREE_API_BASE", "https://api.github.com")

	body := get(c, fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", apiBase, repo, sha))
	if body == nil {
		die("trees API 拉取失败(网络或匿名限流)")
	}
	var tr tree
	if err := json.Unmarshal(body, &tr); err != nil {
		die("trees JSON 解析失败: %v", err)
	}
	if tr.Truncated {
		die("树被截断(>100k 条目)——需要分页改造")
	}

	blobs, submods := 0, 0
	for _, e := range tr.Tree {
		switch e.Type {
		case "blob":
			blobs++
		case "commit":
			submods++
		}
	}
	if submods > 0 {
		die("树含 %d 个子模块,本工具不支持", submods)
	}
	fmt.Printf("[fetch-tree] %d 条目 / %d 文件\n", len(tr.Tree), blobs)

	var failed, done int64
	sem := make(chan struct{}, 12)
	var wg sync.WaitGroup
	for _, e := range tr.Tree {
		if e.Type != "blob" {
			continue
		}
		e := e
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			u := fmt.Sprintf("%s/%s/%s/%s", rawBase, repo, sha, urlPath(e.Path))
			var b []byte
			if e.Size > chunkThreshold {
				b = getBig(c, u, e.Size)
			} else {
				b = get(c, u)
			}
			if b == nil {
				atomic.AddInt64(&failed, 1)
				return
			}
			p := filepath.Join(dest, e.Path)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				die("mkdir %s: %v", p, err)
			}
			if e.Mode == "120000" {
				os.Remove(p)
				if err := os.Symlink(string(b), p); err != nil {
					die("symlink %s: %v", p, err)
				}
			} else {
				mode := os.FileMode(0o644)
				if e.Mode == "100755" {
					mode = 0o755
				}
				if err := os.WriteFile(p, b, mode); err != nil {
					die("write %s: %v", p, err)
				}
			}
			if n := atomic.AddInt64(&done, 1); n%100 == 0 {
				fmt.Printf("[fetch-tree] %d/%d\n", n, blobs)
			}
		}()
	}
	wg.Wait()
	if failed > 0 {
		die("%d 个文件放弃(重试穷尽)", failed)
	}
	fmt.Printf("[fetch-tree] 完成: %d 文件\n", done)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
