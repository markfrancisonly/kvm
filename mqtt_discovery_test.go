package kvm

import "testing"

func TestMacroObjectID(t *testing.T) {
	cases := map[string]string{
		"macro-1761590000000000000": "macro_macro-1761590000000000000",
		"login_root":                "macro_login_root",
		"with space/slash":          "macro_with_space_slash",
	}
	for id, want := range cases {
		if got := macroObjectID(id); got != want {
			t.Errorf("macroObjectID(%q) = %q, want %q", id, got, want)
		}
	}
}
