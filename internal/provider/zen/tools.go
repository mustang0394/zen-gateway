package zen

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

// 本文件负责「工具定义」这一上游指纹要素。
//
// 上游免费层的校验规则（2026-10-14 起，见 README「上游校验」一节）：
//   - 请求体必须带 stream + 核心工具，否则 403 FreeTierError；
//   - 只要工具名是上游认识的名字，描述与参数结构就必须与官方客户端**逐字节一致**，
//     网关早期自造的 bash/read stub 同样会被判为伪客户端而 403。
//
// 因此这里内嵌从官方 CLI 2.0.29 抓取的真实定义：
//   - 注入时使用真实定义（不再是 stub）；
//   - 下游声明的同名工具会被改写为真实定义（客户端自定义 schema 会 403）；
//   - 上游不认识的名字原样保留，下游自己的函数调用不受影响。

// coreTools 是上游要求的核心工具名。缺任一即 403。
//
// 2.x 客户端的 shell 工具在 1.x 里叫 bash：bash 属于「上游认识的名字」，
// 而网关没有它的真实定义，因此绝不注入 bash（注入即 403）。
var coreTools = []string{"shell", "edit", "glob", "grep", "read"}

//go:embed opencode_tools.json
var toolsJSON []byte

// toolDefinition 是 opencode_tools.json 中一条工具定义（描述 + JSON Schema）。
type toolDefinition struct {
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

var (
	toolsOnce sync.Once
	tools     map[string]toolDefinition
)

// knownTool 返回上游认识的工具名的真实定义。
func knownTool(name string) (toolDefinition, bool) {
	toolsOnce.Do(func() {
		defs := map[string]toolDefinition{}
		if err := json.Unmarshal(toolsJSON, &defs); err != nil {
			// 内嵌数据损坏时退化为「没有真实定义」：工具仍会注入，
			// 但不会拿空定义去覆盖下游的同名工具。
			defs = map[string]toolDefinition{}
		}
		tools = defs
	})
	def, ok := tools[name]
	return def, ok
}

// bareSchema 是参数结构缺失时的兜底空对象 schema。
func bareSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// ---- chat/completions 形状 -------------------------------------------------

// ensureToolsChat 补齐 chat/completions 风格的工具定义，返回是否有改动。
func ensureToolsChat(body map[string]any) bool {
	raw, _ := body["tools"].([]any)
	changed := false
	present := map[string]bool{}

	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := chatToolName(m)
		if name == "" {
			continue
		}
		present[name] = true
		if _, known := knownTool(name); !known {
			continue // 上游不认识的名字：原样透传
		}
		raw[i] = chatTool(name)
		changed = true
	}

	missing := missingCoreTools(present)
	if len(missing) == 0 {
		return changed
	}
	for _, name := range missing {
		raw = append(raw, chatTool(name))
	}
	body["tools"] = raw
	return true
}

// chatToolName 提取 chat 形状工具名（function.name，兼容平铺 name）。
func chatToolName(m map[string]any) string {
	if fn, ok := m["function"].(map[string]any); ok {
		name, _ := fn["name"].(string)
		return strings.TrimSpace(name)
	}
	name, _ := m["name"].(string)
	return strings.TrimSpace(name)
}

// chatTool 渲染一条 chat 形状工具（真实定义优先）。
func chatTool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": toolDescription(name),
			"parameters":  toolParameters(name),
		},
	}
}

// ---- responses 形状 -------------------------------------------------------

// ensureToolsResponses 补齐 responses 风格的工具定义（function 平铺），返回是否有改动。
func ensureToolsResponses(body map[string]any) bool {
	raw, _ := body["tools"].([]any)
	changed := false
	present := map[string]bool{}

	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		present[name] = true
		if _, known := knownTool(name); !known {
			continue
		}
		raw[i] = responsesTool(name)
		changed = true
	}

	missing := missingCoreTools(present)
	if len(missing) == 0 {
		return changed
	}
	for _, name := range missing {
		raw = append(raw, responsesTool(name))
	}
	body["tools"] = raw
	return true
}

// responsesTool 渲染一条 responses 形状工具（真实定义优先）。
func responsesTool(name string) map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        name,
		"description": toolDescription(name),
		"parameters":  toolParameters(name),
	}
}

// ---- 公共部分 -------------------------------------------------------------

// missingCoreTools 返回缺失的核心工具名（按 coreTools 顺序，保证注入顺序确定）。
func missingCoreTools(present map[string]bool) []string {
	var out []string
	for _, name := range coreTools {
		if !present[name] {
			out = append(out, name)
		}
	}
	return out
}

func toolDescription(name string) string {
	if def, ok := knownTool(name); ok && strings.TrimSpace(def.Description) != "" {
		return def.Description
	}
	return "Agent tool " + name
}

func toolParameters(name string) map[string]any {
	if def, ok := knownTool(name); ok && def.Parameters != nil {
		return def.Parameters
	}
	return bareSchema()
}
