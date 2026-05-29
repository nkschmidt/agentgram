package bot

import (
	"testing"
	"unicode/utf8"
)

func TestCleanStripsInvalidUTF8(t *testing.T) {
	// "текст" cut mid-rune: keep a valid prefix plus the first byte of "т".
	full := []byte("текст")
	cut := string(full[:7]) // 3 Cyrillic runes (6 bytes) + 1 dangling byte
	if utf8.ValidString(cut) {
		t.Fatal("precondition: cut string should be invalid UTF-8")
	}
	got := clean(cut)
	if !utf8.ValidString(got) {
		t.Fatalf("clean must return valid UTF-8, got %q", got)
	}
	if got != "тек" {
		t.Fatalf("want %q, got %q", "тек", got)
	}
}

func TestCleanLeavesValidUnchanged(t *testing.T) {
	const s = "Привет, мир! 🚀 code `x`"
	if got := clean(s); got != s {
		t.Fatalf("valid text changed: %q -> %q", s, got)
	}
}
