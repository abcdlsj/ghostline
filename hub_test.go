package ghostline_test

import (
	"bytes"
	"context"
	"errors"
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

func waitSpool(t *testing.T, session ghostline.Session, needle string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(session.SpoolPath())
		if err == nil && bytes.Contains(data, []byte(needle)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(session.SpoolPath())
	t.Fatalf("spool did not contain %q; got %q", needle, data)
}

func TestHubStartWritesSpoolAndCapture(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "capture",
		Directory: t.TempDir(),
		Command:   "printf 'hello-hub\\r\\n'",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitSpool(t, session, "hello-hub")

	snapshot, err := session.Snapshot(context.Background())
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
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "styles",
		Directory: t.TempDir(),
		Command:   command,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitSpool(t, session, "line-25")

	snapshot, err := session.Snapshot(context.Background())
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

func TestHubInputReachesChild(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "input",
		Directory: t.TempDir(),
		Command:   "sh",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Input(context.Background(), []byte("echo hub-input-ok\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitSpool(t, session, "hub-input-ok")
}

func TestHubChildInheritsEnvironment(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:        "env",
		Directory:   t.TempDir(),
		Command:     "sh",
		Environment: []string{"GHOSTLINE_TEST=value", "TERM=custom-term", "NO_COLOR="},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Input(context.Background(), []byte("echo TERM=$TERM NO_COLOR=$NO_COLOR TEST=$GHOSTLINE_TEST\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitSpool(t, session, "TERM=custom-term NO_COLOR= TEST=value")
}

func TestSessionRecoverReadsSpoolRange(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "recover",
		Directory: t.TempDir(),
		Command:   "sh",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitSpool(t, session, "$")
	offset := spoolSizeOf(t, session)
	if err := session.Input(context.Background(), []byte("echo recover-tail-ok\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitSpool(t, session, "recover-tail-ok")
	data, err := session.Recover(context.Background(), offset, spoolSizeOf(t, session))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !bytes.Contains(data, []byte("recover-tail-ok")) {
		t.Fatalf("recovered bytes missing output: %q", data)
	}
}

func TestSessionResize(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "resize",
		Directory: t.TempDir(),
		Command:   "sh",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Resize(context.Background(), ghostline.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := session.Input(context.Background(), []byte("stty size\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitSpool(t, session, "24 80")
}

func TestHubRejectsInvalidSizes(t *testing.T) {
	if hub, err := ghostline.New(ghostline.Options{DefaultSize: ghostline.Size{Columns: 0, Rows: 24}}); err == nil {
		_ = hub.Close()
		t.Fatal("New accepted a partial default size")
	}
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "sizes",
		Directory: t.TempDir(),
		Command:   "sleep 30",
	})
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
			if _, err := hub.Start(ctx, ghostline.SessionOptions{
				Name: "duplicate", Directory: workDir, Command: "sleep 30",
			}); err == nil {
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
		if _, err := hub.Start(context.Background(), ghostline.SessionOptions{
			Name: name, Directory: t.TempDir(), Command: "sleep 30",
		}); !errors.Is(err, ghostline.ErrInvalidSessionName) {
			t.Errorf("Start(%q) error = %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outputDir), "escape.out")); !os.IsNotExist(err) {
		t.Fatalf("unsafe name escaped the output directory: %v", err)
	}
}

func TestHubStartCleansUpAfterSpoolFailure(t *testing.T) {
	outputDir := t.TempDir()
	hub, err := ghostline.New(ghostline.Options{OutputDir: outputDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	name := "spool-failure"
	if err := os.MkdirAll(filepath.Join(outputDir, name+".out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: name, Directory: t.TempDir(), Command: "sleep 30",
	}); err == nil {
		t.Fatal("Start succeeded despite an unwritable spool path")
	}
	if _, ok := hub.Session(name); ok {
		t.Fatal("failed Start left a session behind")
	}
}

func TestHubCloseTerminatesAndPreventsStart(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "close-hub", Directory: t.TempDir(), Command: "sleep 30",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if session.Alive() {
		t.Fatal("session alive after hub close")
	}
	if _, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "after-close", Directory: t.TempDir(), Command: "sleep 30",
	}); !errors.Is(err, ghostline.ErrClosed) {
		t.Fatalf("Start after close = %v", err)
	}
}

func TestSessionCloseKeepsRecordAndRemoveDeletes(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "lifecycle", Directory: t.TempDir(), Command: "sleep 30",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := hub.Session(session.Name()); !ok {
		t.Fatal("record disappeared after Close")
	}
	if err := session.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := hub.Session(session.Name()); ok {
		t.Fatal("record remained after Remove")
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSessionHandleCannotAffectReplacement(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	oldSession, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "reused", Directory: t.TempDir(), Command: "sleep 30",
	})
	if err != nil {
		t.Fatalf("Start old: %v", err)
	}
	if err := oldSession.Close(); err != nil {
		t.Fatalf("Close old: %v", err)
	}
	if err := oldSession.Remove(); err != nil {
		t.Fatalf("Remove old: %v", err)
	}
	newSession, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "reused", Directory: t.TempDir(), Command: "sleep 30",
	})
	if err != nil {
		t.Fatalf("Start replacement: %v", err)
	}
	if err := oldSession.Input(context.Background(), []byte("x")); !errors.Is(err, ghostline.ErrSessionClosed) {
		t.Fatalf("old Input = %v", err)
	}
	if !newSession.Alive() {
		t.Fatal("replacement session not alive")
	}
}

func TestHubWaitReturnsExitError(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "wait", Directory: t.TempDir(), Command: "exit 7",
	})
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
	old, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "watcher", Directory: t.TempDir(), Command: "printf 'old-marker\\r\\n'",
	})
	if err != nil {
		t.Fatalf("Start old: %v", err)
	}
	output := make(chan []byte, 64)
	watcher, err := old.WatchOutput(ghostline.WatchOptions{
		OnOutput: func(data []byte) {
			output <- append([]byte(nil), data...)
		},
	})
	if err != nil {
		t.Fatalf("WatchOutput: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case data := <-output:
			if bytes.Contains(data, []byte("old-marker")) {
				goto oldSeen
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("old watcher never saw its output")
		}
	}
oldSeen:
	if err := old.Close(); err != nil {
		t.Fatalf("Close old: %v", err)
	}
	if err := old.Remove(); err != nil {
		t.Fatalf("Remove old: %v", err)
	}
	replacement, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "watcher", Directory: t.TempDir(), Command: "printf 'fresh-marker\\r\\n'",
	})
	if err != nil {
		t.Fatalf("Start replacement: %v", err)
	}
	waitSpool(t, replacement, "fresh-marker")
	select {
	case data := <-output:
		if bytes.Contains(data, []byte("fresh-marker")) {
			t.Fatalf("old watcher received replacement output: %q", data)
		}
	case <-time.After(200 * time.Millisecond):
	}
	watcher.Close()
}

func spoolSizeOf(t *testing.T, session ghostline.Session) int64 {
	t.Helper()
	size, err := session.SpoolSize(context.Background())
	if err != nil {
		t.Fatalf("SpoolSize: %v", err)
	}
	return size
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d still alive", pid)
}
