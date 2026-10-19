package ghostline_test

import (
	"bytes"
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/abcdlsj/ghostline"
)

// winchReporter prints its size once at start and again on every SIGWINCH,
// which is how a TUI answers a resize.
var winchReporter = ghostline.Shell(`trap 'stty size' WINCH; stty size; while :; do sleep 0.01; done`)

// assertResizeAttribution reads one session's output with ReadSized and
// requires that every `stty size` line arrives in a chunk attributed to the
// size it reports, with the resize reported before the child's answer.
func assertResizeAttribution(t *testing.T, session *ghostline.Session, initial ghostline.Size) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reader, err := session.Output(ctx, ghostline.Cursor{})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	defer reader.Close()

	resized := ghostline.Size{Columns: 101, Rows: 31}
	expect := func(want ghostline.Size) {
		t.Helper()
		needle := []byte(strconv.Itoa(want.Rows) + " " + strconv.Itoa(want.Columns))
		var current ghostline.Size
		var attributed []byte
		buffer := make([]byte, 4096)
		for {
			n, size, err := reader.ReadSized(buffer)
			if err != nil {
				t.Fatalf("ReadSized while waiting for %q: %v", needle, err)
			}
			if n == 0 {
				current = size
				attributed = attributed[:0]
				continue
			}
			if size != current {
				t.Fatalf("chunk attributed to %v after a report of %v", size, current)
			}
			attributed = append(attributed, buffer[:n]...)
			if bytes.Contains(attributed, needle) {
				if current != want {
					t.Fatalf("%q arrived under %v", needle, current)
				}
				return
			}
		}
	}
	expect(initial)
	if err := session.Resize(ctx, resized); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	expect(resized)

	state, err := session.AtomicState(ctx)
	if err != nil {
		t.Fatalf("AtomicState: %v", err)
	}
	if state.Size != resized {
		t.Fatalf("AtomicState.Size = %v, want %v", state.Size, resized)
	}
}

func TestLocalReadSizedAttributesOutputToTheResizeItFollows(t *testing.T) {
	initial := ghostline.Size{Columns: 80, Rows: 24}
	hub := newHub(t, ghostline.Options{DefaultSize: initial})
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name: "sized-local", Process: winchReporter,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertResizeAttribution(t, session, initial)
}

func TestRemoteReadSizedAttributesOutputToTheResizeItFollows(t *testing.T) {
	initial := ghostline.Size{Columns: 80, Rows: 24}
	_, client := startTestServerWithOptions(t, ghostline.Options{OutputDir: t.TempDir(), DefaultSize: initial})
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "sized-remote", Process: winchReporter,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertResizeAttribution(t, session, initial)
}
