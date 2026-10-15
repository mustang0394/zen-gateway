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
// 服务端除了结构(ses_ 前缀 + 12 位小写 hex + 14 位 base62)之外,
// **还校验时间戳低 12 位的取值**:counter 即 `v & 0xFFF`(descending 取反后
// 的同一字段),上游要求 counter <= 63,等价于 hex 主体末 3 位 >= 0xfc0。
// counter 越界一律 403 FreeTierError(实测,见下方 validate 段注释)。
//
// 这与 opencode 客户端 identifier.ts 的语义一致:counter 在**每一毫秒重置**
// (timestamp 变化即 counter=0),同一毫秒内最多递增,因此正常客户端产出的
// counter 必然很小;上游据此识别「非本机连续生成的 ID」。
package idgen

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"time"
)

const (
	alphabet    = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	totalLength = 26 // 前缀后 ID 主体总长:12 hex + 14 base62
	timeScale   = 0x1000
	// MaxCounter 是上游允许的时间戳低 12 位最大值(实测边界:63 通过、64 拒绝)。
	// 对应 opencode identifier.ts 中 counter 逐毫秒重置、同一毫秒内最多 +1 的语义。
	MaxCounter = 63
)

// counter 模拟 opencode 客户端:**每一毫秒重置**。
//
// 不能用全局自增取模:那样产出的 counter 会均匀落在 0..4094,
// 其中 62/64 的取值超出上游 MaxCounter 而被判为非官方客户端(403)。
var (
	counterMu  sync.Mutex
	counterMs  int64 // counter 当前所属的毫秒时间戳
	counterVal uint64
)

// Session 生成 ses_ 前缀的会话 ID。
func Session() string { return create("ses") }

// Message 生成 msg_ 前缀的消息 ID。
//
// 上游自 2026-10-14 起对 x-opencode-request 一律 403，网关已不再发送该头；
// 生成器保留给本地追踪/测试使用（见 internal/provider/zen）。
func Message() string { return create("msg") }

func create(prefix string) string {
	ms := time.Now().UnixMilli()

	counterMu.Lock()
	if ms != counterMs {
		counterMs = ms
		counterVal = 0
	}
	counterVal++
	c := counterVal
	if c > MaxCounter {
		// 同一毫秒内请求超过 MaxCounter 次(几乎不可能,单机毫秒级 64 次)。
		// 若回绕到 1 会与已有 ID 冲突,且 counter 仍是合法值;这里进位到下一毫秒
		// 以同时保证唯一性与合法性。
		counterMs = ms + 1
		counterVal = 1
		ms = counterMs
		c = counterVal
	}
	counterMu.Unlock()

	// descending:64 位取反后截低 48 位
	inverted := (^(ms*timeScale + int64(c))) & ((1 << 48) - 1)
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
// 上游校验 x-opencode-session 的两类规则(均经真实上游实测,见 validate_test.go):
//
//  1. 结构:`ses_` + 恰好 12 位**小写** hex + 恰好 14 位 base62。
//  2. 时间戳低 12 位(counter,hex 主体末 3 位)必须 <= MaxCounter(63),
//     即末 3 位 >= 0xfc0;越界视为非本机客户端。
//
// 实测对照(对真实上游 opencode.ai/zen/v1):
//
//	ses_ef5920720ffeDse8BpbCnsgiMT  → 200(12 小写 hex + 14 base62,末3位 ffe)
//	ses_EF5920720FFEDse8BpbCnsgiMT  → 403(hex 大写)
//	ses_ef5920720ffDse8BpbCnsgiMTX  → 403(hex 11 位)
//	ses_ef5920720fffeDse8BpbCnsgiMT → 403(hex 13 位)
//	ses_gf5920720ffgDse8BpbCnsgiMT  → 403(hex 含非 hex 字符)
//	ses_ef5920720ffeDse8BpbCnsgiM   → 403(base62 13 位)
//	ses_ef5920720ffeDse8BpbCnsgiMTX → 403(base62 15 位)
//	ses_ef5920720ffeDse8BpbCnsgiM_  → 403(base62 含下划线)
//	ses_<12hex 末3位 fc0..fff>     → 200(counter 0..63)
//	ses_<12hex 末3位 000..fbf>     → 403(counter 64..4094)
//
// 边界精确到末 3 位:0xfc0 通过、0xfbf 拒绝。
//
// 注:x-opencode-request 已停止发送(携带即 403),这里保留同一套校验规则
// 仅供测试与其他内部用途。

const (
	// hexLen 是 ID 主体中时间部分的十六进制位数。
	hexLen = 12
	// b62Len 是随机部分的 base62 位数。
	b62Len = 14
	// bodyLen 是前缀之后的主体长度。
	bodyLen = hexLen + b62Len
	// fullLen 是含前缀与下划线的完整长度(3 + 1 + 26)。
	fullLen = 30
	// hexCounterMin 是时间部分低 12 位(counter)的下界,即 hex 主体末 3 位的最小值。
	// 上游要求 counter 落在 [0, MaxCounter],换算成 hex 即末 3 位 >= 0xfff-MaxCounter。
	hexCounterMin = 0xfff - MaxCounter // 0xfc0
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
	// 时间戳低 12 位(counter)必须 <= MaxCounter,即末 3 位 hex >= 0xfc0。
	// 上游据此识别非本机客户端:counter 越界返回 403 FreeTierError(实测边界 63/64)。
	low, err := strconv.ParseUint(id[4+hexLen-3:4+hexLen], 16, 16)
	if err != nil || low < hexCounterMin {
		return false
	}
	return true
}

// IsSession 报告字符串是否为合法的 ses_ ID（上游会严格校验此格式）。
func IsSession(id string) bool { return isValidID(id, "ses") }

// IsMessage 报告字符串是否为合法的 msg_ ID。
func IsMessage(id string) bool { return isValidID(id, "msg") }
