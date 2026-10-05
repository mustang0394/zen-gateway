// Package idgen 生成 opencode 格式的会话与消息 ID。
//
// opencode 的 ID 结构（见 packages/opencode/src/id/id.ts）：
//
//	<prefix>_<timeHex(12)><randomBase62(14)>
//
// 其中 timeHex = 6 字节时间部分，取值方向决定正/负：
//   - ascending:   v = ts*0x1000 + counter
//   - descending:  v = ~v（按 64 位有符号取反，截取低 48 位）
//
// 服务端只校验结构（ses_ 前缀 + 12 位小写 hex + 14 位 base62），
// 这里按 descending 语义生成，与真实 opencode 客户端一致。
package idgen

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"
)

const (
	alphabet    = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	totalLength = 26 // 前缀后 ID 主体总长：12 hex + 14 base62
	timeScale   = 0x1000
)

var counter atomic.Uint64

// Session 生成 ses_ 前缀的会话 ID。
func Session() string { return create("ses") }

// Message 生成 msg_ 前缀的消息 ID（用作 x-opencode-request）。
func Message() string { return create("msg") }

func create(prefix string) string {
	now := time.Now().UnixMilli()*timeScale + int64(counter.Add(1)%(timeScale-1))
	// descending：64 位取反后截低 48 位
	inverted := (^now) & ((1 << 48) - 1)
	return prefix + "_" + fmt.Sprintf("%012x", inverted) + randomBase62(14)
}

func randomBase62(n int) string {
	max := big.NewInt(int64(len(alphabet)))
	buf := make([]byte, n)
	for i := range buf {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			// crypto/rand 失败概率极低；兜底用时钟熵
			buf[i] = alphabet[time.Now().UnixNano()%int64(len(alphabet))]
			continue
		}
		buf[i] = alphabet[v.Int64()]
	}
	return string(buf)
}

// ---- 格式校验 -------------------------------------------------------------
//
// 上游只校验 x-opencode-session 的结构（实测结论，见 idgen_test.go 的对照记录）：
// 必须是 `ses_` + 恰好 12 位**小写** hex + 恰好 14 位 base62。
// 大小写、长度或字符集不符都会被判定为非官方客户端，返回 403 FreeTierError。
//
// 实测对照（对真实上游 opencode.ai/zen/v1）：
//
//	ses_ef5920720ffeDse8BpbCnsgiMT  → 200（12 小写 hex + 14 base62）
//	ses_EF5920720FFEDse8BpbCnsgiMT  → 403（hex 大写）
//	ses_ef5920720ffDse8BpbCnsgiMTX  → 403（hex 11 位）
//	ses_ef5920720fffeDse8BpbCnsgiMT → 403（hex 13 位）
//	ses_gf5920720ffgDse8BpbCnsgiMT  → 403（hex 含非 hex 字符）
//	ses_ef5920720ffeDse8BpbCnsgiM   → 403（base62 13 位）
//	ses_ef5920720ffeDse8BpbCnsgiMTX → 403（base62 15 位）
//	ses_ef5920720ffeDse8BpbCnsgiM_  → 403（base62 含下划线）
//
// 注：x-opencode-request 实测不做格式校验（UUID、短值、错误前缀均放行），
// 但这里仍按同一规则校验，以便在网关侧统一处理、并为上游日后收紧规则留出余量。

const (
	// hexLen 是 ID 主体中时间部分的十六进制位数。
	hexLen = 12
	// b62Len 是随机部分的 base62 位数。
	b62Len = 14
	// bodyLen 是前缀之后的主体长度。
	bodyLen = hexLen + b62Len
	// fullLen 是含前缀与下划线的完整长度（3 + 1 + 26）。
	fullLen = 30
)

// isLowerHex 报告字符是否为小写十六进制。
func isLowerHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// isBase62 报告字符是否属于 base62 字母表。
func isBase62(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isValidID 校验形如 <prefix>_<12位小写hex><14位base62> 的 ID。
func isValidID(id, prefix string) bool {
	if len(id) != fullLen {
		return false
	}
	if len(prefix) != 3 || id[0] != prefix[0] || id[1] != prefix[1] || id[2] != prefix[2] {
		return false
	}
	if id[3] != '_' {
		return false
	}
	for i := 4; i < 4+hexLen; i++ {
		if !isLowerHex(id[i]) {
			return false
		}
	}
	for i := 4 + hexLen; i < fullLen; i++ {
		if !isBase62(id[i]) {
			return false
		}
	}
	return true
}

// IsSession 报告字符串是否为合法的 ses_ ID（上游会严格校验此格式）。
func IsSession(id string) bool { return isValidID(id, "ses") }

// IsMessage 报告字符串是否为合法的 msg_ ID。
func IsMessage(id string) bool { return isValidID(id, "msg") }
