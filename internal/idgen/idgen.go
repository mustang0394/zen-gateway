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
