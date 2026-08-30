// domains/og —— GET /api/og?url= 链接预览代理(#122,#117-b):聊天渲染端
// 对 autolink 拉卡片(标题/描述/图)。服务端代理因为浏览器读不了跨源
// HTML;一处 Redis 缓存服务全部客户端;集中执行 size/time/SSRF 安全。
// 行为对齐 749863e og.ts + router /og 段。
package og

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/redis/go-redis/v9"
)

const (
	cachePrefix   = "cumora:og:"
	positiveTTLS  = 7 * 24 * 3600
	negativeTTLS  = 3600
	fetchTimeout  = 6 * time.Second
	maxBytes      = 1024 * 1024
	cacheHeader   = "public, max-age=300"
	fetchUA       = "Mozilla/5.0 (compatible; CumoraBot/1.0; +https://cumora.ai)"
	fetchAccept   = "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5"
	fetchLanguage = "en-US,en;q=0.9"
)

// Result:渲染端卡片形状;键省略式(omitempty 对齐 TS 条件赋值)。
type Result struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
	SiteName    string `json:"siteName,omitempty"`
	// FinalURL:重定向后的落点(短链展开,卡片副标题显示真实主机)。
	FinalURL string `json:"finalUrl,omitempty"`
}

// ogError:用户输入类(4xx)与上游类(502/504)错误,携带 HTTP 状态。
type ogError struct {
	status int
	msg    string
}

func (e *ogError) Error() string { return e.msg }

// 原 handler 工厂的一次性构造(client/resolver)提为包级 var —— 保持
// 跨请求共享连接池的既有语义(#187 批次 8)。
var ogClient = &http.Client{Timeout: fetchTimeout}
var ogResolver = net.DefaultResolver

func Serve(rdb *redis.Client, w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		httpx.WriteError(w, http.StatusBadRequest, "url required")
		return
	}
	result, err := preview(r.Context(), rdb, ogClient, ogResolver, raw)
	if err != nil {
		if oe, ok := err.(*ogError); ok {
			httpx.WriteError(w, oe.status, oe.msg)
			return
		}
		httpx.WriteInternalError(w, r, err)
		return
	}
	// 5min 浏览器/CDN 缓存:同会话的兄弟客户端不重复打 Redis;
	// Redis 仍持有 7 天服务端缓存。
	w.Header().Set("Cache-Control", cacheHeader)
	if result == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"url": raw, "empty": true})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func preview(ctx context.Context, rdb *redis.Client, client *http.Client, resolver *net.Resolver, rawURL string) (*Result, error) {
	normalized, err := validateURLString(rawURL)
	if err != nil {
		return nil, err
	}
	cacheKey := cachePrefix + normalized

	if rdb != nil {
		cached, err := rdb.Get(ctx, cacheKey).Result()
		if err == nil {
			// 负缓存序列化为字面 "null":"查过且无可用"与"没查过"可区分。
			if cached == "null" {
				return nil, nil
			}
			var res Result
			if json.Unmarshal([]byte(cached), &res) == nil {
				return &res, nil
			}
			// 坏条目:当 miss 重取。
		}
	}

	host := urlHost(normalized)
	if err := assertPublicHost(ctx, resolver, host); err != nil {
		return nil, err
	}

	var result *Result
	if fetched, ferr := fetchAndParse(ctx, client, normalized); ferr != nil {
		// 网络/解析失败:短负缓存防打爆上游(协议类错误在此之前已抛)。
		result = nil
	} else {
		result = fetched
	}
	// 无标题也无图 ⇒ 弃卡(空卡比没卡更糟)。
	if result != nil && result.Title == "" && result.Image == "" {
		result = nil
	}

	if rdb != nil {
		payload := "null"
		ttl := negativeTTLS
		if result != nil {
			b, _ := json.Marshal(result)
			payload = string(b)
			ttl = positiveTTLS
		}
		rdb.Set(ctx, cacheKey, payload, time.Duration(ttl)*time.Second)
	}
	return result, nil
}

func validateURLString(raw string) (string, *ogError) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", &ogError{status: http.StatusBadRequest, msg: "invalid url"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", &ogError{status: http.StatusBadRequest, msg: "only http(s) urls are supported"}
	}
	// 去 fragment 提命中率(#section 不改变 OG 元数据)。
	u.Fragment = ""
	u.RawFragment = ""
	return u.String(), nil
}

func urlHost(normalized string) string {
	if u, err := url.Parse(normalized); err == nil {
		return u.Hostname()
	}
	return ""
}

// assertPublicHost:解析后私网/环回/链路本地拒绝(防 rebinding——字面
// 主机名不露私网 IP 的绕道)。IP 字面量直查不走 DNS。
func assertPublicHost(ctx context.Context, resolver *net.Resolver, host string) *ogError {
	blocked := &ogError{status: http.StatusForbidden, msg: "blocked private host"}
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return blocked
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return blocked
		}
		return nil
	}
	addrs, err := resolver.LookupHost(ctx, host)
	if err != nil {
		return &ogError{status: http.StatusBadRequest, msg: "dns lookup failed"}
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && isBlockedIP(ip) {
			return blocked
		}
	}
	return nil
}

// isBlockedIP:TS 清单的等价覆盖——私网(含 172.16-31 与 fc/fd)、环回、
// 链路本地(169.254./fe80:,含云 metadata 169.254.169.254)、全零。
func isBlockedIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

