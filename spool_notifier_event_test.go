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

func TestSpoolWatcherDoesNotPollIdleFileBeforeHeartbeat(t *testing.T) {
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

func TestSpoolWatcherDrainsWriteBurstInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "burst.out")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var delivered []byte
	watcher, err := NewSpoolWatcher(path, 0, func(data []byte) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, data...)
	}, nil, nil)
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
	initialDeadline := time.Now().Add(250 * time.Millisecond)
	for checks.Load() == 0 && time.Now().Before(initialDeadline) {
		time.Sleep(time.Millisecond)
	}
	if checks.Load() == 0 {
		t.Fatal("watcher did not complete its initial drain")
	}

	want := "0123456789"
	for _, chunk := range want {
		appendToFile(t, path, string(chunk))
		time.Sleep(2 * time.Millisecond)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		mu.Lock()
		count := len(delivered)
		mu.Unlock()
		if count == len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if string(delivered) != want {
		t.Fatalf("delivered = %q, want %q", delivered, want)
	}
	if got := watcher.Offset(); got != int64(len(want)) {
		t.Fatalf("offset = %d, want %d", got, len(want))
	}
}

func TestSpoolWatcherHeartbeatRecoversWithoutFileEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat.out")
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
	oldNotifier := watcher.notifier
	oldNotifier.Close()
	watcher.notifier = &silentSpoolNotifier{events: make(chan struct{})}
	watcher.heartbeat = 20 * time.Millisecond
	initialCheck := make(chan struct{})
	var initialOnce sync.Once
	stat := watcher.stat
	watcher.stat = func() (os.FileInfo, error) {
		initialOnce.Do(func() { close(initialCheck) })
		return stat()
	}
	watcher.Start()
	defer watcher.Close()

	select {
	case <-initialCheck:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watcher did not perform its initial check")
	}
	appendToFile(t, path, "heartbeat")
	select {
	case got := <-delivered:
		if got != "heartbeat" {
			t.Fatalf("delivered = %q, want heartbeat", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("heartbeat did not recover output without a file event")
	}
}

type silentSpoolNotifier struct {
	events <-chan struct{}
}

func (n *silentSpoolNotifier) Events() <-chan struct{} { return n.events }

func (*silentSpoolNotifier) Close() {}
