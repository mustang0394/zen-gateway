package cline

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// modelsResp 是 ConvertModels 输出的解析结构（用于断言）。
type modelsResp struct {
	Object string `json:"object"`
	Data   []struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Created     int64  `json:"created"`
		OwnedBy     string `json:"owned_by"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"data"`
}

func convert(t *testing.T, in string) modelsResp {
	t.Helper()
	out, err := (&Provider{}).ConvertModels([]byte(in))
	if err != nil {
		t.Fatalf("ConvertModels: %v", err)
	}
	var got modelsResp
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	return got
}

// 真实上游 free 桶的结构（2026-10 实测，字段名与类型照抄）。
const realFreePayload = `{
  "recommended": [{"id":"anthropic/claude-sonnet-5.5","name":"claude-sonnet-5.5","description":"","tags":["NEW"]}],
  "free": [
    {"id":"stealth/space-bunny-alpha","name":"space-bunny-alpha","description":"Blazing-fast inference with 1M context","tags":[]},
    {"id":"cline-free/mimo-v2.6-flash","name":"Mimo V2.6 Flash","description":"Mixture-of-Experts architecture with 309B total parameters","tags":[]},
    {"id":"cline-free/muse-spark-1.3-contributor","name":"Muse Spark 1.3 Contributor","description":"Meta’s multimodal reasoning model.","tags":[]}
  ],
  "clinePass": [{"id":"glm-5.2","name":"glm-5.2","description":"","tags":[]}],
  "clineCloud": [{"id":"cline-cloud/x","name":"x","description":"","tags":[]}]
}`

func TestConvertModelsOnlyTakesFreeBucket(t *testing.T) {
	got := convert(t, realFreePayload)

	if got.Object != "list" {
		t.Errorf("object = %q, want %q", got.Object, "list")
	}
	if len(got.Data) != 3 {
		t.Fatalf("data length = %d, want 3 (only free bucket)", len(got.Data))
	}

	wantIDs := []string{
		"stealth/space-bunny-alpha",
		"cline-free/mimo-v2.6-flash",
		"cline-free/muse-spark-1.3-contributor",
	}
	for i, want := range wantIDs {
		if got.Data[i].ID != want {
			t.Errorf("data[%d].id = %q, want %q", i, got.Data[i].ID, want)
		}
	}

	// 付费/其他桶不得混入
	for _, m := range got.Data {
		if strings.HasPrefix(m.ID, "anthropic/") || strings.HasPrefix(m.ID, "cline-cloud/") || m.ID == "glm-5.2" {
			t.Errorf("non-free model leaked into list: %q", m.ID)
		}
	}
}

func TestConvertModelsOpenAIFormatFields(t *testing.T) {
	got := convert(t, realFreePayload)
	m := got.Data[1] // cline-free/mimo-v2.6-flash

	if m.Object != "model" {
		t.Errorf("object = %q, want %q", m.Object, "model")
	}
	if m.OwnedBy != "cline-free" {
		t.Errorf("owned_by = %q, want %q", m.OwnedBy, "cline-free")
	}
	if m.Created <= 0 {
		t.Errorf("created = %d, want positive unix timestamp", m.Created)
	}
	if m.Name != "Mimo V2.6 Flash" {
		t.Errorf("name = %q, want %q", m.Name, "Mimo V2.6 Flash")
	}
	if !strings.Contains(m.Description, "Mixture-of-Experts") {
		t.Errorf("description not preserved: %q", m.Description)
	}
}

