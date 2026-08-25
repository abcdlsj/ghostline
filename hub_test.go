package ghostline_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/abcdlsj/ghostline"
)

func newHub(t *testing.T, options ghostline.Options) *ghostline.Hub {
	t.Helper()
	if options.OutputDir == "" {
		options.OutputDir = t.TempDir()
	}
	hub, err := ghostline.New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := hub.Close(); err != nil {
			t.Errorf("Close hub: %v", err)
		}
	})
	return hub
}

func shellOptions(name, directory, command string) ghostline.SessionOptions {
	return ghostline.SessionOptions{
		Name: name,
		Process: ghostline.ProcessSpec{
			Directory: directory, ShellCommand: command,
		},
	}
}

func waitReplay(t *testing.T, session *ghostline.Session, needle string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := session.Replay(context.Background())
		if err == nil && bytes.Contains(data, []byte(needle)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := session.Replay(context.Background())
	t.Fatalf("replay did not contain %q; got %q", needle, data)
}

func TestHubStartWritesOutputAndReplay(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), shellOptions("capture", t.TempDir(), "printf 'hello-hub\\r\\n'"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReplay(t, session, "hello-hub")

	snapshot, err := session.Replay(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !bytes.HasPrefix(snapshot, []byte("\x1b[3J\x1b[2J\x1b[H")) {
		t.Fatalf("snapshot missing reset prefix: %q", snapshot[:min(len(snapshot), 16)])
	}
	if !bytes.Contains(snapshot, []byte("hello-hub")) {
		t.Fatalf("snapshot missing output: %q", snapshot)
	}
}

func TestHubCapturePreservesStyleAndScrollback(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	command := "printf '\\033[31mRED\\033[0m\\n'; i=1; while [ $i -le 25 ]; do echo line-$i; i=$((i+1)); done"
	session, err := hub.Start(context.Background(), shellOptions("styles", t.TempDir(), command))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReplay(t, session, "line-25")

	snapshot, err := session.Replay(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !bytes.Contains(snapshot, []byte("\x1b[38;5;1mRED")) {
		t.Fatalf("snapshot missing red SGR: %q", snapshot)
	}
	if !bytes.Contains(snapshot, []byte("line-1")) {
		t.Fatalf("snapshot missing scrollback head: %q", snapshot)
	}
}

func TestHubCaptureEmitsCursorPosition(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), shellOptions("cursor", t.TempDir(), "printf abc"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReplay(t, session, "abc")

	snapshot, err := session.Replay(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !bytes.HasSuffix(snapshot, []byte("\x1b[1;4H")) {
		t.Fatalf("snapshot does not restore cursor after content: %q", snapshot[len(snapshot)-min(len(snapshot), 32):])
	}
}

func TestHubInputReachesChild(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), shellOptions("input", t.TempDir(), "sh"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("echo hub-input-ok\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitReplay(t, session, "hub-input-ok")
}

func TestHubChildInheritsEnvironment(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "env",
		Process: ghostline.ProcessSpec{
			Path: "sh", Directory: t.TempDir(),
			Environment: []string{"GHOSTLINE_TEST=value", "TERM=custom-term", "NO_COLOR="},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("echo TERM=$TERM NO_COLOR=$NO_COLOR TEST=$GHOSTLINE_TEST\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitReplay(t, session, "TERM=custom-term NO_COLOR= TEST=value")
}

func TestSessionOutputReadsFromCheckpointCursor(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), shellOptions("recover", t.TempDir(), "sh"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReplay(t, session, "$")
	checkpoint, err := session.Checkpoint(context.Background())
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reader, err := session.Output(ctx, checkpoint.Cursor)
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	defer reader.Close()
	if err := session.WriteInput(context.Background(), []byte("echo recover-tail-ok\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	var data []byte
	buffer := make([]byte, 128)
	for !bytes.Contains(data, []byte("recover-tail-ok")) {
		n, readErr := reader.Read(buffer)
		data = append(data, buffer[:n]...)
		if readErr != nil {
			t.Fatalf("Read: %v", readErr)
		}
	}
	if !bytes.Contains(data, []byte("recover-tail-ok")) {
		t.Fatalf("recovered bytes missing output: %q", data)
	}
}

func TestSessionResize(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), shellOptions("resize", t.TempDir(), "sh"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Resize(context.Background(), ghostline.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("stty size\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitReplay(t, session, "24 80")
}

func TestHubRejectsInvalidSizes(t *testing.T) {
	if hub, err := ghostline.New(ghostline.Options{DefaultSize: ghostline.Size{Columns: 0, Rows: 24}}); err == nil {
		_ = hub.Close()
		t.Fatal("New accepted a partial default size")
	}
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), shellOptions("sizes", t.TempDir(), "sleep 30"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, size := range []ghostline.Size{
		{},
		{Columns: 0, Rows: 24},
		{Columns: 120, Rows: 0},
		{Columns: 1 << 16, Rows: 24},
	} {
		if err := session.Resize(context.Background(), size); err == nil {
			t.Fatalf("Resize accepted %+v", size)
		}
	}
}

func TestHubStartIsAtomicForDuplicateNames(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	ctx := context.Background()
	workDir := t.TempDir()
	const attempts = 8
	start := make(chan struct{})
	var successes atomic.Int32
	var wait sync.WaitGroup
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if _, err := hub.Start(ctx, shellOptions("duplicate", workDir, "sleep 30")); err == nil {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful duplicate starts = %d, want 1", got)
	}
}

func TestHubRejectsUnsafeNames(t *testing.T) {
	outputDir := t.TempDir()
	hub, err := ghostline.New(ghostline.Options{OutputDir: outputDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	for _, name := range []string{"", ".", "..", "../escape", `nested\\escape`} {
		if _, err := hub.Start(context.Background(), shellOptions(name, t.TempDir(), "sleep 30")); !errors.Is(err, ghostline.ErrInvalidSessionName) {
			t.Errorf("Start(%q) error = %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outputDir), "escape.out")); !os.IsNotExist(err) {
		t.Fatalf("unsafe name escaped the output directory: %v", err)
	}
}

func TestHubStartCleansUpAfterOutputLogFailure(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(outputDir, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	hub, err := ghostline.New(ghostline.Options{OutputDir: outputDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	name := "output-failure"
	if _, err := hub.Start(context.Background(), shellOptions(name, t.TempDir(), "sleep 30")); err == nil {
		t.Fatal("Start succeeded despite an invalid output directory")
	}
	if _, err := hub.Get(context.Background(), name); !errors.Is(err, ghostline.ErrSessionNotFound) {
		t.Fatal("failed Start left a session behind")
	}
}

func TestHubCloseTerminatesAndPreventsStart(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), shellOptions("close-hub", t.TempDir(), "sleep 30"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	status, err := session.Status(context.Background())
	if err != nil || status.Alive {
		t.Fatal("session alive after hub close")
	}
	if _, err := hub.Start(context.Background(), shellOptions("after-close", t.TempDir(), "sleep 30")); !errors.Is(err, ghostline.ErrClosed) {
		t.Fatalf("Start after close = %v", err)
	}
}

func TestSessionCloseKeepsRecordAndRemoveDeletes(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), shellOptions("lifecycle", t.TempDir(), "sleep 30"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Terminate(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := hub.Get(context.Background(), session.Name()); err != nil {
		t.Fatal("record disappeared after Close")
	}
	if err := session.Delete(context.Background()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := hub.Get(context.Background(), session.Name()); !errors.Is(err, ghostline.ErrSessionNotFound) {
		t.Fatal("record remained after Remove")
	}
	if err := session.Terminate(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSessionHandleCannotAffectReplacement(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	oldSession, err := hub.Start(context.Background(), shellOptions("reused", t.TempDir(), "sleep 30"))
	if err != nil {
		t.Fatalf("Start old: %v", err)
	}
	if err := oldSession.Terminate(context.Background()); err != nil {
		t.Fatalf("Close old: %v", err)
	}
	if err := oldSession.Delete(context.Background()); err != nil {
		t.Fatalf("Remove old: %v", err)
	}
	newSession, err := hub.Start(context.Background(), shellOptions("reused", t.TempDir(), "sleep 30"))
	if err != nil {
		t.Fatalf("Start replacement: %v", err)
	}
	if err := oldSession.WriteInput(context.Background(), []byte("x")); !errors.Is(err, ghostline.ErrSessionClosed) {
		t.Fatalf("old Input = %v", err)
	}
	if _, err := oldSession.Size(context.Background()); !errors.Is(err, ghostline.ErrSessionClosed) {
		t.Fatalf("old Size = %v", err)
	}
	if _, err := oldSession.OutputCursor(context.Background()); !errors.Is(err, ghostline.ErrSessionClosed) {
		t.Fatalf("old OutputCursor = %v", err)
	}
	if err := oldSession.Signal(context.Background(), syscall.SIGKILL); !errors.Is(err, ghostline.ErrSessionClosed) {
		t.Fatalf("old Signal = %v", err)
	}
	status, err := newSession.Status(context.Background())
	if err != nil || !status.Alive {
		t.Fatal("replacement session not alive")
	}
}

func TestHubWaitReturnsExitError(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), shellOptions("wait", t.TempDir(), "exit 7"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var exitErr *ghostline.ExitError
	if err := session.Wait(context.Background()); !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Wait = %v, want exit code 7", err)
	}
}

func TestWatcherDoesNotFollowReplacement(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	old, err := hub.Start(context.Background(), shellOptions("watcher", t.TempDir(), "printf 'old-marker\\r\\n'"))
	if err != nil {
		t.Fatalf("Start old: %v", err)
	}
	reader, err := old.Output(context.Background(), ghostline.Cursor{})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if err := old.Wait(context.Background()); err != nil {
		t.Fatalf("Wait old: %v", err)
	}
	oldOutput, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatalf("ReadAll old: %v", err)
	}
	if !bytes.Contains(oldOutput, []byte("old-marker")) {
		t.Fatalf("old output missing marker: %q", oldOutput)
	}
	if err := old.Delete(context.Background()); err != nil {
		t.Fatalf("Remove old: %v", err)
	}
	replacement, err := hub.Start(context.Background(), shellOptions("watcher", t.TempDir(), "printf 'fresh-marker\\r\\n'"))
	if err != nil {
		t.Fatalf("Start replacement: %v", err)
	}
	waitReplay(t, replacement, "fresh-marker")
	if bytes.Contains(oldOutput, []byte("fresh-marker")) {
		t.Fatalf("old reader received replacement output: %q", oldOutput)
	}
}
