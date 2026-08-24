//go:build darwin || linux

package ghostline

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpoolWatcherFileEventPrecedesHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.out")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	delivered := make(chan string, 1)
	watcher, err := NewSpoolWatcher(path, 0, func(data []byte) {
		delivered <- string(append([]byte(nil), data...))
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	watcher.Start()
	defer watcher.Close()

	appendToFile(t, path, "ready")
	select {
	case got := <-delivered:
		if got != "ready" {
			t.Fatalf("delivered = %q, want ready", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("file event did not deliver output before the safety heartbeat")
	}
}

func TestSpoolWatcherDoesNotStatIdleFileAtInteractiveCadence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idle.out")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	watcher, err := NewSpoolWatcher(path, 0, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var checks atomic.Int64
	stat := watcher.stat
	watcher.stat = func() (os.FileInfo, error) {
		checks.Add(1)
		return stat()
	}
	watcher.Start()
	defer watcher.Close()

	deadline := time.Now().Add(250 * time.Millisecond)
	for checks.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if checks.Load() == 0 {
		t.Fatal("watcher did not perform its initial check")
	}
	baseline := checks.Load()
	time.Sleep(150 * time.Millisecond)
	if got := checks.Load(); got != baseline {
		t.Fatalf("idle stat count advanced from %d to %d before heartbeat", baseline, got)
	}
}

func TestSpoolWatcherCoalescesInteractiveWriteBurst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "burst.out")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var delivered []byte
	var callbacks int
	watcher, err := NewSpoolWatcher(path, 0, func(data []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, data...)
		callbacks++
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	watcher.eventDelay = 20 * time.Millisecond
	watcher.Start()
	defer watcher.Close()

	for range 10 {
		appendToFile(t, path, "x")
		time.Sleep(2 * time.Millisecond)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		mu.Lock()
		count := len(delivered)
		mu.Unlock()
		if count == 10 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if string(delivered) != "xxxxxxxxxx" {
		t.Fatalf("delivered = %q, want ten bytes", delivered)
	}
	if callbacks > 2 {
		t.Fatalf("callbacks = %d, want at most two coalesced deliveries", callbacks)
	}
}
