package provider

import "testing"

// TestKindPathsAreStable 保证路由层与 provider 的路径约定一致（防止重构后错位）。
func TestKindPathsAreStable(t *testing.T) {
	cases := []struct {
		name string
		got  Kind
		want string
	}{
		{"chat", KindChat, "chat"},
		{"responses", KindResponses, "responses"},
		{"models", KindModels, "models"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("%s Kind = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestExtractBearer(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"abc":         "abc",
		"":            "",
		"Bearer ":     "",
		"  Bearer x ": "x",
	}
	for in, want := range cases {
		if got := extractBearer(in); got != want {
			t.Errorf("extractBearer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestActionConstants(t *testing.T) {
	if ActionPass == ActionRetrySame || ActionRetrySame == ActionCooldownAndNext {
		t.Fatal("actions must be distinct")
	}
}
