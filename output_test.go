package ghostline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCursorTextAndJSONRoundTrip(t *testing.T) {
	want := Cursor{generation: 42, offset: 99}
	text, err := want.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCursor(string(text))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ParseCursor() = %v, want %v", got, want)
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Cursor
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != want {
		t.Fatalf("JSON cursor = %v, want %v", decoded, want)
	}
}

func TestParseCursorRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"1:2", "v0:1:2", "v1:0:0", "v1:x:0", "v1:1:-1", "v1:1:2:3"} {
		if _, err := ParseCursor(value); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("ParseCursor(%q) error = %v, want ErrInvalidCursor", value, err)
		}
	}
}

func TestOutputReaderDrainsRapidRotationAndRegrowth(t *testing.T) {
	log, err := createOutputLog(t.TempDir(), "rotation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.discard() })
	reader, err := log.reader(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	if err := log.append([]byte("old")); err != nil {
		t.Fatal(err)
	}
	boundary, err := log.rotate()
	if err != nil {
		t.Fatal(err)
	}
	if err := log.append([]byte("new-and-longer")); err != nil {
		t.Fatal(err)
	}
	log.close(nil)

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "oldnew-and-longer"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if boundary != (Cursor{generation: 2}) {
		t.Fatalf("rotation cursor = %v", boundary)
	}
	if got := reader.Cursor(); got != (Cursor{generation: 2, offset: 14}) {
		t.Fatalf("reader cursor = %v", got)
	}
}

func TestOutputReaderIsBoundedByCallerBuffer(t *testing.T) {
	log, err := createOutputLog(t.TempDir(), "bounded")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.discard() })
	if err := log.append([]byte(strings.Repeat("x", 1024))); err != nil {
		t.Fatal(err)
	}
	reader, err := log.reader(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	buffer := make([]byte, 7)
	n, err := reader.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(buffer) {
		t.Fatalf("Read() = %d bytes, want %d", n, len(buffer))
	}
}

func TestOutputReaderCancellationAndCloseUnblockRead(t *testing.T) {
	for _, test := range []struct {
		name string
		stop func(context.CancelFunc, *OutputReader)
		want error
	}{
		{name: "context", stop: func(cancel context.CancelFunc, _ *OutputReader) { cancel() }, want: context.Canceled},
		{name: "close", stop: func(_ context.CancelFunc, reader *OutputReader) { _ = reader.Close() }, want: io.ErrClosedPipe},
	} {
		t.Run(test.name, func(t *testing.T) {
			log, err := createOutputLog(t.TempDir(), "blocked")
			if err != nil {
				t.Fatal(err)
			}
			defer log.discard()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cursor, err := log.cursor()
			if err != nil {
				t.Fatal(err)
			}
			reader, err := log.reader(ctx, cursor)
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				_, readErr := reader.Read(make([]byte, 1))
				result <- readErr
			}()
			test.stop(cancel, reader)
			select {
			case readErr := <-result:
				if test.name == "close" && errors.Is(readErr, io.ErrClosedPipe) {
					return
				}
				if !errors.Is(readErr, test.want) {
					t.Fatalf("Read() error = %v, want %v", readErr, test.want)
				}
			case <-time.After(time.Second):
				t.Fatal("Read did not unblock")
			}
		})
	}
}

func TestOutputPruneExpiresNewReadersButOpenReaderDrains(t *testing.T) {
	log, err := createOutputLog(t.TempDir(), "prune")
	if err != nil {
		t.Fatal(err)
	}
	defer log.discard()
	if err := log.append([]byte("first")); err != nil {
		t.Fatal(err)
	}
	reader, err := log.reader(context.Background(), Cursor{generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := log.rotate(); err != nil {
		t.Fatal(err)
	}
	if err := log.append([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := log.prune(Cursor{generation: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.reader(context.Background(), Cursor{generation: 1}); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("reader from pruned cursor error = %v, want ErrCursorExpired", err)
	}
	log.close(nil)
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "firstsecond"; got != want {
		t.Fatalf("open reader output = %q, want %q", got, want)
	}
}

func TestOutputPruneReclaimsSegmentMetadata(t *testing.T) {
	log, err := createOutputLog(t.TempDir(), "prune-index")
	if err != nil {
		t.Fatal(err)
	}
	defer log.discard()

	for range 128 {
		boundary, err := log.rotate()
		if err != nil {
			t.Fatal(err)
		}
		if err := log.prune(boundary); err != nil {
			t.Fatal(err)
		}
	}

	log.mu.Lock()
	generation := log.generation
	segmentCount := len(log.segments)
	log.mu.Unlock()
	if segmentCount != 1 {
		t.Fatalf("retained segment metadata = %d, want 1", segmentCount)
	}
	if _, err := log.reader(context.Background(), Cursor{generation: generation - 1}); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("reader from pruned cursor error = %v, want ErrCursorExpired", err)
	}
}

func FuzzParseCursorRoundTrip(f *testing.F) {
	for _, seed := range []string{"", "v1:1:0", "v1:42:99", "v0:1:0", "not-a-cursor"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		cursor, err := ParseCursor(value)
		if err != nil {
			return
		}
		roundTrip, err := ParseCursor(cursor.String())
		if err != nil || roundTrip != cursor {
			t.Fatalf("round trip %q = (%v, %v), want %v", value, roundTrip, err, cursor)
		}
	})
}
