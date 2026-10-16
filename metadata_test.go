package ghostline_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abcdlsj/ghostline"
)

func TestMetadataDisabledByDefault(t *testing.T) {
	hub, err := ghostline.New(ghostline.Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:    "metadata-off",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	metadata, err := session.Metadata(context.Background())
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if metadata.Process != "" || metadata.CommandLine != "" {
		t.Fatalf("Metadata = %+v, want no foreground data when probing is disabled", metadata)
	}
}

func TestMetadataEnabledReportsForegroundProcess(t *testing.T) {
	directory := t.TempDir()
	hub, err := ghostline.New(ghostline.Options{
		OutputDir:       t.TempDir(),
		ProbeForeground: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:    "metadata-on",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: directory},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForForegroundMetadata(t, session, directory, "sleep")
}

func TestRemoteMetadataEnabledReportsForegroundProcess(t *testing.T) {
	_, client := startTestServerWithOptions(t, ghostline.Options{
		OutputDir:       t.TempDir(),
		ProbeForeground: true,
	})
	directory := t.TempDir()
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name:    "metadata-remote",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: directory},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Terminate(context.Background()) })
	waitForForegroundMetadata(t, session, directory, "sleep")
}

// TestMetadataReportsOSC7PwdWithoutProbe verifies that the working directory is
// collected from the parsed output stream even when OS foreground probing is
// disabled.
func TestMetadataReportsOSC7PwdWithoutProbe(t *testing.T) {
	directory := t.TempDir()
	hub, err := ghostline.New(ghostline.Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:    "metadata-pwd",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: directory},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte(reportPwdCommand(directory)+"\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitForReportedPwd(t, session, directory)
}

func waitForForegroundMetadata(t *testing.T, session *ghostline.Session, directory, processNeedle string) {
	t.Helper()
	ctx := context.Background()
	command := "cd " + shellQuote(directory) + " && " + reportPwdCommand("$PWD") + " && exec sleep 30\r"
	if err := session.WriteInput(ctx, []byte(command)); err != nil {
		t.Fatalf("Input: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var last ghostline.SessionMetadata
	for time.Now().Before(deadline) {
		metadata, err := session.Metadata(ctx)
		if err != nil {
			t.Fatalf("Metadata: %v", err)
		}
		last = metadata
		if strings.Contains(metadata.Process, processNeedle) &&
			strings.Contains(metadata.CommandLine, processNeedle) &&
			samePath(metadata.Directory, directory) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("metadata did not converge: last=%+v", last)
}

func waitForReportedPwd(t *testing.T, session *ghostline.Session, directory string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	var last ghostline.SessionMetadata
	for time.Now().Before(deadline) {
		metadata, err := session.Metadata(ctx)
		if err != nil {
			t.Fatalf("Metadata: %v", err)
		}
		last = metadata
		if samePath(metadata.Directory, directory) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("reported pwd did not converge: last=%+v", last)
}

// reportPwdCommand prints an OSC 7 file URI for the given path expression.
// The single-quoted printf format keeps the escape bytes out of the shell
// parser while still emitting them to the PTY.
func reportPwdCommand(pathExpression string) string {
	return "printf '\\033]7;file://localhost%s\\033\\\\' \"" + pathExpression + "\""
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func samePath(got, want string) bool {
	if got == "" {
		return false
	}
	resolvedGot, gotErr := filepath.EvalSymlinks(got)
	resolvedWant, wantErr := filepath.EvalSymlinks(want)
	if gotErr != nil || wantErr != nil {
		return got == want
	}
	return resolvedGot == resolvedWant
}

// semanticPromptCommand builds a shell command line that drives OSC 133 by
// hand: it emits a prompt, switches to input mode, prints the command text the
// way a shell integration would, starts command mode, runs the command, then
// reports the real exit status and redraws a prompt. The script is sent as the
// first command of a session so the shell's own echo is ordinary output rather
// than semantic input.
func semanticPromptCommand(command string) string {
	return "printf '\\033]133;A\\007$ \\033]133;B\\007" + command + "\\r\\n\\033]133;C\\007'; " +
		command + "; printf '\\033]133;D;%s\\007\\033]133;A\\007$ \\033]133;B\\007' \"$?\""
}

// semanticLongCommand emits the same prompt/input/command markers but leaves
// the named command running so metadata can be observed mid-command.
func semanticLongCommand(command string) string {
	return "printf '\\033]133;A\\007$ \\033]133;B\\007" + command + "\\r\\n\\033]133;C\\007'; " + command
}

func waitForShellMetadata(t *testing.T, session *ghostline.Session, done func(*ghostline.ShellMetadata) bool) *ghostline.ShellMetadata {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	var last ghostline.SessionMetadata
	for time.Now().Before(deadline) {
		metadata, err := session.Metadata(ctx)
		if err != nil {
			t.Fatalf("Metadata: %v", err)
		}
		last = metadata
		if metadata.Shell != nil && done(metadata.Shell) {
			return metadata.Shell
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("semantic prompt metadata did not converge: last=%+v", last)
	return nil
}

func TestMetadataReportsSemanticCommandState(t *testing.T) {
	hub, err := ghostline.New(ghostline.Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:    "metadata-semantic",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte(semanticPromptCommand("false")+"\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	shell := waitForShellMetadata(t, session, func(metadata *ghostline.ShellMetadata) bool {
		return metadata.LastCommand == "false" && metadata.LastExitCode != nil && *metadata.LastExitCode == 1
	})
	if !shell.AtPrompt {
		t.Fatalf("AtPrompt = false after the command finished, want true: %+v", shell)
	}
	if shell.RunningCommand != "" {
		t.Fatalf("RunningCommand = %q after completion, want empty", shell.RunningCommand)
	}
	if shell.LastExitAt == nil {
		t.Fatal("LastExitAt = nil, want the OSC 133;D time")
	}
}

func TestMetadataReportsRunningSemanticCommand(t *testing.T) {
	hub, err := ghostline.New(ghostline.Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:    "metadata-running",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte(semanticLongCommand("sleep 30")+"\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	shell := waitForShellMetadata(t, session, func(metadata *ghostline.ShellMetadata) bool {
		return metadata.RunningCommand == "sleep 30"
	})
	if shell.AtPrompt {
		t.Fatalf("AtPrompt = true while sleep runs, want false: %+v", shell)
	}
	if shell.LastExitCode != nil {
		t.Fatalf("LastExitCode = %v before a command finished, want nil", shell.LastExitCode)
	}
}

func TestMetadataReportsSemanticStateOverRPC(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name:    "metadata-semantic-remote",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Terminate(context.Background()) })
	if err := session.WriteInput(context.Background(), []byte(semanticPromptCommand("true")+"\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	shell := waitForShellMetadata(t, session, func(metadata *ghostline.ShellMetadata) bool {
		return metadata.LastCommand == "true" && metadata.LastExitCode != nil && *metadata.LastExitCode == 0
	})
	if !shell.AtPrompt {
		t.Fatalf("AtPrompt = false over RPC, want true: %+v", shell)
	}
}

func TestMetadataSemanticMarkersAbsentMeansUnknown(t *testing.T) {
	hub, err := ghostline.New(ghostline.Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:    "metadata-no-markers",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("echo SEMANTIC-NOMARK\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitForReplay(t, session, "SEMANTIC-NOMARK")
	metadata, err := session.Metadata(context.Background())
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if metadata.Shell != nil {
		t.Fatalf("Shell = %+v without OSC 133 markers, want unknown (nil)", metadata.Shell)
	}
}
