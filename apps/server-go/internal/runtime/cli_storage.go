// /runtime/cli 附件存储(#89):storage.ts 本地模式的 Go 等价 —— 键落
// UPLOAD_DIR,URL /uploads/<key>(与 TS 静态处理器同形;R2 模式待存储
// 抽象票接入,镜像测试走本地模式)。附件对象进 messages.attachment
// (jsonb 列,PG 自行按键长排序,序列化键序无对齐负担)。
package runtime

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// agentAttachment:reply --attach* 家族落库的附件对象。Mime/Size/Key 仅
// 在有值时出现(TS 字面量 undefined 键被 stringify 丢弃)。
type agentAttachment struct {
	URL  string `json:"url"`
	Name string `json:"name"`
	Kind string `json:"kind"` // 'img' | 'file'
	Mime *string `json:"mime,omitempty"`
	Size *int64  `json:"size,omitempty"`
	Key  *string `json:"key,omitempty"`
}

func uploadDir() string {
	if d := os.Getenv("CUMORA_UPLOADS_DIR"); d != "" {
		return d
	}
	return filepath.Join("server", "uploads")
}

// cliStoragePut:本地模式 storage.put —— 写文件,返回 /uploads/<key>。
func cliStoragePut(key string, body []byte) (string, error) {
	path := filepath.Join(uploadDir(), filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return "/uploads/" + key, nil
}

func randHex32() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// cliSaveTextAttachment:把正文内容存成真实文件附件(text/* 渲染端会
// 内联进上下文)。
func cliSaveTextAttachment(filename, content string) (agentAttachment, error) {
	ext := "txt"
	if i := strings.LastIndexByte(filename, '.'); i >= 0 {
		ext = strings.ToLower(filename[i+1:])
	}
	mime := "text/plain"
	switch ext {
	case "md":
		mime = "text/markdown"
	case "json":
		mime = "application/json"
	case "csv":
		mime = "text/csv"
	case "html":
		mime = "text/html"
	case "yml", "yaml":
		mime = "application/x-yaml"
	case "toml":
		mime = "application/x-toml"
	}
	buf := []byte(content)
	key := "attachments/" + randHex32() + "." + ext
	url, err := cliStoragePut(key, buf)
	if err != nil {
		return agentAttachment{}, err
	}
	size := int64(len(buf))
	return agentAttachment{URL: url, Name: filename, Kind: "file", Mime: &mime, Size: &size, Key: &key}, nil
}

// nodeB64Alphabet:Node 的 base64 解码对非字母表字符是"丢弃"而非报错
// (Buffer.from('!!!','base64') → 空串)。镜像该宽容度。
var nodeB64Filter = regexp.MustCompile(`[^A-Za-z0-9+/\-_]`)

// nodeB64Decode:先滤掉非法字符再补齐 padding 解码;空输入返回空串、
// 无错误(与 Buffer.from 一致,零字节由调用方按 TS 语义报错)。
func nodeB64Decode(s string) ([]byte, error) {
	cleaned := nodeB64Filter.ReplaceAllString(strings.TrimSpace(s), "")
	if cleaned == "" {
		return []byte{}, nil
	}
	// URL-safe 变体归一到标准字母表
	cleaned = strings.NewReplacer("-", "+", "_", "/").Replace(cleaned)
	// 丢弃 padding 后按 4 对齐重补(Buffer.from 的实际行为)
	cleaned = strings.TrimRight(cleaned, "=")
	pad := (4 - len(cleaned)%4) % 4
	cleaned += strings.Repeat("=", pad)
	return base64.StdEncoding.DecodeString(cleaned)
}

// cliSaveBytesAttachment:--attach-bytes 路径(base64 → 32MB 上限 → 存
// 储)。mime:显式提示 > 扩展名猜测 > octet-stream;image/* 呈现为图。
func cliSaveBytesAttachment(filename, base64Body string, mimeHint string) (agentAttachment, error) {
	const maxBytes = 32 * 1024 * 1024
	buf, err := nodeB64Decode(base64Body)
	if err != nil {
		return agentAttachment{}, fmt.Errorf("--bytes-b64 is not valid base64")
	}
	if len(buf) == 0 {
		return agentAttachment{}, fmt.Errorf("--bytes-b64 decoded to zero bytes")
	}
	if len(buf) > maxBytes {
		return agentAttachment{}, fmt.Errorf("attachment too large (%d > %d)", len(buf), maxBytes)
	}
	ext := "bin"
	if i := strings.LastIndexByte(filename, '.'); i >= 0 {
		ext = strings.ToLower(filename[i+1:])
	}
	mime := mimeHint
	if mime == "" {
		if m := extToMime(ext); m != "" {
			mime = m
		} else {
			mime = "application/octet-stream"
		}
	}
	kind := "file"
	if strings.HasPrefix(mime, "image/") {
		kind = "img"
	}
	key := "attachments/" + randHex32() + "." + ext
	url, err := cliStoragePut(key, buf)
	if err != nil {
		return agentAttachment{}, err
	}
	size := int64(len(buf))
	return agentAttachment{URL: url, Name: filename, Kind: kind, Mime: &mime, Size: &size, Key: &key}, nil
}

// extToMime:扩展名的尽力 mime 猜测;未知返回 ""。
func extToMime(ext string) string {
	switch ext {
	// Images
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	case "svg":
		return "image/svg+xml"
	// Docs
	case "pdf":
		return "application/pdf"
	case "doc":
		return "application/msword"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "xls":
		return "application/vnd.ms-excel"
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	// Archives
	case "zip":
		return "application/zip"
	case "tar":
		return "application/x-tar"
	case "gz":
		return "application/gzip"
	// Text / code
	case "txt":
		return "text/plain"
	case "md":
		return "text/markdown"
	case "csv":
		return "text/csv"
	case "json":
		return "application/json"
	case "yml", "yaml":
		return "application/x-yaml"
	case "toml":
		return "application/x-toml"
	case "html":
		return "text/html"
	// Media
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "mp4":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	default:
		return ""
	}
}
