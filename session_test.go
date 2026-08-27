package ghostline_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/abcdlsj/ghostline"
)

type fakeSignal struct{}

func (fakeSignal) Signal()        {}
func (fakeSignal) String() string { return "fake" }

func TestSessionSignalValidationAndCancellation(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "signal-validation", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, signal := range []os.Signal{nil, fakeSignal{}, syscall.Signal(0)} {
		if err := session.Signal(context.Background(), signal); !errors.Is(err, ghostline.ErrInvalidSignal) {
			t.Fatalf("Signal(%v) = %v, want ErrInvalidSignal", signal, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.Signal(ctx, syscall.SIGCONT); !errors.Is(err, context.Canceled) {
		t.Fatalf("Signal canceled context = %v", err)
	}
}

func waitForReplay(t *testing.T, session *ghostline.Session, needle string) {
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

func TestStartConfiguresSizeAndEnvironment(t *testing.T) {
	hub := newHub(t, ghostline.Options{
		DefaultSize: ghostline.Size{Columns: 90, Rows: 28},
	})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "configured",
		Process: ghostline.ProcessSpec{
			Path: "sh", Directory: t.TempDir(),
			Environment: []string{"GHOSTLINE_TEST=value", "TERM=custom-term"},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("stty size; echo env=$GHOSTLINE_TEST term=$TERM\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitForReplay(t, session, "28 90")
	waitForReplay(t, session, "env=value term=custom-term")
}

func TestSessionOutputAndWait(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "watch", Process: ghostline.Shell("printf 'watched-output\\r\\n'; exit 7"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	reader, err := session.Output(context.Background(), ghostline.Cursor{})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	defer reader.Close()
	waitErr := session.Wait(context.Background())
	var exitErr *ghostline.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Wait error = %v, want exit code 7", waitErr)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Contains(output, []byte("watched-output")) {
		t.Fatalf("output = %q", output)
	}
}

func TestSessionCheckpointMatchesOutputBoundary(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "checkpoint", Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("printf 'checkpoint-output\\r\\n'\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitForReplay(t, session, "checkpoint-output")
	checkpoint, err := session.Checkpoint(context.Background())
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !bytes.Contains(checkpoint.Replay, []byte("checkpoint-output")) {
		t.Fatalf("replay missing output: %q", checkpoint.Replay)
	}
	if checkpoint.Cursor.String() == "" {
		t.Fatal("checkpoint returned zero cursor")
	}
}

func TestSessionAtomicStateReturnsOpaqueVTStateAndCursor(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:    "atomic-state",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("printf 'atomic-state-output\\r\\n'\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitForReplay(t, session, "atomic-state-output")
	state, err := session.AtomicState(context.Background())
	if err != nil {
		t.Fatalf("AtomicState: %v", err)
	}
	if state.Format != ghostline.AtomicStateFormat {
		t.Fatalf("AtomicState format = %q, want %q", state.Format, ghostline.AtomicStateFormat)
	}
	if len(state.Payload) == 0 {
		t.Fatal("AtomicState returned an empty payload")
	}
	if state.Cursor.String() == "" {
		t.Fatal("AtomicState returned a zero cursor")
	}
	if !bytes.HasPrefix(state.Payload, []byte("GHOSTSNP")) {
		t.Fatalf("AtomicState payload does not start with the Ghostty snapshot magic: %q", state.Payload[:min(len(state.Payload), 16)])
	}
}

func TestSessionWaitCancellationDoesNotTerminateChild(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "wait-cancel", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := session.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %v", err)
	}
	status, err := session.Status(context.Background())
	if err != nil || !status.Alive {
		t.Fatal("canceling Wait terminated the session")
	}
}

func TestErrorsAreInspectable(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	options := ghostline.SessionOptions{Name: "duplicate", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()}}
	if _, err := hub.Start(context.Background(), options); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := hub.Start(context.Background(), options); !errors.Is(err, ghostline.ErrSessionExists) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := hub.Start(context.Background(), ghostline.SessionOptions{Name: "../unsafe"}); !errors.Is(err, ghostline.ErrInvalidSessionName) {
		t.Fatalf("invalid name error = %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := hub.Start(context.Background(), ghostline.SessionOptions{Name: "closed"}); !errors.Is(err, ghostline.ErrClosed) {
		t.Fatalf("closed hub error = %v", err)
	}
}

func TestNewRejectsInvalidDefaultSize(t *testing.T) {
	for _, size := range []ghostline.Size{
		{Columns: 0, Rows: 24},
		{Columns: 120, Rows: 0},
		{Columns: -1, Rows: 24},
		{Columns: 1 << 16, Rows: 24},
	} {
		if hub, err := ghostline.New(ghostline.Options{DefaultSize: size}); err == nil {
			_ = hub.Close()
			t.Fatalf("New accepted invalid default size %+v", size)
		}
	}
}

func TestSessionResizeRejectsInvalidSizes(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "invalid-resize", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
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
			t.Fatalf("Resize accepted invalid size %+v", size)
		}
	}
}

func TestSessionsIncludesStoppedAndIsOrdered(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	first, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "session-b", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start first: %v", err)
	}
	second, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "session-a", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start second: %v", err)
	}
	sessions, err := hub.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 2 || sessions[0].Name() != first.Name() || sessions[1].Name() != second.Name() {
		t.Fatalf("Sessions ordering = %q, %q", sessions[0].Name(), sessions[1].Name())
	}
	if err := first.Terminate(context.Background()); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	sessions, err = hub.List(context.Background())
	if err != nil {
		t.Fatalf("List after Terminate: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("Sessions after Close = %d, want 2", len(sessions))
	}
	if handle, err := hub.Get(context.Background(), first.Name()); err != nil {
		t.Fatal("stopped session lookup failed")
	} else {
		status, statusErr := handle.Status(context.Background())
		if statusErr != nil || status.Alive {
			t.Fatal("stopped session reported alive")
		}
	}
}

func TestSessionHandleAfterHubClose(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "after-close", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("x")); !errors.Is(err, ghostline.ErrSessionClosed) {
		t.Fatalf("Input after hub close = %v", err)
	}
	status, err := session.Status(context.Background())
	if err != nil || status.Alive {
		t.Fatal("session reported alive after hub close")
	}
}
