package ghostline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func startTestServer(t *testing.T) (string, *Client) {
	t.Helper()
	directory := t.TempDir()
	// Unix socket paths are capped at 104 bytes; a long test-name TempDir can
	// exceed it, so keep the socket in a short sibling directory.
	socketDir, err := os.MkdirTemp("", "ghostline-")
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	socket := filepath.Join(socketDir, "ghostline.sock")
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	server, err := NewServer(Options{OutputDir: directory})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	done := make(chan struct{})
	serveErr := make(chan error, 1)
	go func() {
		defer close(done)
		serveErr <- server.Serve(socket)
	}()
	client := NewClient(socket)
	if err := client.WaitReady(context.Background(), 3*time.Second); err != nil {
		t.Fatalf("server did not become ready: %v; serve error: %v", err, <-serveErr)
	}
	t.Cleanup(func() {
		_ = client.Kill(context.Background(), "ghost_test_serve")
		_ = server.Close()
		<-done
	})
	return socket, client
}

func TestServerClientLifecycle(t *testing.T) {
	_, client := startTestServer(t)
	ctx := context.Background()
	if err := client.Create(ctx, "ghost_test_serve", t.TempDir(), "sh"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !client.Exists(ctx, "ghost_test_serve") {
		t.Fatal("session should exist after Create")
	}
	if err := client.Input(ctx, "ghost_test_serve", []byte("echo hello-serve\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := client.Capture(ctx, "ghost_test_serve")
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		if bytes.Contains(snapshot, []byte("hello-serve")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot missing output: %q", snapshot)
		}
		time.Sleep(50 * time.Millisecond)
	}
	sessions, err := client.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !sessions["ghost_test_serve"] {
		t.Fatalf("List missing session: %v", sessions)
	}
	if err := client.Kill(ctx, "ghost_test_serve"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if client.Exists(ctx, "ghost_test_serve") {
		t.Fatal("session should not exist after Kill")
	}
}

// TestServerClientReconnect simulates an embedding daemon restart: a fresh
// Client connects to the same server and the session is still alive.
func TestServerClientReconnect(t *testing.T) {
	socket, client := startTestServer(t)
	ctx := context.Background()
	if err := client.Create(ctx, "ghost_test_reconnect", t.TempDir(), "sh"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := client.Input(ctx, "ghost_test_reconnect", []byte("echo reconnected-ok\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	restarted := NewClient(socket)
	if !restarted.Exists(ctx, "ghost_test_reconnect") {
		t.Fatal("session should survive a client reconnect")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := restarted.Capture(ctx, "ghost_test_reconnect")
		if err != nil {
			t.Fatalf("Capture after reconnect: %v", err)
		}
		if bytes.Contains(snapshot, []byte("reconnected-ok")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot after reconnect missing output: %q", snapshot)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestServerClientSpoolOperations(t *testing.T) {
	_, client := startTestServer(t)
	ctx := context.Background()
	if err := client.Create(ctx, "ghost_test_spool", t.TempDir(), "sh"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := client.Input(ctx, "ghost_test_spool", []byte("echo spool-data\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		size, err := client.SpoolSize(ctx, "ghost_test_spool")
		if err == nil && size > 0 {
			recovered, err := client.Recover(ctx, "ghost_test_spool", 0, size)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if bytes.Contains(recovered, []byte("spool-data")) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("spool did not receive output")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if path := client.SpoolPath("ghost_test_spool"); path == "" {
		t.Fatal("SpoolPath returned empty")
	}
	if err := client.TruncateSpool(ctx, "ghost_test_spool"); err != nil {
		t.Fatalf("TruncateSpool: %v", err)
	}
	if size, _ := client.SpoolSize(ctx, "ghost_test_spool"); size != 0 {
		t.Fatalf("spool size after truncate = %d", size)
	}
}
