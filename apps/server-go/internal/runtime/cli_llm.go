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
	"log/slog"
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
	// TS legacy 客户端不显式传 baseURL —— OpenAI SDK 缺省读 OPENAI_BASE_URL。
	legacyBase := os.Getenv("OPENAI_BASE_URL")
	if legacyBase == "" {
		legacyBase = "https://api.openai.com/v1"
	}
	baseURL, apiKey = legacyBase, os.Getenv("OPENAI_API_KEY")
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
	model := imageModelEnv()
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

/* ───────────── Responses 客户端(palette/gender/avatar/agenda 用) ───────────── */

// cliResponsesArgs:CLI 侧 Responses 调用的参数面(非流式、无 tools——
// agent turn 的流式/工具翻译不在 /runtime/cli 范围内)。
type cliResponsesArgs struct {
	Model           string
	Instructions    string
	Input           string
	MaxOutputTokens int64  // 0 = 省略
	JSONMode        bool   // text.format.type == 'json_object'
	ReasoningEffort string // "" = 省略(适配器路径本就丢弃)
}

// cliResponsesResult:output_text + 用量(台账用)。
type cliResponsesResult struct {
	OutputText string
	Usage      *TokenUsage
}

var novitaUnconfiguredWarned bool

// isNovitaModel / novitaPrefix 与 llm.ts withNovitaRouting 一致:模型名
// novita/ 前缀选择 Novita;未配置 key 则降级走普通客户端(仅警告一次)。
func isNovitaModel(model string) bool { return strings.HasPrefix(model, "novita/") }

// cliResponsesCreate:responses.create 的直接 HTTP 形态 + Novita
// Chat-Completions 翻译(novita.ts 的非流式分支:instructions→system、
// string input→user、max_output_tokens→max_tokens、json_object→
// response_format;usage 反向映射)。返回 output_text(SDK 的
// output_text = message content 的 output_text 拼接)。
func (s *Service) cliResponsesCreate(ctx context.Context, tenant string, args cliResponsesArgs) (cliResponsesResult, error) {
	baseURL, apiKey := s.cliLlmEndpoint(ctx, tenant)
	var reqURL, method string
	var body map[string]any
	if isNovitaModel(args.Model) && os.Getenv("NOVITA_API_KEY") != "" {
		base := strings.TrimRight(os.Getenv("NOVITA_BASE_URL"), "/")
		if base == "" {
			base = "https://api.novita.ai/openai"
		}
		reqURL = base + "/chat/completions"
		body = map[string]any{
			"model":    strings.TrimPrefix(args.Model, "novita/"),
			"messages": novitaChatMessages(args.Instructions, args.Input),
			"stream":   false,
		}
		if args.MaxOutputTokens > 0 {
			body["max_tokens"] = args.MaxOutputTokens
		}
		if args.JSONMode {
			body["response_format"] = map[string]any{"type": "json_object"}
		}
	} else {
		if isNovitaModel(args.Model) && !novitaUnconfiguredWarned {
			novitaUnconfiguredWarned = true
			slog.Warn(`[llm] model requests Novita but NOVITA_API_KEY is unset — using the tenant's normal client instead`, "model", args.Model)
		}
		reqURL = baseURL + "/responses"
		body = map[string]any{
			"model":        args.Model,
			"instructions": args.Instructions,
			"input":        args.Input,
		}
		if args.MaxOutputTokens > 0 {
			body["max_output_tokens"] = args.MaxOutputTokens
		}
		if args.JSONMode {
			body["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
		}
		if args.ReasoningEffort != "" {
			body["reasoning"] = map[string]any{"effort": args.ReasoningEffort}
		}
	}
	method = http.MethodPost
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(payload))
	if err != nil {
		return cliResponsesResult{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+apiKey)
	resp, err := httpClientLLM.Do(req)
	if err != nil {
		return cliResponsesResult{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return cliResponsesResult{}, fmt.Errorf("%d %s", resp.StatusCode, truncateRunesSimple(string(respBody), 400))
	}
	if isNovitaModel(args.Model) && strings.HasSuffix(reqURL, "/chat/completions") {
		return parseNovitaChatCompletion(respBody)
	}
	return parseResponsesOutput(respBody)
}

// novitaChatMessages:instructions → system,string input → user。
func novitaChatMessages(instructions, input string) []map[string]any {
	msgs := []map[string]any{}
	if instructions != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": instructions})
	}
	if input != "" {
		msgs = append(msgs, map[string]any{"role": "user", "content": input})
	}
	return msgs
}

// parseResponsesOutput:Responses 非流式响应 → output_text + usage。
func parseResponsesOutput(raw []byte) (cliResponsesResult, error) {
	var parsed struct {
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			InputDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokens int64 `json:"output_tokens"`
			OutputDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return cliResponsesResult{}, err
	}
	var b strings.Builder
	for _, item := range parsed.Output {
		if item.Type != "message" && item.Type != "" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" || c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
	}
	out := cliResponsesResult{OutputText: b.String()}
	if parsed.Usage != nil {
		u := TokenUsage{
			InputTokens:  parsed.Usage.InputTokens,
			OutputTokens: parsed.Usage.OutputTokens,
		}
		if parsed.Usage.InputDetails != nil {
			u.CachedInputTokens = parsed.Usage.InputDetails.CachedTokens
		}
		out.Usage = &u
	}
	return out, nil
}

// parseNovitaChatCompletion:Chat Completions → Responses 形态(novita.ts
// toResponseUsage 的反向映射)。
func parseNovitaChatCompletion(raw []byte) (cliResponsesResult, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return cliResponsesResult{}, err
	}
	out := cliResponsesResult{}
	if len(parsed.Choices) > 0 {
		out.OutputText = parsed.Choices[0].Message.Content
	}
	if parsed.Usage != nil {
		u := TokenUsage{
			InputTokens:  parsed.Usage.PromptTokens,
			OutputTokens: parsed.Usage.CompletionTokens,
		}
		if parsed.Usage.PromptTokensDetails != nil {
			u.CachedInputTokens = parsed.Usage.PromptTokensDetails.CachedTokens
		}
		out.Usage = &u
	}
	return out, nil
}

// imageModelEnv:OPENAI_IMAGE_MODEL(TS 缺省 gpt-image-2)。
func imageModelEnv() string {
	if m := os.Getenv("OPENAI_IMAGE_MODEL"); m != "" {
		return m
	}
	return "gpt-image-2"
}
