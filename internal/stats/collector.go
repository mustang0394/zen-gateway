// Package stats 解析上游响应中的 token 用量与首字时长（TTFT）。
//
// 采集方式：转发响应时把字节流同时喂给 Collector。
//   - 流式（SSE）：逐行解析 data: 负载，首个含非空文本增量的 chunk 记为 TTFT，
//     末尾 usage chunk 提供 token 数。
//   - 非流式：整体 JSON 解析 usage 字段。
//
// 上游若不返回 usage（未支持 stream_options.include_usage），
// 则统计降级为仅记录请求数与 TTFT，不影响转发本身。
package stats

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"
)

// Usage 是一次请求的用量统计。
type Usage struct {
	PromptTokens     int64 `json:"promptTokens"`
	CompletionTokens int64 `json:"completionTokens"`
	CachedTokens     int64 `json:"cachedTokens"`
	TotalTokens      int64 `json:"totalTokens"`
}

// Result 是采集结果。
type Result struct {
	Usage    Usage
	TTFTMs   int64
	HasTTFT  bool
	Finished bool
}

// Collector 在转发过程中增量解析响应。
type Collector struct {
	mu       sync.Mutex
	start    time.Time
	stream   bool
	usage    Usage
	haveUse  bool
	ttftMs   int64
	hasTTFT  bool
	finished bool

	// SSE 解析状态
	tail     []byte
	buf      bytes.Buffer
	sseLimit int
}

// maxParseBytes 是解析缓冲上限（非流式响应与异常流的保护）。
const maxParseBytes = 2 << 20

// NewCollector 创建采集器。stream 表示该请求是否为流式。
func NewCollector(stream bool, start time.Time) *Collector {
	return &Collector{start: start, stream: stream, sseLimit: maxParseBytes}
}

// Write 追加响应字节（实现 io.Writer，可直接接在复制流上）。
func (c *Collector) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if c.finished {
		return n, nil
	}

	if c.stream {
		c.consumeSSE(p)
		return n, nil
	}
	// 非流式：缓存到上限后整体解析
	if c.buf.Len() < c.sseLimit {
		remain := c.sseLimit - c.buf.Len()
		if len(p) > remain {
			c.buf.Write(p[:remain])
		} else {
			c.buf.Write(p)
		}
	}
	return n, nil
}

// Finish 结束采集并返回结果。
func (c *Collector) Finish() Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finished = true

	if c.stream {
		// 处理结尾没有换行的残留行（真实流常在最后一个 chunk 后缺少 \n）
		if len(c.tail) > 0 {
			remain := c.tail
			c.tail = nil
			c.handleSSELine(remain)
		}
	} else if !c.haveUse {
		c.parseJSONUsage(c.buf.Bytes())
	}
	return Result{Usage: c.usage, TTFTMs: c.ttftMs, HasTTFT: c.hasTTFT, Finished: true}
}

// ---- SSE 解析 -------------------------------------------------------------

func (c *Collector) consumeSSE(p []byte) {
	data := append(c.tail, p...)
	c.tail = nil

	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			// 保留未完成的一行（限制长度避免异常流导致内存增长）
			if len(data) > 64<<10 {
				data = data[len(data)-(64<<10):]
			}
			c.tail = append([]byte(nil), data...)
			return
		}
		line := bytes.TrimRight(data[:idx], "\r")
		data = data[idx+1:]
		c.handleSSELine(line)
	}
}

func (c *Collector) handleSSELine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	c.consumeJSONChunk(payload)
}

// consumeJSONChunk 解析单个 SSE chunk，提取文本增量与 usage。
func (c *Collector) consumeJSONChunk(payload []byte) {
	var chunk struct {
		Usage   *usagePayload `json:"usage"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
				// responses 风格的流式文本
				Text string `json:"text"`
			} `json:"delta"`
			Text string `json:"text"` // 某些实现直接把 text 放在 choice 上
		} `json:"choices"`
		// OpenAI Responses API 的流式事件
		Type     string `json:"type"`
		Delta    string `json:"delta"`
		Response *struct {
			Usage *usagePayload `json:"usage"`
		} `json:"response"`
		// 顶层 usage（responses 的 response.completed 事件）
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}

	// TTFT：首个包含文本增量的 chunk
	if !c.hasTTFT {
		text := ""
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				text = ch.Delta.Content
				break
			}
			if ch.Delta.Text != "" {
				text = ch.Delta.Text
				break
			}
			if ch.Text != "" {
				text = ch.Text
				break
			}
		}
		if text == "" && chunk.Delta != "" {
			text = chunk.Delta
		}
		if text != "" {
			c.hasTTFT = true
			c.ttftMs = time.Since(c.start).Milliseconds()
		}
	}

	// usage 可能在 chunk 顶层、response 字段内
	if chunk.Usage != nil {
		c.applyUsage(chunk.Usage)
	}
	if chunk.Response != nil && chunk.Response.Usage != nil {
		c.applyUsage(chunk.Response.Usage)
	}
}

// ---- 非流式解析 -----------------------------------------------------------

func (c *Collector) parseJSONUsage(raw []byte) {
	if len(raw) == 0 {
		return
	}
	var payload struct {
		Usage *usagePayload `json:"usage"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	if payload.Usage != nil {
		c.applyUsage(payload.Usage)
	}
}

// usagePayload 兼容 chat/completions 与 responses 两套字段名。
type usagePayload struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`

	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (c *Collector) applyUsage(u *usagePayload) {
	in, out, total := u.PromptTokens, u.CompletionTokens, u.TotalTokens
	if in == 0 && u.InputTokens > 0 {
		in = u.InputTokens
	}
	if out == 0 && u.OutputTokens > 0 {
		out = u.OutputTokens
	}
	if total == 0 {
		total = in + out
	}
	cached := int64(0)
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	} else if u.InputTokensDetails != nil {
		cached = u.InputTokensDetails.CachedTokens
	}

	// 多个 usage chunk 时取最后一次的完整值（流式通常只在末尾出现）
	c.usage = Usage{
		PromptTokens:     in,
		CompletionTokens: out,
		CachedTokens:     cached,
		TotalTokens:      total,
	}
	c.haveUse = true
}

// HasUsage 报告是否解析到用量数据（供日志与展示）。
func (c *Collector) HasUsage() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.haveUse
}
