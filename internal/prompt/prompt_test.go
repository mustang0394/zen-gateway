package prompt

import (
	"strings"
	"sync"
	"testing"
)

// TestBuiltinZenPromptEmbedded 验证内嵌提示词确实可用且内容完整。
// go:embed 失败或文件为空会导致整个注入功能静默失效，因此必须断言。
func TestBuiltinZenPromptEmbedded(t *testing.T) {
	p := Module("zen")
	if p == "" {
		t.Fatal("zen builtin prompt must be embedded and non-empty")
	}
	if len(p) < 5000 {
		t.Errorf("zen prompt looks truncated: %d bytes", len(p))
	}
	// 关键内容断言：确保是 opencode 的提示词而非其他文件
	for _, want := range []string{
		"You are opencode",
		"# Tone and style",
		"# Tool usage policy",
		"# Code References",
		"file_path:line_number",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("zen prompt missing %q", want)
		}
	}
	if !strings.HasSuffix(p, "\n") {
		t.Error("prompt should end with a newline (matches upstream file)")
	}
	// 不得含模板占位符（上游客户端是原样发送的）
	for _, bad := range []string{"{{", "${", "%s"} {
		if strings.Contains(p, bad) {
			t.Errorf("prompt must not contain template placeholder %q", bad)
		}
	}
}

func TestModuleLookup(t *testing.T) {
	if !HasBuiltin("zen") {
		t.Error("zen should have builtin prompt")
	}
	for _, m := range []string{"cline", "other", ""} {
		if HasBuiltin(m) {
			t.Errorf("%q should not have builtin prompt yet", m)
		}
		if got := Module(m); got != "" {
			t.Errorf("Module(%q) = %q, want empty", m, got)
		}
	}
	// 大小写与空白不敏感
	if Module("ZEN") == "" || Module(" zen ") == "" {
		t.Error("module lookup should be case/space insensitive")
	}
}

func TestResolvePrefersOverride(t *testing.T) {
	SetAllOverrides(nil)
	builtin := Module("zen")

	if got := Resolve("zen"); got != builtin {
		t.Error("without override, Resolve should return builtin")
	}
	if Source("zen") != "builtin" {
		t.Errorf("source = %q, want builtin", Source("zen"))
	}

	SetOverride("zen", "custom prompt")
	if got := Resolve("zen"); got != "custom prompt" {
		t.Errorf("override should win, got %q", got)
	}
	if Source("zen") != "override" || !IsOverridden("zen") {
		t.Error("source should report override")
	}

	// 空串清除覆盖，回落到内置
	SetOverride("zen", "   ")
	if got := Resolve("zen"); got != builtin {
		t.Error("blank override should fall back to builtin")
	}
	if Source("zen") != "builtin" {
		t.Error("source should be builtin again")
	}

	// 无内置也无覆盖的模块
	if Source("cline") != "none" || Resolve("cline") != "" {
		t.Error("cline should report none")
	}

	// 覆盖仅存于自定义的模块
	SetOverride("cline", "cline custom")
	if Resolve("cline") != "cline custom" || Source("cline") != "override" {
		t.Error("cline override should be resolvable")
	}
}

func TestSetAllOverridesReplacesAndNormalizes(t *testing.T) {
	SetAllOverrides(map[string]string{
		"ZEN":    "upper",
		" cline": "spaced",
		"empty":  "   ",
	})
	defer SetAllOverrides(nil)

	if got := Resolve("zen"); got != "upper" {
		t.Errorf("zen = %q, want upper (keys normalized)", got)
	}
	// 首尾带空格的 key 也应被规范化
	if got := Resolve("cline"); got != "spaced" {
		t.Errorf("cline = %q, want spaced", got)
	}
	// 纯空白值视为未设置
	if Source("empty") != "none" {
		t.Errorf("blank override should be dropped, got %q", Source("empty"))
	}
}

func TestConcurrentOverrideAccess(t *testing.T) {
	SetAllOverrides(nil)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				SetOverride("zen", strings.Repeat("x", j%10+1))
				_ = Resolve("zen")
				_ = Source("zen")
				SetAllOverrides(map[string]string{"cline": "c"})
			}
		}(i)
	}
	wg.Wait()
	SetAllOverrides(nil)
}
