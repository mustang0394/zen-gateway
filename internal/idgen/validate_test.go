package idgen

import "testing"

// TestIsSession 锁定上游对 x-opencode-session 的格式要求。
// 用例中的拒绝项均经真实上游验证会返回 403 FreeTierError（见 idgen.go 注释）。
func TestIsSession(t *testing.T) {
	valid := []string{
		"ses_ef5920720ffeDse8BpbCnsgiMT",
		"ses_000000000000AAAAAAAAAAAAAA",
		"ses_ffffffffffffzzzzzzzzzzzzzz",
		"ses_0123456789abABCDEFGHIJKLMN", // 12 位 hex + 14 位 base62（含大写）
	}
	for _, id := range valid {
		if !IsSession(id) {
			t.Errorf("IsSession(%q) = false, want true", id)
		}
	}

	invalid := map[string]string{
		"空串":              "",
		"hex 大写":          "ses_EF5920720FFEDse8BpbCnsgiMT",
		"hex 11 位":        "ses_ef5920720ffDse8BpbCnsgiMTX",
		"hex 13 位":        "ses_ef5920720fffeDse8BpbCnsgiMT",
		"hex 含非 hex 字符":   "ses_gf5920720ffgDse8BpbCnsgiMT",
		"base62 13 位":     "ses_ef5920720ffeDse8BpbCnsgiM",
		"base62 15 位":     "ses_ef5920720ffeDse8BpbCnsgiMTX",
		"base62 含下划线":     "ses_ef5920720ffeDse8BpbCnsgiM_",
		"base62 含连字符":     "ses_ef5920720ffeDse8BpbCnsgiM-",
		"前缀错配 msg":        "msg_ef5920720ffeDse8BpbCnsgiMT",
		"缺少前缀":            "ef5920720ffeDse8BpbCnsgiMT",
		"缺少下划线":           "sesef5920720ffeDse8BpbCnsgiMT",
		"UUID 形态":         "550e8400-e29b-41d4-a716-446655440000",
		"OpenClaw turnId": "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"含空格":             "    ses_ef5920720ffeDse8BpbCnsgiMT",
		"普通字符串":           "claude-code-session-abc123",
	}
	for label, id := range invalid {
		if IsSession(id) {
			t.Errorf("IsSession(%q) = true (%s), want false", id, label)
		}
	}
}

func TestIsMessage(t *testing.T) {
	if !IsMessage("msg_ef5920720ffc83HCMpIIJDrDdM") {
		t.Error("standard msg_ id should be valid")
	}
	for _, id := range []string{
		"", "msg_abc",
		"ses_ef5920720ffc83HCMpIIJDrDdM", // 前缀错配
		"msg_EF5920720FFC83HCMpIIJDrDdM", // hex 大写
		"550e8400-e29b-41d4-a716-446655440000",
	} {
		if IsMessage(id) {
			t.Errorf("IsMessage(%q) = true, want false", id)
		}
	}
	// session 与 message 的校验规则一致（仅前缀不同）
	if IsMessage("msg_ef5920720ffeDse8BpbCnsgiMT") == false {
		t.Error("same body length should be accepted for msg_ prefix")
	}
}

// TestGeneratedIDsAlwaysValid 验证生成器产物永远通过自己的校验，
// 否则「生成合法值」这一兜底就失去意义。
func TestGeneratedIDsAlwaysValid(t *testing.T) {
	for i := 0; i < 2000; i++ {
		if s := Session(); !IsSession(s) {
			t.Fatalf("generated session invalid: %q", s)
		}
		if m := Message(); !IsMessage(m) {
			t.Fatalf("generated message invalid: %q", m)
		}
	}
}

// TestGeneratedIDsHaveCorrectShape 校验长度与字符集（防止生成器与校验器同时写错）。
func TestGeneratedIDsHaveCorrectShape(t *testing.T) {
	s := Session()
	if len(s) != fullLen {
		t.Errorf("session length = %d, want %d", len(s), fullLen)
	}
	if s[:4] != "ses_" {
		t.Errorf("session prefix = %q", s[:4])
	}
	for i := 4; i < 4+hexLen; i++ {
		if !isLowerHex(s[i]) {
			t.Errorf("byte %d (%q) is not lowercase hex", i, s[i])
		}
	}
	for i := 4 + hexLen; i < fullLen; i++ {
		if !isBase62(s[i]) {
			t.Errorf("byte %d (%q) is not base62", i, s[i])
		}
	}
}
