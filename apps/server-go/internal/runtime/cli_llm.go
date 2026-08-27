// /runtime/cli LLM lite 客户端(#89):llm.ts 的 Go 等价路由(sub2api
// 租户键 → legacy OPENAI_API_KEY)+ images.generate(generateAndUploadImage
// 的引擎面)。Responses/Chat-Completions 客户端随 remote-classify 落地。
package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// cliLlmEndpoint:getLlmClient 的路由决策 —— sub2api 已配置且能解析出租
// 户 owner 的 sub2api_api_key → 该键 + sub2api 的 OpenAI 兼容 base;否
// 则 legacy(OPENAI_API_KEY @ api.openai.com)。解析失败绝不致命,退回
// legacy(sub2api 卡死不得拖垮 agent turn)。
func (s *Service) cliLlmEndpoint(ctx context.Context, tenant string) (baseURL, apiKey string) {
	baseURL, apiKey = "https://api.openai.com/v1", os.Getenv("OPENAI_API_KEY")
	if tenant == "" || os.Getenv("SUB2API_INTERNAL_URL") == "" || os.Getenv("SUB2API_ADMIN_KEY") == "" {
		return baseURL, apiKey
	}
	var key sql.NullString
	err := s.DB.QueryRowContext(ctx,
		`SELECT u.sub2api_api_key
		   FROM companies c
		   JOIN users u ON u.id = c.owner_user_id
		  WHERE c.id = $1`, tenant).Scan(&key)
	if err != nil || !key.Valid || key.String == "" {
		return baseURL, apiKey // 租户未配 key → legacy
	}
	internal := strings.TrimRight(os.Getenv("SUB2API_INTERNAL_URL"), "/")
	public := strings.TrimRight(os.Getenv("SUB2API_PUBLIC_URL"), "/")
	base := internal
	if base == "" {
		base = public
	}
	if base == "" {
		return baseURL, apiKey
	}
	return base + "/v1", key.String
}

var httpClientLLM = &http.Client{Timeout: 5 * time.Minute}

// cliImagesGenerate:images.generate 的直接 HTTP 形态(n 恒 1;响应取
// data[0].b64_json,缺则按 data[0].url 回取)。仅图像模型;Novita 前缀
// 只影响 responses.create,images 透传(TS 同)。
func (s *Service) cliImagesGenerate(ctx context.Context, tenant, model, prompt, size string) ([]byte, error) {
	baseURL, apiKey := s.cliLlmEndpoint(ctx, tenant)
	reqBody, _ := json.Marshal(map[string]any{
		"model":  model,
		"prompt": prompt,
		"size":   size,
		"n":      1,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/images/generations", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+apiKey)
	resp, err := httpClientLLM.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("images API %d: %s", resp.StatusCode, truncateRunesSimple(string(body), 500))
	}
	var parsed struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("image API returned no data")
	}
	if parsed.Data[0].B64JSON != "" {
		return base64.StdEncoding.DecodeString(parsed.Data[0].B64JSON)
	}
	if parsed.Data[0].URL != "" {
		fetch, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.Data[0].URL, nil)
		if err != nil {
			return nil, err
		}
		fr, err := httpClientLLM.Do(fetch)
		if err != nil {
			return nil, err
		}
		defer fr.Body.Close()
		if fr.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("image fetch %d", fr.StatusCode)
		}
		return io.ReadAll(io.LimitReader(fr.Body, 64<<20))
	}
	return nil, fmt.Errorf("image API returned no data")
}

func truncateRunesSimple(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

var imageSizeMap = map[string]string{
	"square": "1024x1024",
	"wide":   "1536x1024",
	"tall":   "1024x1536",
}

var slugStripRe = regexp.MustCompile(`[^a-z0-9\s-]+`)
var slugSplitRe = regexp.MustCompile(`\s+`)

// cliGenerateAndUploadImage:generateAndUploadImage 等价 —— 图像模型 →
// 存储 → 附件对象(prompt slug 做友好文件名)。台账:agent-image 用途,
// 无 token 用量,extras 记 n/size/promptPreview(图像单价表未播种时
// cost_estimated=true,是已知缺口)。
func (s *Service) cliGenerateAndUploadImage(prompt, size, tenant, agentID string) (agentAttachment, error) {
	dims := imageSizeMap[size]
	if dims == "" {
		dims = "1024x1024"
	}
	model := os.Getenv("OPENAI_IMAGE_MODEL")
	if model == "" {
		model = "gpt-image-1"
	}
	t0 := time.Now()
	agentIDArg := agentID
	tenantArg := tenant
	record := func(status string, errMsg *string) {
		s.RecordLlmCall(LlmCallRecord{
			Purpose:   "agent-image",
			CompanyID: &tenantArg,
			AgentID:   &agentIDArg,
			Source:    "cloud",
			Model:     model,
			LatencyMS: time.Since(t0).Milliseconds(),
			Status:    status,
			Error:     errMsg,
			Extras: map[string]any{
				"n": 1, "size": size,
				"promptPreview": truncateRunesSimple(prompt, 120),
			},
		})
	}
	buf, err := s.cliImagesGenerate(context.Background(), tenant, model, prompt, dims)
	if err != nil {
		msg := err.Error()
		record("failed", &msg)
		return agentAttachment{}, err
	}
	record("ok", nil)
	key := "attachments/" + randHex32() + ".png"
	url, err := cliStoragePut(key, buf)
	if err != nil {
		return agentAttachment{}, err
	}
	// prompt slug → 气泡标题的友好文件名。
	slug := slugStripRe.ReplaceAllString(strings.ToLower(prompt), "")
	slug = strings.TrimSpace(slug)
	parts := slugSplitRe.Split(slug, -1)
	if len(parts) > 5 {
		parts = parts[:5]
	}
	name := truncateRunesSimple(strings.Join(parts, "-"), 40)
	if name == "" {
		name = "image"
	}
	mime := "image/png"
	sizeN := int64(len(buf))
	return agentAttachment{URL: url, Name: name + ".png", Kind: "img", Mime: &mime, Size: &sizeN, Key: &key}, nil
}
