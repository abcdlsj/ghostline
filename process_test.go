package ghostline

import (
	"strings"
	"testing"
)

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

func TestMergeTermDefaultAddsBuiltin(t *testing.T) {
	env := mergeTermDefault([]string{"HOME=/tmp"}, "")
	value, ok := envValue(env, "TERM")
	if !ok || value != "xterm-256color" {
		t.Fatalf("expected builtin TERM=xterm-256color, got %q (present=%v)", value, ok)
	}
}

func TestMergeTermDefaultUsesConfiguredDefault(t *testing.T) {
	env := mergeTermDefault([]string{"HOME=/tmp"}, "xterm-ghostty")
	value, ok := envValue(env, "TERM")
	if !ok || value != "xterm-ghostty" {
		t.Fatalf("expected configured TERM=xterm-ghostty, got %q (present=%v)", value, ok)
	}
}

func TestMergeTermDefaultKeepsInheritedTerm(t *testing.T) {
	env := mergeTermDefault([]string{"TERM=xterm"}, "xterm-ghostty")
	value, ok := envValue(env, "TERM")
	if !ok || value != "xterm" {
		t.Fatalf("expected inherited TERM=xterm to win, got %q (present=%v)", value, ok)
	}
}

func TestMergeTermDefaultReplacesEmptyTerm(t *testing.T) {
	env := mergeTermDefault([]string{"TERM=", "HOME=/tmp"}, "xterm-ghostty")
	value, ok := envValue(env, "TERM")
	if !ok || value != "xterm-ghostty" {
		t.Fatalf("expected empty TERM replaced with xterm-ghostty, got %q (present=%v)", value, ok)
	}
}

func TestMergeTermDefaultExplicitOverrideWins(t *testing.T) {
	base := mergeEnvironment([]string{"TERM=xterm", "HOME=/tmp"}, []string{"TERM=custom"})
	env := mergeTermDefault(base, "xterm-ghostty")
	value, ok := envValue(env, "TERM")
	if !ok || value != "custom" {
		t.Fatalf("expected explicit TERM=custom to win, got %q (present=%v)", value, ok)
	}
}
