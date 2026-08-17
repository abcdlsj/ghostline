package ghostline

import (
	"context"
	"testing"
)

func TestHubResolvesVTScrollbackConfiguration(t *testing.T) {
	hub, err := New(Options{
		OutputDir:            t.TempDir(),
		VTScrollbackMaxBytes: 3 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hub.Close()

	defaultSession, err := hub.Start(context.Background(), SessionOptions{
		Name:    "scrollback-default",
		Command: "sleep 30",
	})
	if err != nil {
		t.Fatalf("start default session: %v", err)
	}
	if got := defaultSession.(*localSession).state.scrollbackMaxBytes; got != 3<<20 {
		t.Fatalf("default scrollback = %d, want %d", got, 3<<20)
	}

	overrideSession, err := hub.Start(context.Background(), SessionOptions{
		Name:                 "scrollback-override",
		Command:              "sleep 30",
		VTScrollbackMaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("start override session: %v", err)
	}
	if got := overrideSession.(*localSession).state.scrollbackMaxBytes; got != 1<<20 {
		t.Fatalf("override scrollback = %d, want %d", got, 1<<20)
	}
}
