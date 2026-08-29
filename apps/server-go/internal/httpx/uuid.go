// uuid —— 与 TS crypto.randomUUID 同形的十六进制 UUID 生成(#140 自
// runtime/observability.go 提为通用件):crypto/rand 16 字节 →
// 8-4-4-4-12;熵源故障按纳秒时钟兜底(保形不保熵,仅日志/台账 ID)。
package httpx

import (
	crand "crypto/rand"
	"fmt"
	"time"
)

// UUIDHex:crypto/rand 16 字节 → 8-4-4-4-12(与 TS randomUUID 同形)。
func UUIDHex() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
