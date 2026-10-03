package stats

import (
	"strings"
	"testing"
	"time"
)

func TestStreamCollectorExtractsUsageAndTTFT(t *testing.T) {
	start := time.Now().Add(-120 * time.Millisecond)
	c := NewCollector(true, start)

	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":""}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"你好"}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"世界"}}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":40,"total_tokens":160,"prompt_tokens_details":{"cached_tokens":96}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	// 分片写入，模拟真实网络分块
	for _, chunk := range splitChunks(stream, 37) {
		if _, err := c.Write([]byte(chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	res := c.Finish()

	if !res.Finished {
		t.Error("collector must report finished")
	}
	if res.Usage.PromptTokens != 120 || res.Usage.CompletionTokens != 40 || res.Usage.TotalTokens != 160 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.Usage.CachedTokens != 96 {
		t.Errorf("cached tokens = %d, want 96", res.Usage.CachedTokens)
	}
	if !res.HasTTFT {
		t.Fatal("expected TTFT to be recorded")
	}
	if res.TTFTMs < 100 {
		t.Errorf("TTFT = %dms, expected >= 100ms given start offset", res.TTFTMs)
	}
}

func TestStreamIgnoresRoleOnlyChunksForTTFT(t *testing.T) {
	c := NewCollector(true, time.Now())
	c.Write([]byte(`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n"))
	c.Write([]byte(`data: {"choices":[{"delta":{"content":""}}]}` + "\n"))
	res := c.Finish()
	if res.HasTTFT {
		t.Error("role-only and empty-content chunks must not count as first token")
	}
}

func TestNonStreamCollectorParsesUsage(t *testing.T) {
	c := NewCollector(false, time.Now())
	c.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],`))
	c.Write([]byte(`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	res := c.Finish()

	if res.Usage.PromptTokens != 10 || res.Usage.CompletionTokens != 5 || res.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.HasTTFT {
		t.Error("non-stream response should not report TTFT")
	}
}

func TestResponsesStyleUsageAndTTFT(t *testing.T) {
	c := NewCollector(true, time.Now().Add(-50*time.Millisecond))
	c.Write([]byte(`data: {"type":"response.output_text.delta","delta":"部分文本"}` + "\n"))
	c.Write([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":30,"output_tokens":12,"total_tokens":42,"input_tokens_details":{"cached_tokens":20}}}}` + "\n"))
	res := c.Finish()

	if !res.HasTTFT {
		t.Error("responses delta must count toward TTFT")
	}
	if res.Usage.PromptTokens != 30 || res.Usage.CompletionTokens != 12 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.Usage.CachedTokens != 20 {
		t.Errorf("cached = %d, want 20", res.Usage.CachedTokens)
	}
	if res.Usage.TotalTokens != 42 {
		t.Errorf("total = %d, want 42", res.Usage.TotalTokens)
	}
}

func TestTotalDerivedWhenMissing(t *testing.T) {
	c := NewCollector(false, time.Now())
	c.Write([]byte(`{"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	res := c.Finish()
	if res.Usage.TotalTokens != 10 {
		t.Errorf("total should fall back to in+out, got %d", res.Usage.TotalTokens)
	}
}

func TestMalformedChunksAreIgnored(t *testing.T) {
	c := NewCollector(true, time.Now())
	c.Write([]byte("data: not-json\n\n"))
	c.Write([]byte(": keepalive comment\n\n"))
	c.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"}}]}`))
	// 结尾没有换行：Finish 也应处理残留
	res := c.Finish()
	if res.Usage.TotalTokens != 0 {
		t.Errorf("malformed data must not produce usage, got %+v", res.Usage)
	}
	if !res.HasTTFT {
		t.Error("valid chunk should still be parsed")
	}
}

func TestNoUsageMeansZeroesNotFailure(t *testing.T) {
	c := NewCollector(true, time.Now())
	c.Write([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n"))
	res := c.Finish()
	if res.Usage.TotalTokens != 0 || res.Usage.PromptTokens != 0 {
		t.Errorf("missing usage should be zeros, got %+v", res.Usage)
	}
	if c.HasUsage() {
		t.Error("HasUsage must be false when upstream omits usage")
	}
}

func TestWriteAfterFinishIsIgnored(t *testing.T) {
	c := NewCollector(false, time.Now())
	c.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	first := c.Finish()
	c.Write([]byte(`{"usage":{"prompt_tokens":999,"completion_tokens":999}}`))
	second := c.Finish()
	if first.Usage.PromptTokens != second.Usage.PromptTokens {
		t.Errorf("writes after Finish must not change usage: %+v vs %+v", first.Usage, second.Usage)
	}
}

func splitChunks(s string, size int) []string {
	var out []string
	for len(s) > 0 {
		n := size
		if n > len(s) {
			n = len(s)
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	return out
}
