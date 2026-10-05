// Package prompt 内嵌各上游模块的原生系统提示词。
//
// 用途：当上游按关键词检测 system prompt（例如识别到非官方客户端特征就拒绝）时，
// 网关可检测到关键词后把整条系统提示词替换为本模块的原生提示词，
// 使请求看起来来自官方客户端。
//
// 提示词以内嵌资源形式随二进制发布，避免依赖运行时的外部文件；
// 同时允许通过 Web 管理端覆盖（覆盖值存于 SQLite 的 prompt_overrides 表）。
//
// 更新方式：把新的提示词文本放到对应文件后重新编译。文件为纯 UTF-8 文本，
// 会被原样使用（不做 trim），以保证与上游客户端的实际内容逐字节一致。
package prompt

import (
	_ "embed"
	"strings"
	"sync"
)

//go:embed zen_default.txt
var zenDefault string

// Module 返回指定模块的内置原生提示词。
// 目前仅 zen 有内置提示词；其他模块返回空串（表示未配置）。
func Module(module string) string {
	switch strings.ToLower(strings.TrimSpace(module)) {
	case "zen":
		return zenDefault
	default:
		return ""
	}
}

// HasBuiltin 报告指定模块是否有内置提示词。
func HasBuiltin(module string) bool { return Module(module) != "" }

// ---- 运行时覆盖 -----------------------------------------------------------

var (
	overrideMu sync.RWMutex
	// overrides 是各模块的用户自定义提示词（来自 SQLite），优先级高于内置值。
	overrides = map[string]string{}
)

// SetOverride 设置某模块的提示词覆盖值（空串表示清除覆盖、回落到内置值）。
func SetOverride(module, text string) {
	overrideMu.Lock()
	defer overrideMu.Unlock()
	key := strings.ToLower(strings.TrimSpace(module))
	if strings.TrimSpace(text) == "" {
		delete(overrides, key)
		return
	}
	overrides[key] = text
}

// SetAllOverrides 批量替换覆盖集合（供启动加载与重载使用）。
func SetAllOverrides(m map[string]string) {
	overrideMu.Lock()
	defer overrideMu.Unlock()
	next := make(map[string]string, len(m))
	for k, v := range m {
		if strings.TrimSpace(v) == "" {
			continue
		}
		next[strings.ToLower(strings.TrimSpace(k))] = v
	}
	overrides = next
}

// Resolve 返回该模块最终生效的提示词：优先覆盖值，其次内置值。
func Resolve(module string) string {
	key := strings.ToLower(strings.TrimSpace(module))
	overrideMu.RLock()
	ov := overrides[key]
	overrideMu.RUnlock()
	if strings.TrimSpace(ov) != "" {
		return ov
	}
	return Module(module)
}

// Source 报告提示词的来源："override" | "builtin" | "none"。
func Source(module string) string {
	key := strings.ToLower(strings.TrimSpace(module))
	overrideMu.RLock()
	_, hasOverride := overrides[key]
	overrideMu.RUnlock()
	if hasOverride {
		return "override"
	}
	if HasBuiltin(module) {
		return "builtin"
	}
	return "none"
}

// IsOverridden 报告该模块是否使用了自定义覆盖。
func IsOverridden(module string) bool { return Source(module) == "override" }
