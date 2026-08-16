package ghostline_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	err = client.Check(context.Background())
	for err != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		err = client.Check(context.Background())
	}
	if err != nil {
		t.Fatal("server did not become ready")
	}
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		<-done
		_ = os.RemoveAll(socketDir)
	})
	return socket, client
}

// TestServerHelperProcess is the subprocess spawned by Connect tests.
func TestServerHelperProcess(t *testing.T) {
	if os.Getenv("GHOSTLINE_HELPER") != "1" {
		return
	}
	server, err := ghostline.NewServer(ghostline.Options{
		OutputDir: os.Getenv("GHOSTLINE_HELPER_DIR"),
	})
	if err != nil {
		os.Exit(1)
	}
	if err := server.Serve(context.Background(), os.Getenv("GHOSTLINE_HELPER_SOCKET")); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func waitRemoteSpool(t *testing.T, session ghostline.Session, needle string) {
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
	defer func() { _ = connection.Close() }()
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
	defer func() { _ = connection.Close() }()
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

func connectOptions(t *testing.T, dir string) ghostline.ConnectOptions {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("", "ghostline-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "ghostline.sock")
	return ghostline.ConnectOptions{
		Socket:       socket,
		Spawn:        []string{executable, "-test.run=TestServerHelperProcess"},
		ReadyTimeout: 5 * time.Second,
		Env: []string{
			"GHOSTLINE_HELPER=1",
			"GHOSTLINE_HELPER_DIR=" + dir,
			"GHOSTLINE_HELPER_SOCKET=" + socket,
		},
	}
}

func TestConnectSpawnsServer(t *testing.T) {
	dir := t.TempDir()
	client, err := ghostline.Connect(context.Background(), connectOptions(t, dir))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "connect", Directory: t.TempDir(), Command: "sh",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !session.Alive() {
		t.Fatal("session not alive after Connect")
	}
}

func TestConnectReusesRunningServer(t *testing.T) {
	socket, _ := startTestServer(t)
	client, err := ghostline.Connect(context.Background(), ghostline.ConnectOptions{Socket: socket})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := ghostline.NewClient(socket).Check(context.Background()); err != nil {
		t.Fatal("Close stopped a server it did not spawn")
	}
}

func TestEnsureRespawnsServer(t *testing.T) {
	dir := t.TempDir()
	client, err := ghostline.Connect(context.Background(), connectOptions(t, dir))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	_ = client.Close()
	if err := client.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "ensure", Directory: t.TempDir(), Command: "sleep 30",
	}); err != nil {
		t.Fatalf("Start after Ensure: %v", err)
	}
}

func TestLimitedRecoveryRetriesIdempotentCalls(t *testing.T) {
	dir := t.TempDir()
	client, err := ghostline.Connect(context.Background(), connectOptions(t, dir))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "recover", Directory: t.TempDir(), Command: "sleep 30",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = client.Close()
	names, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List after recovery: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("recovered server should be empty, got %v", names)
	}
}

func TestLimitedRecoveryDoesNotRetryInput(t *testing.T) {
	dir := t.TempDir()
	options := connectOptions(t, dir)
	client, err := ghostline.Connect(context.Background(), options)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "no-retry", Directory: t.TempDir(), Command: "sleep 30",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = client.Close()
	if err := session.Input(context.Background(), []byte("x")); err == nil {
		t.Fatal("Input succeeded after server shutdown")
	}
	if ghostline.Ping(options.Socket) {
		t.Fatal("non-idempotent call spawned the server")
	}
}

func TestConnectFailsFastWhenSpawnExits(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "ghostline-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "ghostline.sock")
	started := time.Now()
	client, err := ghostline.Connect(context.Background(), ghostline.ConnectOptions{
		Socket:       socket,
		Spawn:        []string{"sh", "-c", "echo boom >&2; exit 1"},
		ReadyTimeout: 30 * time.Second,
	})
	if err == nil {
		_ = client.Close()
		t.Fatal("Connect succeeded with a failing spawn")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should include spawn output, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("spawn failure took %v, want a fast failure", elapsed)
	}
}

func TestConnectRejectsInvalidEnv(t *testing.T) {
	_, err := ghostline.Connect(context.Background(), ghostline.ConnectOptions{
		Socket: filepath.Join(t.TempDir(), "ghostline.sock"),
		Spawn:  []string{"sh", "-c", "exit 0"},
		Env:    []string{"NO_EQUALS"},
	})
	if err == nil {
		t.Fatal("Connect succeeded with an invalid environment entry")
	}
}

func TestConnectConcurrentSpawnsOneServer(t *testing.T) {
	dir := t.TempDir()
	options := connectOptions(t, dir)
	const callers = 8
	type result struct {
		client *ghostline.Client
		err    error
	}
	results := make([]result, callers)
	var group sync.WaitGroup
	for index := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			client, err := ghostline.Connect(context.Background(), options)
			results[index] = result{client: client, err: err}
		}()
	}
	group.Wait()
	for index, result := range results {
		if result.err != nil {
			t.Fatalf("Connect %d: %v", index, result.err)
		}
		if err := result.client.Check(context.Background()); err != nil {
			t.Fatalf("client %d Check: %v", index, err)
		}
	}
	for _, result := range results {
		_ = result.client.Close()
	}
	if ghostline.Ping(options.Socket) {
		t.Fatal("server still running after every client closed")
	}
}
