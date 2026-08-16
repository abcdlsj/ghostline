package ghostline

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSpoolWatcherStreamsFromOffsetAndDetectsRotation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "session.out")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}

	bytesSeen := make(chan []byte, 8)
	rotated := make(chan struct{}, 1)
	watcher, err := NewSpoolWatcher(
		path,
		0,
		func(data []byte) { bytesSeen <- append([]byte(nil), data...) },
		func() { rotated <- struct{}{} },
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	watcher.Start()
	defer watcher.Close()

	collect := func(want string) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		var received []byte
		for {
			select {
			case chunk := <-bytesSeen:
				received = append(received, chunk...)
				if string(received) == want {
					return
				}
			case <-deadline:
				t.Fatalf("watcher received %q, want %q", received, want)
			}
		}
	}

	collect("abc")
	if watcher.Offset() != 3 {
		t.Fatalf("offset = %d, want 3", watcher.Offset())
	}

	appendToFile(t, path, "def")
	collect("def")

	// In-place compaction: tmux's pipe keeps its O_APPEND descriptor, so the
	// watcher must re-base to zero and report the rotation.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rotated:
	case <-time.After(2 * time.Second):
		t.Fatal("rotation was not detected")
	}
	if watcher.Offset() != 0 {
		t.Fatalf("offset after rotation = %d, want 0", watcher.Offset())
	}
	appendToFile(t, path, "g")
	collect("g")
}

func TestSpoolWatcherRejectsOffsetBeyondSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.out")
	if err := os.WriteFile(path, []byte("ab"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSpoolWatcher(path, 3, nil, nil, nil); err == nil {
		t.Fatal("offset beyond file size was accepted")
	}
}

func TestSpoolWatcherReusesBufferAndAvoidsIdleReadBuffer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.out")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	watcher, err := NewSpoolWatcher(path, 0, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	watcher.drain()
	if watcher.buffer != nil {
		t.Fatal("idle drain allocated a read buffer")
	}
	appendToFile(t, path, "abc")
	watcher.drain()
	if watcher.buffer == nil {
		t.Fatal("non-empty drain did not allocate a read buffer")
	}
	buffer := &watcher.buffer[0]
	watcher.drain()
	if &watcher.buffer[0] != buffer {
		t.Fatal("watcher replaced its read buffer")
	}
}

func TestSpoolWatcherSkipToRequiresPauseAndValidOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.out")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	watcher, err := NewSpoolWatcher(path, 0, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	if err := watcher.SkipTo(1); err == nil {
		t.Fatal("SkipTo succeeded while watcher was running")
	}
	watcher.Pause()
	defer watcher.Resume()
	if err := watcher.SkipTo(-1); err == nil {
		t.Fatal("SkipTo accepted a negative offset")
	}
	if err := watcher.SkipTo(4); err == nil {
		t.Fatal("SkipTo accepted an offset beyond EOF")
	}
	if err := watcher.SkipTo(2); err != nil {
		t.Fatalf("SkipTo valid offset: %v", err)
	}
	if watcher.Offset() != 2 {
		t.Fatalf("offset = %d, want 2", watcher.Offset())
	}
}

func TestSpoolWatcherOffsetIsSafeDuringDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.out")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	watcher, err := NewSpoolWatcher(path, 0, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	stop := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = watcher.Offset()
			}
		}
	}()
	for range 20 {
		appendToFile(t, path, "x")
		watcher.drain()
	}
	close(stop)
	wait.Wait()
	if watcher.Offset() != 20 {
		t.Fatalf("offset = %d, want 20", watcher.Offset())
	}
}

func BenchmarkSpoolWatcherIdleDrain(b *testing.B) {
	path := filepath.Join(b.TempDir(), "session.out")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		b.Fatal(err)
	}
	watcher, err := NewSpoolWatcher(path, 0, nil, nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer watcher.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		watcher.drain()
	}
}

func appendToFile(t *testing.T, path, data string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(file)
	if _, err := file.WriteString(data); err != nil {
		t.Fatal(err)
	}
}