func fetchAndParse(ctx context.Context, client *http.Client, urlStr string) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	// 描述性 UA 提升 OG 命中(部分站点对类 cURL 爬虫给精简页)。
	req.Header.Set("User-Agent", fetchUA)
	req.Header.Set("Accept", fetchAccept)
	req.Header.Set("Accept-Language", fetchLanguage)

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return nil, &ogError{status: http.StatusBadGateway, msg: fmt.Sprintf("upstream %d", res.StatusCode)}
	}
	ct := strings.ToLower(res.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/html") && !strings.Contains(ct, "xhtml") {
		return nil, &ogError{status: http.StatusUnsupportedMediaType, msg: "unsupported content-type: " + orUnknown(ct)}
	}

	// 尺寸有界读:OG 元数据在 <head>,1MB 对任何正经页面都绰绰有余。
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		body = body[:maxBytes]
	}
	html := string(body)

	finalURL := urlStr
	if res.Request != nil && res.Request.URL != nil {
		finalURL = res.Request.URL.String()
	}

	out := &Result{URL: urlStr, FinalURL: finalURL}
	if title := firstNonEmpty(
		pickMetaContent(html, "og:title"),
		pickMetaContent(html, "twitter:title"),
		pickHtmlTitle(html),
	); title != "" {
		out.Title = truncate(decodeEntities(title), 280)
	}
	if desc := firstNonEmpty(
		pickMetaContent(html, "og:description"),
		pickMetaContent(html, "twitter:description"),
		pickMetaName(html, "description"),
	); desc != "" {
		out.Description = truncate(decodeEntities(desc), 500)
	}
	if image := firstNonEmpty(
		pickMetaContent(html, "og:image"),
		pickMetaContent(html, "twitter:image"),
		pickMetaContent(html, "twitter:image:src"),
	); image != "" {
		out.Image = resolveURL(image, finalURL)
	}
	if siteName := pickMetaContent(html, "og:site_name"); siteName != "" {
		out.SiteName = truncate(decodeEntities(siteName), 80)
	}
	return out, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// 截断按 rune(TS .slice 按 UTF-16 码元;标题/描述域 ASCII 为主,超长
// 星面字符的 2x 预算差属展示边界,留档不放大实现)。
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}

var titleRe = regexp.MustCompile(`(?i)<title[^>]*>([^<]+)</title>`)

// metaTagRe:给定键属名(attr = property|name)与 meta 键,匹配
// "键在前 content 在后"与"content 在前键在后"两种属性序;引号双单皆可。
func metaTagRe(attr, prop string) *regexp.Regexp {
	e := regexp.QuoteMeta(prop)
	a := regexp.QuoteMeta(attr)
	return regexp.MustCompile(`(?i)<meta\b[^>]*\b` + a + `\s*=\s*["']` + e + `["'][^>]*\bcontent\s*=\s*["']([^"']*)["']`)
}

func metaTagReversed(attr, prop string) *regexp.Regexp {
	e := regexp.QuoteMeta(prop)
	a := regexp.QuoteMeta(attr)
	return regexp.MustCompile(`(?i)<meta\b[^>]*\bcontent\s*=\s*["']([^"']*)["'][^>]*\b` + a + `\s*=\s*["']` + e + `["']`)
}

func firstMatch(html string, res ...*regexp.Regexp) string {
	for _, re := range res {
		if m := re.FindStringSubmatch(html); m != nil && m[1] != "" {
			return m[1]
		}
	}
	return ""
}

// pickMetaContent:TS 优先级——property 全序(两种属性序)试尽才试 name
// (页面同时带 property=og:x 与 name=og:x 时取 property 的)。
func pickMetaContent(html, prop string) string {
	return firstMatch(html,
		metaTagRe("property", prop), metaTagReversed("property", prop),
		metaTagRe("name", prop), metaTagReversed("name", prop),
	)
}

// pickMetaName:description 兜底专用——只看 name 面。
func pickMetaName(html, name string) string {
	return firstMatch(html, metaTagRe("name", name), metaTagReversed("name", name))
}

func pickHtmlTitle(html string) string {
	if m := titleRe.FindStringSubmatch(html); m != nil && m[1] != "" {
		return m[1]
	}
	return ""
}

// resolveURL:og:image 常为相对路径,按最终响应 URL 绝对化;解析失败
// 原样返回(破图好过没卡)。
func resolveURL(href, base string) string {
	b, err := url.Parse(base)
	if err != nil {
		return href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return href
	}
	return b.ResolveReference(ref).String()
}

// decodeEntities:meta 常用实体最小解码集(其余原样透传)。
func decodeEntities(s string) string {
	r := strings.NewReplacer(
		"&amp;", "&", "&quot;", `"`, "&#39;", "'",
		"&lt;", "<", "&gt;", ">", "&nbsp;", " ",
	)
	s = r.Replace(s)
	s = numericEntityRe.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "&#x") || strings.HasPrefix(m, "&#X") {
			if n, err := strconv.ParseInt(m[3:len(m)-1], 16, 32); err == nil {
				return string(rune(n))
			}
			return m
		}
		if n, err := strconv.Atoi(m[2 : len(m)-1]); err == nil {
			return string(rune(n))
		}
		return m
	})
	return s
}

var numericEntityRe = regexp.MustCompile(`&#(?:[0-9]+|[xX][0-9a-fA-F]+);`)
