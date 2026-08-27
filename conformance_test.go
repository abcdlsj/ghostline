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

type sessionStore interface {
	Start(context.Context, ghostline.SessionOptions) (*ghostline.Session, error)
	Get(context.Context, string) (*ghostline.Session, error)
	List(context.Context) ([]*ghostline.Session, error)
}

func TestLocalAndDaemonSessionConformance(t *testing.T) {
	tests := []struct {
		name string
		new  func(*testing.T) sessionStore
	}{
		{
			name: "local",
			new: func(t *testing.T) sessionStore {
				return newHub(t, ghostline.Options{})
			},
		},
		{
			name: "daemon",
			new: func(t *testing.T) sessionStore {
				_, client := startTestServer(t)
				return client
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertSessionConformance(t, test.new(t))
		})
	}
}

func assertSessionConformance(t *testing.T, store sessionStore) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := store.Start(ctx, ghostline.SessionOptions{
		Name:    "conformance",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
		Size:    ghostline.Size{Columns: 90, Rows: 28},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if session.Name() != "conformance" || session.CreatedAt().IsZero() {
		t.Fatalf("identity = %+v", session.Info())
	}
	got, err := store.Get(ctx, session.Name())
	if err != nil || got.Info() != session.Info() {
		t.Fatalf("Get = (%+v, %v), want %+v", got, err, session.Info())
	}
	listed, err := store.List(ctx)
	if err != nil || len(listed) != 1 || listed[0].Info() != session.Info() {
		t.Fatalf("List = (%+v, %v)", listed, err)
	}
	status, err := session.Status(ctx)
	if err != nil || !status.Alive || status.Exit != nil {
		t.Fatalf("initial Status = (%+v, %v)", status, err)
	}
	if _, err := session.Metadata(ctx); err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if err := session.Resize(ctx, ghostline.Size{Columns: 100, Rows: 30}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if size, err := session.Size(ctx); err != nil || size != (ghostline.Size{Columns: 100, Rows: 30}) {
		t.Fatalf("Size = (%+v, %v)", size, err)
	}
	if err := session.WriteInput(ctx, []byte("trap 'printf \"signal-%s\\r\\n\" received' CONT; printf 'signal-%s\\r\\n' ready\r")); err != nil {
		t.Fatalf("install signal trap: %v", err)
	}
	waitForReplay(t, session, "signal-ready")
	if err := session.Signal(ctx, syscall.SIGCONT); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	// dash defers trapped signals while blocked in its interactive read until
	// another input byte wakes it; an empty line keeps this conformance check
	// portable without changing the signal API's process-group semantics.
	if err := session.WriteInput(ctx, []byte("\r")); err != nil {
		t.Fatalf("wake after Signal: %v", err)
	}
	waitForReplay(t, session, "signal-received")
	if err := session.WriteInput(ctx, []byte("printf 'before-checkpoint\\r\\n'\r")); err != nil {
		t.Fatalf("WriteInput before checkpoint: %v", err)
	}
	waitForReplay(t, session, "before-checkpoint")
	outputCursor, err := session.OutputCursor(ctx)
	if err != nil || outputCursor == (ghostline.Cursor{}) {
		t.Fatalf("OutputCursor = (%q, %v)", outputCursor, err)
	}
	checkpoint, err := session.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !bytes.Contains(checkpoint.Replay, []byte("before-checkpoint")) || checkpoint.Cursor == (ghostline.Cursor{}) {
		t.Fatalf("Checkpoint = replay %d bytes, cursor %q", len(checkpoint.Replay), checkpoint.Cursor)
	}
	atomicState, err := session.AtomicState(ctx)
	if err != nil {
		t.Fatalf("AtomicState: %v", err)
	}
	if atomicState.Format != ghostline.AtomicStateFormat || len(atomicState.Payload) == 0 || atomicState.Cursor == (ghostline.Cursor{}) {
		t.Fatalf("AtomicState = format %q, payload %d bytes, cursor %q", atomicState.Format, len(atomicState.Payload), atomicState.Cursor)
	}
	reader, err := session.Output(ctx, checkpoint.Cursor)
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if err := session.WriteInput(ctx, []byte("printf 'after-checkpoint\\r\\n'\r")); err != nil {
		t.Fatalf("WriteInput after checkpoint: %v", err)
	}
	waitForReplay(t, session, "after-checkpoint")
	if err := session.Terminate(ctx); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	raw, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read output = (%v, %v)", readErr, closeErr)
	}
	if !bytes.Contains(raw, []byte("after-checkpoint")) {
		t.Fatalf("post-checkpoint output = %q", raw)
	}
	var exit *ghostline.ExitError
	if err := session.Wait(ctx); err != nil && !errors.As(err, &exit) {
		t.Fatalf("Wait: %v", err)
	}
	status, err = session.Status(ctx)
	if err != nil || status.Alive {
		t.Fatalf("stopped Status = (%+v, %v)", status, err)
	}
	if err := session.Signal(ctx, syscall.SIGCONT); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Signal after stop = %v, want os.ErrProcessDone", err)
	}
	if err := session.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, session.Name()); !errors.Is(err, ghostline.ErrSessionNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrSessionNotFound", err)
	}
}
