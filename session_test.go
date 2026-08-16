package ghostline_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/abcdlsj/ghostline"
)

func waitForSpool(t *testing.T, session ghostline.Session, needle string) {
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

func TestStartConfiguresSizeAndEnvironment(t *testing.T) {
	hub := newHub(t, ghostline.Options{
		DefaultSize: ghostline.Size{Columns: 90, Rows: 28},
	})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:        "configured",
		Directory:   t.TempDir(),
		Command:     "sh",
		Environment: []string{"GHOSTLINE_TEST=value", "TERM=custom-term"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Input(context.Background(), []byte("stty size; echo env=$GHOSTLINE_TEST term=$TERM\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitForSpool(t, session, "28 90")
	waitForSpool(t, session, "env=value term=custom-term")
}

func TestSessionWatchOutputAndWait(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "watch",
		Directory: t.TempDir(),
		Command:   "printf 'watched-output\\r\\n'; exit 7",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	output := make(chan string, 1)
	watcher, err := session.WatchOutput(ghostline.WatchOptions{
		OnOutput: func(data []byte) {
			select {
			case output <- string(data):
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("WatchOutput: %v", err)
	}
	defer watcher.Close()

	select {
	case got := <-output:
		if !bytes.Contains([]byte(got), []byte("watched-output")) {
			t.Fatalf("output = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for output")
	}
	waitErr := session.Wait(context.Background())
	var exitErr *ghostline.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Wait error = %v, want exit code 7", waitErr)
	}
}

func TestSessionCheckpointMatchesSpoolBoundary(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "checkpoint",
		Directory: t.TempDir(),
		Command:   "sh",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Input(context.Background(), []byte("printf 'checkpoint-output\\r\\n'\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitForSpool(t, session, "checkpoint-output")
	checkpoint, err := session.Checkpoint(context.Background())
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !bytes.Contains(checkpoint.Replay, []byte("checkpoint-output")) {
		t.Fatalf("replay missing output: %q", checkpoint.Replay)
	}
	size, err := session.SpoolSize(context.Background())
	if err != nil {
		t.Fatalf("SpoolSize: %v", err)
	}
	if checkpoint.Offset != size {
		t.Fatalf("checkpoint offset = %d, spool size = %d", checkpoint.Offset, size)
	}
}

func TestSessionWaitCancellationDoesNotTerminateChild(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "wait-cancel",
		Directory: t.TempDir(),
		Command:   "sleep 30",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := session.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %v", err)
	}
	if !session.Alive() {
		t.Fatal("canceling Wait terminated the session")
	}
}

func TestErrorsAreInspectable(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	options := ghostline.SessionOptions{Name: "duplicate", Directory: t.TempDir(), Command: "sleep 30"}
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
		Name:      "invalid-resize",
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
			t.Fatalf("Resize accepted invalid size %+v", size)
		}
	}
}

func TestSessionsIncludesStoppedAndIsOrdered(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	first, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "session-b",
		Directory: t.TempDir(),
		Command:   "sleep 30",
	})
	if err != nil {
		t.Fatalf("Start first: %v", err)
	}
	second, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "session-a",
		Directory: t.TempDir(),
		Command:   "sleep 30",
	})
	if err != nil {
		t.Fatalf("Start second: %v", err)
	}
	sessions := hub.Sessions()
	if len(sessions) != 2 || sessions[0].Name() != first.Name() || sessions[1].Name() != second.Name() {
		t.Fatalf("Sessions ordering = %q, %q", sessions[0].Name(), sessions[1].Name())
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	sessions = hub.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("Sessions after Close = %d, want 2", len(sessions))
	}
	if handle, ok := hub.Session(first.Name()); !ok {
		t.Fatal("stopped session lookup failed")
	} else if handle.Alive() {
		t.Fatal("stopped session reported alive")
	}
}

func TestSessionHandleAfterHubClose(t *testing.T) {
	hub := newHub(t, ghostline.Options{})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "after-close",
		Directory: t.TempDir(),
		Command:   "sleep 30",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := session.Input(context.Background(), []byte("x")); !errors.Is(err, ghostline.ErrSessionClosed) {
		t.Fatalf("Input after hub close = %v", err)
	}
	if session.Alive() {
		t.Fatal("session reported alive after hub close")
	}
}