// owned_by 取 ID 前缀，与官方 /v1/models 惯例一致。
func TestConvertModelsOwnedByDerivedFromPrefix(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"cline-free/mimo-v2.6-flash", "cline-free"},
		{"stealth/space-bunny-alpha", "stealth"},
		{"a/b/c", "a"},
		{"no-prefix-model", "cline"}, // 无前缀回落
		{"/leading-slash", "cline"},  // 前缀为空，回落
	}
	for _, c := range cases {
		if got := ownerOf(c.id); got != c.want {
			t.Errorf("ownerOf(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// free 桶显式为 [] 或 null 时，应返回合法的空列表而不是 null。
func TestConvertModelsEmptyFreeBucket(t *testing.T) {
	for name, in := range map[string]string{
		"free 为空数组":   `{"free":[]}`,
		"free 为 null": `{"free":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := convert(t, in)
			if got.Object != "list" {
				t.Errorf("object = %q, want %q", got.Object, "list")
			}
			if len(got.Data) != 0 {
				t.Errorf("data = %+v, want empty", got.Data)
			}
			// data 必须是 [] 而非 null，否则部分客户端解析失败
			out, _ := (&Provider{}).ConvertModels([]byte(in))
			if !strings.Contains(string(out), `"data":[]`) {
				t.Errorf("data must serialize as [], got %s", out)
			}
		})
	}
}

// free 桶整体缺失说明上游 schema 漂移或返回了错误对象，必须报错——
// 否则下游只看到“没有模型”，故障被静默掩盖。
func TestConvertModelsMissingFreeBucket(t *testing.T) {
	for name, in := range map[string]string{
		"只有 recommended": `{"recommended":[{"id":"a/b"}]}`,
		"空对象":            `{}`,
		"错误对象":           `{"error":"unauthorized"}`,
		"嵌套包装":           `{"data":{"free":[{"id":"a/b"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&Provider{}).ConvertModels([]byte(in)); err == nil {
				t.Fatalf("expected error for %s", in)
			}
		})
	}
}

// 报错信息必须列出实际存在的顶层字段，否则运维无从判断上游变成了什么形状。
func TestConvertModelsMissingFreeBucketReportsPresentKeys(t *testing.T) {
	_, err := (&Provider{}).ConvertModels([]byte(`{"recommended":[],"clinePass":[],"clineCloud":[]}`))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"clineCloud", "clinePass", "recommended"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention present key %q, got: %v", want, err)
		}
	}
}

// 缺少 id 的条目对下游无意义，应跳过而不是产生空 id 模型。
func TestConvertModelsSkipsBlankID(t *testing.T) {
	got := convert(t, `{"free":[{"id":"","name":"empty"},{"id":"  ","name":"spaces"},{"id":"ok/m"}]}`)
	if len(got.Data) != 1 || got.Data[0].ID != "ok/m" {
		t.Fatalf("data = %+v, want only ok/m", got.Data)
	}
}

// ID 两端空白应被裁掉。
func TestConvertModelsTrimsWhitespace(t *testing.T) {
	got := convert(t, `{"free":[{"id":"  pad/m  ","name":"  N  ","description":"  D  "}]}`)
	if len(got.Data) != 1 {
		t.Fatalf("data = %+v", got.Data)
	}
	m := got.Data[0]
	if m.ID != "pad/m" || m.Name != "N" || m.Description != "D" {
		t.Errorf("whitespace not trimmed: %+v", m)
	}
}

// 非法 JSON 必须报错，而不是静默返回空列表（否则上游异常会被掩盖成"没有模型"）。
func TestConvertModelsInvalidJSON(t *testing.T) {
	if _, err := (&Provider{}).ConvertModels([]byte(`{"free":`)); err == nil {
		t.Error("expected error for truncated JSON, got nil")
	}
	if _, err := (&Provider{}).ConvertModels([]byte(`not json`)); err == nil {
		t.Error("expected error for non-JSON body, got nil")
	}
}

// 上游把 free 写成字符串等类型不符时，应显式报错而非产出错误数据。
func TestConvertModelsTypeMismatch(t *testing.T) {
	for name, in := range map[string]string{
		"free 为字符串": `{"free":"nope"}`,
		"free 为对象":  `{"free":{"id":"a/b"}}`,
		"条目 id 类型错": `{"free":[{"id":123}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&Provider{}).ConvertModels([]byte(in)); err == nil {
				t.Errorf("expected error for %s", in)
			}
		})
	}
}

// tags 是上游存在但网关不用的字段，其类型变动不得拖挂整个端点。
func TestConvertModelsIgnoresUnusedFields(t *testing.T) {
	for name, in := range map[string]string{
		"tags 为字符串":  `{"free":[{"id":"a/b","name":"N","tags":"NEW"}]}`,
		"tags 为数字数组": `{"free":[{"id":"a/b","name":"N","tags":[1,2]}]}`,
		"未知新字段":      `{"free":[{"id":"a/b","name":"N","brandNew":{"x":1}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := convert(t, in)
			if len(got.Data) != 1 || got.Data[0].ID != "a/b" {
				t.Errorf("data = %+v", got.Data)
			}
		})
	}
}

func TestModelsUpstreamPath(t *testing.T) {
	if got := (&Provider{}).ModelsUpstreamPath(); got != "/ai/cline/recommended-models" {
		t.Errorf("ModelsUpstreamPath() = %q, want %q", got, "/ai/cline/recommended-models")
	}
}

// 未配置上游时 New 必须回落默认基址，保证 ModelsUpstreamPath 拼接后可用。
func TestNewFallsBackToDefaultUpstream(t *testing.T) {
	p := New("", time.Hour, nil)
	if p.UpstreamBase() != DefaultUpstream {
		t.Errorf("UpstreamBase() = %q, want %q", p.UpstreamBase(), DefaultUpstream)
	}
	if got := p.UpstreamBase() + p.ModelsUpstreamPath(); got != "https://api.cline.bot/api/v1/ai/cline/recommended-models" {
		t.Errorf("full models URL = %q", got)
	}
}
