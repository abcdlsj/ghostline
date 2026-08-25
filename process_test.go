package ghostline

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
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

func TestOutputAppendFailureStopsSessionAndReachesWaitAndStatus(t *testing.T) {
	hub, err := New(Options{OutputDir: t.TempDir()})
	if errors.Is(err, ErrUnavailable) {
		t.Skip("libghostty-vt is unavailable for this build")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	session, err := hub.Start(context.Background(), SessionOptions{
		Name: "output-failure", Process: ProcessSpec{Path: "sh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := hub.session(session.Name())
	state.output.mu.Lock()
	err = state.output.active.Close()
	state.output.mu.Unlock()
	if err != nil {
		t.Fatalf("close active output: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("printf output-failure-trigger\r")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waitErr := session.Wait(ctx)
	if waitErr == nil || !strings.Contains(waitErr.Error(), "append output") {
		t.Fatalf("Wait error = %v, want append output failure", waitErr)
	}
	if _, statusErr := session.Status(context.Background()); statusErr == nil || !strings.Contains(statusErr.Error(), "append output") {
		t.Fatalf("Status error = %v, want append output failure", statusErr)
	}
	if _, migrationErr := sessionMetaOf(state); migrationErr == nil || !strings.Contains(migrationErr.Error(), "append output") {
		t.Fatalf("migration metadata error = %v, want append output failure", migrationErr)
	}
}

func TestSessionDeleteRemovesOutputStorage(t *testing.T) {
	hub, err := New(Options{OutputDir: t.TempDir()})
	if errors.Is(err, ErrUnavailable) {
		t.Skip("libghostty-vt is unavailable for this build")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	session, err := hub.Start(context.Background(), SessionOptions{
		Name: "delete-storage", Process: ProcessSpec{Path: "sleep", Args: []string{"30"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := hub.session(session.Name())
	directory, _ := state.output.metadata()
	if err := session.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output directory still exists after Delete: %v", err)
	}
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
