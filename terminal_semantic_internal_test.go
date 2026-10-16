package ghostline

import (
	"strings"
	"testing"
	"time"
)

func requireShellMetadata(t *testing.T, vt *vtTerminal) *ShellMetadata {
	t.Helper()
	metadata := vt.ShellMetadata()
	if metadata == nil {
		t.Fatal("ShellMetadata is nil, want semantic prompt state")
	}
	return metadata
}

func TestVTTerminalSemanticUnknownWithoutMarkers(t *testing.T) {
	vt := newTestVTTerminal(t, vtTerminalOptions{})
	defer vt.Close()
	vt.Feed([]byte("plain output\r\ntext\r\n"))
	if metadata := vt.ShellMetadata(); metadata != nil {
		t.Fatalf("ShellMetadata = %+v, want nil without OSC 133 markers", metadata)
	}
}

func TestVTTerminalSemanticTracksPromptAndCommand(t *testing.T) {
	vt := newTestVTTerminal(t, vtTerminalOptions{})
	defer vt.Close()

	vt.Feed([]byte("\x1b]133;A\x07$ \x1b]133;B\x07echo hi"))
	metadata := requireShellMetadata(t, vt)
	if !metadata.AtPrompt {
		t.Fatalf("AtPrompt = false at the prompt, want true")
	}
	if metadata.RunningCommand != "" || metadata.LastCommand != "" {
		t.Fatalf("command state before start = %+v, want empty", metadata)
	}

	vt.Feed([]byte("\r\n\x1b]133;C\x07"))
	metadata = requireShellMetadata(t, vt)
	if metadata.AtPrompt {
		t.Fatalf("AtPrompt = true while a command runs, want false")
	}
	if metadata.RunningCommand != "echo hi" {
		t.Fatalf("RunningCommand = %q, want %q", metadata.RunningCommand, "echo hi")
	}

	vt.Feed([]byte("hi\r\n\x1b]133;D;3\x07\x1b]133;A\x07$ \x1b]133;B\x07"))
	metadata = requireShellMetadata(t, vt)
	if !metadata.AtPrompt {
		t.Fatalf("AtPrompt = false after the next prompt, want true")
	}
	if metadata.RunningCommand != "" {
		t.Fatalf("RunningCommand = %q after completion, want empty", metadata.RunningCommand)
	}
	if metadata.LastCommand != "echo hi" {
		t.Fatalf("LastCommand = %q, want %q", metadata.LastCommand, "echo hi")
	}
	if metadata.LastExitCode == nil || *metadata.LastExitCode != 3 {
		t.Fatalf("LastExitCode = %v, want 3", metadata.LastExitCode)
	}
	if metadata.LastExitAt == nil {
		t.Fatal("LastExitAt = nil, want the OSC 133;D time")
	}
}

func TestVTTerminalSemanticJoinsContinuationLines(t *testing.T) {
	vt := newTestVTTerminalSize(t, 40, 12, vtTerminalOptions{})
	defer vt.Close()
	vt.Feed([]byte("\x1b]133;A\x07$ \x1b]133;B\x07echo foo \\\r\n  && echo bar"))
	vt.Feed([]byte("\r\n\x1b]133;C\x07"))
	metadata := requireShellMetadata(t, vt)
	if metadata.RunningCommand != `echo foo \ && echo bar` {
		t.Fatalf("RunningCommand = %q, want the continuation joined by a space", metadata.RunningCommand)
	}
}

func TestVTTerminalSemanticJoinsSoftWrapWithoutSpace(t *testing.T) {
	vt := newTestVTTerminalSize(t, 20, 12, vtTerminalOptions{})
	defer vt.Close()
	command := "echo " + strings.Repeat("a", 40)
	vt.Feed([]byte("\x1b]133;A\x07$ \x1b]133;B\x07" + command))
	vt.Feed([]byte("\x1b]133;C\x07"))
	metadata := requireShellMetadata(t, vt)
	if metadata.RunningCommand != command {
		t.Fatalf("RunningCommand = %q, want %q", metadata.RunningCommand, command)
	}
}

func TestVTTerminalSemanticDropsOversizedCommand(t *testing.T) {
	vt := newTestVTTerminalSize(t, 200, 100, vtTerminalOptions{})
	defer vt.Close()
	command := strings.Repeat("a", maxSemanticCommandBytes+512)
	vt.Feed([]byte("\x1b]133;A\x07$ \x1b]133;B\x07" + command))
	vt.Feed([]byte("\x1b]133;C\x07"))
	metadata := requireShellMetadata(t, vt)
	if metadata.RunningCommand != "" {
		t.Fatalf("RunningCommand was truncated instead of dropped: %d bytes", len(metadata.RunningCommand))
	}
}

func TestVTTerminalSemanticKeepsCommandsSeparate(t *testing.T) {
	vt := newTestVTTerminalSize(t, 40, 12, vtTerminalOptions{})
	defer vt.Close()
	// Two adjacent prompts with no output between them must not merge the
	// previous command into the new one.
	vt.Feed([]byte("\x1b]133;A\x07$ \x1b]133;B\x07true\r\n\x1b]133;C\x07\x1b]133;D;0\x07\x1b]133;A\x07$ \x1b]133;B\x07"))
	vt.Feed([]byte("ls\x1b]133;C\x07"))
	metadata := requireShellMetadata(t, vt)
	if metadata.RunningCommand != "ls" {
		t.Fatalf("RunningCommand = %q, want %q", metadata.RunningCommand, "ls")
	}
}

func TestVTTerminalSemanticUnknownWithoutPrimaryPrompt(t *testing.T) {
	vt := newTestVTTerminal(t, vtTerminalOptions{})
	defer vt.Close()
	// Input mode without a prompt start has no primary prompt row to anchor the
	// region, so the command text stays unknown instead of guessing.
	vt.Feed([]byte("\x1b]133;B\x07echo hi\r\n\x1b]133;C\x07"))
	metadata := requireShellMetadata(t, vt)
	if metadata.RunningCommand != "" {
		t.Fatalf("RunningCommand = %q, want unknown", metadata.RunningCommand)
	}
}

func TestVTTerminalSetShellMetadata(t *testing.T) {
	vt := newTestVTTerminal(t, vtTerminalOptions{})
	defer vt.Close()
	if metadata := vt.ShellMetadata(); metadata != nil {
		t.Fatalf("ShellMetadata = %+v before seeding, want nil", metadata)
	}
	code := 42
	at := time.Unix(1700000000, 0)
	vt.SetShellMetadata(&ShellMetadata{
		AtPrompt:       false,
		RunningCommand: "make test",
		LastCommand:    "go vet",
		LastExitCode:   &code,
		LastExitAt:     &at,
	})
	metadata := requireShellMetadata(t, vt)
	if metadata.AtPrompt || metadata.RunningCommand != "make test" || metadata.LastCommand != "go vet" {
		t.Fatalf("seeded metadata = %+v", metadata)
	}
	if metadata.LastExitCode == nil || *metadata.LastExitCode != 42 {
		t.Fatalf("seeded LastExitCode = %v, want 42", metadata.LastExitCode)
	}
	if metadata.LastExitAt == nil || !metadata.LastExitAt.Equal(at) {
		t.Fatalf("seeded LastExitAt = %v, want %v", metadata.LastExitAt, at)
	}
}
