package ghostline_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abcdlsj/ghostline"
)

func startTestServer(t *testing.T) (string, *ghostline.Client) {
	t.Helper()
	socketDir, err := os.MkdirTemp("", "ghostline-")
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	socket := filepath.Join(socketDir, "ghostline.sock")
	server, err := ghostline.NewServer(ghostline.Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(context.Background(), socket)
	}()
	client := ghostline.NewClient(socket)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if client.Check(context.Background()) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		<-done
		_ = os.RemoveAll(socketDir)
	})
	return socket, client
}

func waitRemoteSpool(t *testing.T, session *ghostline.Session, needle string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := session.Snapshot(context.Background())
		if err == nil && bytes.Contains(snapshot, []byte(needle)) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("remote output did not contain %q", needle)
}

func TestClientLifecycle(t *testing.T) {
	_, client := startTestServer(t)
	ctx := context.Background()
	session, err := client.Start(ctx, ghostline.SessionOptions{
		Name: "serve", Directory: t.TempDir(), Command: "sh",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !session.Alive() {
		t.Fatal("session should be alive")
	}
	if err := session.Input(ctx, []byte("echo hello-serve\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitRemoteSpool(t, session, "hello-serve")
	names, err := client.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "serve" {
		t.Fatalf("List = %v", names)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if session.Alive() {
		t.Fatal("session alive after Close")
	}
	if err := session.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestClientStartSendsSizeAndEnvironment(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name:        "configured",
		Directory:   t.TempDir(),
		Command:     "sh",
		Size:        ghostline.Size{Columns: 90, Rows: 28},
		Environment: []string{"GHOSTLINE_TEST=remote"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Input(context.Background(), []byte("stty size; echo env=$GHOSTLINE_TEST\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitRemoteSpool(t, session, "28 90")
	waitRemoteSpool(t, session, "env=remote")
}

func TestClientWaitReturnsExitError(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "wait", Directory: t.TempDir(), Command: "exit 7",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var exitErr *ghostline.ExitError
	if err := session.Wait(context.Background()); !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Wait = %v, want exit code 7", err)
	}
	select {
	case <-session.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not close after exit")
	}
}

func TestClientErrorsPreserveIdentity(t *testing.T) {
	_, client := startTestServer(t)
	ctx := context.Background()
	options := ghostline.SessionOptions{Name: "dup", Directory: t.TempDir(), Command: "sleep 30"}
	if _, err := client.Start(ctx, options); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := client.Start(ctx, options); !errors.Is(err, ghostline.ErrSessionExists) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := client.Start(ctx, ghostline.SessionOptions{Name: "../unsafe"}); !errors.Is(err, ghostline.ErrInvalidSessionName) {
		t.Fatalf("invalid name error = %v", err)
	}
}

func TestClientCheckpointAndRecover(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "checkpoint", Directory: t.TempDir(), Command: "sh",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Input(context.Background(), []byte("printf 'checkpoint-data\\r\\n'\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitRemoteSpool(t, session, "checkpoint-data")
	checkpoint, err := session.Checkpoint(context.Background())
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !bytes.Contains(checkpoint.Replay, []byte("checkpoint-data")) {
		t.Fatalf("replay missing output: %q", checkpoint.Replay)
	}
	size, err := session.SpoolSize(context.Background())
	if err != nil {
		t.Fatalf("SpoolSize: %v", err)
	}
	if checkpoint.Offset != size {
		t.Fatalf("offset = %d, size = %d", checkpoint.Offset, size)
	}
	recovered, err := session.Recover(context.Background(), 0, size)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !bytes.Contains(recovered, []byte("checkpoint-data")) {
		t.Fatalf("recovered missing output: %q", recovered)
	}
}

func TestClientWaitCancellationKeepsSession(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "cancel", Directory: t.TempDir(), Command: "sleep 30",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := session.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v", err)
	}
	if !session.Alive() {
		t.Fatal("canceling Wait terminated the session")
	}
}

func TestServerSocketPermissions(t *testing.T) {
	socket, _ := startTestServer(t)
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}

func TestServerRejectsOversizedRequest(t *testing.T) {
	socket, _ := startTestServer(t)
	connection, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, _ = connection.Write(bytes.Repeat([]byte("a"), 2<<20))
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 1)
	if _, err := connection.Read(buffer); err == nil {
		t.Fatal("oversized request was not rejected")
	}
}

func TestServerRejectsMalformedRequest(t *testing.T) {
	socket, _ := startTestServer(t)
	connection, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("not-json\n")); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 4096)
	count, err := connection.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buffer[:count]), "invalid request") {
		t.Fatalf("response = %q", buffer[:count])
	}
}
