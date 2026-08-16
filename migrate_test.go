package ghostline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func startMigrateServer(t *testing.T, outputDir, tag string) (*Server, string) {
	t.Helper()
	socketDir, err := os.MkdirTemp("", "ghostline-migrate-")
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	socket := filepath.Join(socketDir, tag+".sock")
	server, err := NewServer(Options{OutputDir: outputDir})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(ctx, socket)
	}()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		<-done
		_ = os.RemoveAll(socketDir)
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if Ping(socket) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !Ping(socket) {
		t.Fatalf("server %s not ready", tag)
	}
	return server, socket
}

func waitSessionOutput(t *testing.T, session Session, needle string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := session.Snapshot(context.Background())
		if err == nil && bytes.Contains(snapshot, []byte(needle)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session output missing %q", needle)
}

// TestRollingAdoptKeepsChildRunning migrates a live session from one server
// to another: the PTY master moves over SCM_RIGHTS, the emulator state is
// restored, and the child keeps answering input on the new server.
func TestRollingAdoptKeepsChildRunning(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	oldServer, oldSocket := startMigrateServer(t, outputDir, "old")
	session, err := oldServer.hub.Start(ctx, SessionOptions{Name: "mig", Command: "sh"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Input(ctx, []byte("echo before-migrate\r")); err != nil {
		t.Fatalf("Input before migrate: %v", err)
	}
	waitSessionOutput(t, session, "before-migrate")

	newServer, _ := startMigrateServer(t, outputDir, "new")
	adopted, err := Adopt(ctx, oldSocket+".admin", newServer.hub)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted != 1 {
		t.Fatalf("adopted = %d, want 1", adopted)
	}
	deadline := time.Now().Add(3 * time.Second)
	for Ping(oldSocket) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if Ping(oldSocket) {
		t.Fatal("old server still serving after adoption")
	}

	adoptedSession, ok := newServer.hub.Session("mig")
	if !ok {
		t.Fatal("adopted session missing on new server")
	}
	if !adoptedSession.Alive() {
		t.Fatal("child died during migration")
	}
	if err := adoptedSession.Input(ctx, []byte("echo after-migrate\r")); err != nil {
		t.Fatalf("Input after migrate: %v", err)
	}
	waitSessionOutput(t, adoptedSession, "after-migrate")

	// The adopted session must still render the pre-migration history.
	snapshot, err := adoptedSession.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !bytes.Contains(snapshot, []byte("before-migrate")) {
		t.Fatalf("snapshot lost pre-migration history: %q", snapshot)
	}
}

// TestMigrationAbortKeepsServing verifies that a failed adoption resumes the
// old server's copy loop instead of leaving the session paused forever.
func TestMigrationAbortKeepsServing(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()
	oldServer, oldSocket := startMigrateServer(t, outputDir, "abort")
	session, err := oldServer.hub.Start(ctx, SessionOptions{Name: "keep", Command: "sh"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	state := oldServer.hub.session("keep")
	state.beginMigration()
	select {
	case <-state.migrationStable():
	case <-time.After(2 * time.Second):
		t.Fatal("migration did not reach stable point")
	}
	state.abortMigration()
	if err := session.Input(ctx, []byte("echo still-alive\r")); err != nil {
		t.Fatalf("Input after abort: %v", err)
	}
	waitSessionOutput(t, session, "still-alive")
	if Ping(oldSocket) == false {
		t.Fatal("old server stopped after aborted migration")
	}
}
