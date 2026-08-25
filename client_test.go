package ghostline_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	return startTestServerWithOptions(t, ghostline.Options{OutputDir: t.TempDir()})
}

func startTestServerWithOptions(t *testing.T, options ghostline.Options) (string, *ghostline.Client) {
	t.Helper()
	socketDir, err := os.MkdirTemp("", "ghostline-")
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	socket := filepath.Join(socketDir, "ghostline.sock")
	server, err := ghostline.NewServer(options)
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

func waitRemoteReplay(t *testing.T, session *ghostline.Session, needle string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := session.Replay(context.Background())
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
		Name:    "serve",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	status, err := session.Status(ctx)
	if err != nil || !status.Alive {
		t.Fatal("session should be alive")
	}
	if err := session.WriteInput(ctx, []byte("echo hello-serve\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitRemoteReplay(t, session, "hello-serve")
	sessions, err := client.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Name() != "serve" {
		t.Fatalf("List = %v", sessions)
	}
	if err := session.Terminate(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	status, err = session.Status(ctx)
	if err != nil || status.Alive {
		t.Fatal("session alive after Close")
	}
	if err := session.Delete(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestClientStartSendsSizeAndEnvironment(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "configured",
		Process: ghostline.ProcessSpec{
			Path: "sh", Directory: t.TempDir(),
			Environment: []string{"GHOSTLINE_TEST=remote"},
		},
		Size: ghostline.Size{Columns: 90, Rows: 28},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("stty size; echo env=$GHOSTLINE_TEST\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitRemoteReplay(t, session, "28 90")
	waitRemoteReplay(t, session, "env=remote")
}

func TestClientSessionByNameAndSessions(t *testing.T) {
	_, client := startTestServer(t)
	ctx := context.Background()
	if _, err := client.Start(ctx, ghostline.SessionOptions{
		Name:    "ghost_test_named",
		Process: ghostline.ProcessSpec{Path: "sh"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	session, err := client.Get(ctx, "ghost_test_named")
	if err != nil {
		t.Fatal("Session should find the started session")
	}
	if session.Name() != "ghost_test_named" {
		t.Fatalf("session name = %q", session.Name())
	}
	if session.CreatedAt().IsZero() {
		t.Fatal("Session handle should resolve CreatedAt lazily")
	}
	if err := session.WriteInput(ctx, []byte("echo named-ok\r")); err != nil {
		t.Fatalf("Input on named session: %v", err)
	}

	if _, err := client.Get(ctx, "ghost_test_missing"); !errors.Is(err, ghostline.ErrSessionNotFound) {
		t.Fatal("Session should not find a missing session")
	}

	sessions, err := client.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, existing := range sessions {
		if existing.Name() == "ghost_test_named" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Sessions missing the started session: %d handles", len(sessions))
	}
}

func TestClientVersionReportsProtocol(t *testing.T) {
	_, client := startTestServer(t)
	version, err := client.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version != ghostline.ProtocolVersion {
		t.Fatalf("Version = %q, want %q", version, ghostline.ProtocolVersion)
	}
}

func TestClientVersionInfoReportsProtocolAndTag(t *testing.T) {
	_, client := startTestServer(t)
	info, err := client.VersionInfo(context.Background())
	if err != nil {
		t.Fatalf("VersionInfo: %v", err)
	}
	if info.ProtocolVersion != ghostline.ProtocolVersion {
		t.Fatalf("VersionInfo protocol = %q, want %q", info.ProtocolVersion, ghostline.ProtocolVersion)
	}
	if info.TagVersion != ghostline.TagVersion() {
		t.Fatalf("VersionInfo tag = %q, want %q", info.TagVersion, ghostline.TagVersion())
	}
	capabilities := make(map[string]bool, len(info.Capabilities))
	for _, capability := range info.Capabilities {
		capabilities[capability] = true
	}
	if !capabilities[ghostline.CapabilityRawPayload] || !capabilities[ghostline.CapabilityStreams] {
		t.Fatalf("VersionInfo capabilities = %v", info.Capabilities)
	}
	if info.Limits.MaxHeaderBytes <= 0 || info.Limits.MaxPayloadBytes <= 0 || info.Limits.MaxChunkBytes <= 0 ||
		info.Limits.MaxChunkBytes > info.Limits.MaxPayloadBytes {
		t.Fatalf("VersionInfo limits = %+v", info.Limits)
	}
}

func TestClientStreamsLargeReplayAndCheckpoint(t *testing.T) {
	_, client := startTestServerWithOptions(t, ghostline.Options{
		OutputDir:            t.TempDir(),
		VTScrollbackMaxBytes: 32 << 20,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	line := strings.Repeat("x", 256)
	session, err := client.Start(ctx, ghostline.SessionOptions{
		Name:    "large-replay",
		Process: ghostline.Shell(fmt.Sprintf("yes %s | head -c 1600000", line)),
		Size:    ghostline.Size{Columns: 300, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	output, err := session.Output(ctx, ghostline.Cursor{})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	written, copyErr := io.Copy(io.Discard, output)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("drain output = (%v, %v)", copyErr, closeErr)
	}
	if written < 1<<20 {
		t.Fatalf("raw output = %d bytes, want more than one RPC frame", written)
	}
	if err := session.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	replay, err := session.Replay(ctx)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(replay) <= 1<<20 {
		t.Fatalf("Replay = %d bytes, want chunked payload larger than one RPC frame", len(replay))
	}
	checkpoint, err := session.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !bytes.Equal(checkpoint.Replay, replay) {
		t.Fatalf("Checkpoint replay differs from stable Replay: %d != %d bytes", len(checkpoint.Replay), len(replay))
	}
	if checkpoint.Cursor == (ghostline.Cursor{}) {
		t.Fatal("Checkpoint returned a zero cursor after output")
	}
}

func TestClientWaitReturnsExitError(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "wait", Process: ghostline.Shell("exit 7"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var exitErr *ghostline.ExitError
	if err := session.Wait(context.Background()); !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Wait = %v, want exit code 7", err)
	}
	status, err := session.Status(context.Background())
	if err != nil || status.Exit == nil || status.Exit.Code != 7 {
		t.Fatalf("Status = (%+v, %v), want exit code 7", status, err)
	}
}

func TestClientErrorsPreserveIdentity(t *testing.T) {
	_, client := startTestServer(t)
	ctx := context.Background()
	options := ghostline.SessionOptions{Name: "dup", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()}}
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
		Name: "checkpoint", Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("printf 'checkpoint-data\\r\\n'\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitRemoteReplay(t, session, "checkpoint-data")
	checkpoint, err := session.Checkpoint(context.Background())
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !bytes.Contains(checkpoint.Replay, []byte("checkpoint-data")) {
		t.Fatalf("replay missing output: %q", checkpoint.Replay)
	}
	if checkpoint.Cursor.String() == "" {
		t.Fatal("checkpoint returned a zero cursor")
	}
	readerCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reader, err := session.Output(readerCtx, ghostline.Cursor{})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	defer reader.Close()
	buffer := make([]byte, 1024)
	n, err := reader.Read(buffer)
	if err != nil {
		t.Fatalf("Read output: %v", err)
	}
	if !bytes.Contains(buffer[:n], []byte("checkpoint-data")) {
		t.Fatalf("output missing checkpoint data: %q", buffer[:n])
	}
}

func TestClientWaitCancellationKeepsSession(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "cancel", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := session.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v", err)
	}
	status, statusErr := session.Status(context.Background())
	if statusErr != nil || !status.Alive {
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
	buffer := make([]byte, 4096)
	count, err := connection.Read(buffer)
	if err != nil {
		t.Fatalf("read frame-limit response: %v", err)
	}
	if !strings.Contains(string(buffer[:count]), "frame_too_large") {
		t.Fatalf("response = %q, want frame_too_large", buffer[:count])
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

func managedClientOptions(t *testing.T, dir string) ghostline.ManagedClientOptions {
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
	return ghostline.ManagedClientOptions{
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

func TestConnectManagedSpawnsServer(t *testing.T) {
	dir := t.TempDir()
	client, err := ghostline.ConnectManaged(context.Background(), managedClientOptions(t, dir))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "connect", Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	status, err := session.Status(context.Background())
	if err != nil || !status.Alive {
		t.Fatal("session not alive after Connect")
	}
}

func TestConnectManagedReusesRunningServer(t *testing.T) {
	socket, _ := startTestServer(t)
	client, err := ghostline.ConnectManaged(context.Background(), ghostline.ManagedClientOptions{Socket: socket})
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
	client, err := ghostline.ConnectManaged(context.Background(), managedClientOptions(t, dir))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	_ = client.Close()
	if err := client.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "ensure", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	}); err != nil {
		t.Fatalf("Start after Ensure: %v", err)
	}
}

func TestLimitedRecoveryRetriesIdempotentCalls(t *testing.T) {
	dir := t.TempDir()
	client, err := ghostline.ConnectManaged(context.Background(), managedClientOptions(t, dir))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "recover", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
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
	options := managedClientOptions(t, dir)
	client, err := ghostline.ConnectManaged(context.Background(), options)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "no-retry", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = client.Close()
	if err := session.WriteInput(context.Background(), []byte("x")); err == nil {
		t.Fatal("Input succeeded after server shutdown")
	}
	if err := ghostline.NewClient(options.Socket).Check(context.Background()); err == nil {
		t.Fatal("non-idempotent call spawned the server")
	}
}

func TestConnectManagedFailsFastWhenSpawnExits(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "ghostline-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "ghostline.sock")
	started := time.Now()
	client, err := ghostline.ConnectManaged(context.Background(), ghostline.ManagedClientOptions{
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

func TestConnectManagedRejectsInvalidEnv(t *testing.T) {
	_, err := ghostline.ConnectManaged(context.Background(), ghostline.ManagedClientOptions{
		Socket: filepath.Join(t.TempDir(), "ghostline.sock"),
		Spawn:  []string{"sh", "-c", "exit 0"},
		Env:    []string{"NO_EQUALS"},
	})
	if err == nil {
		t.Fatal("Connect succeeded with an invalid environment entry")
	}
}

func TestConnectManagedConcurrentSpawnsOneServer(t *testing.T) {
	dir := t.TempDir()
	options := managedClientOptions(t, dir)
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
			client, err := ghostline.ConnectManaged(context.Background(), options)
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
	if err := ghostline.NewClient(options.Socket).Check(context.Background()); err == nil {
		t.Fatal("server still running after every client closed")
	}
}

func TestManagedClientConcurrentEnsurePIDAndClose(t *testing.T) {
	dir := t.TempDir()
	client, err := ghostline.ConnectManaged(context.Background(), managedClientOptions(t, dir))
	if err != nil {
		t.Fatalf("ConnectManaged: %v", err)
	}
	const callers = 16
	var group sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			errs <- client.Ensure(context.Background())
			_ = client.PID()
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Ensure: %v", err)
		}
	}

	closeErrs := make(chan error, callers)
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			closeErrs <- client.Close()
		}()
	}
	group.Wait()
	close(closeErrs)
	for err := range closeErrs {
		if err != nil {
			t.Fatalf("concurrent Close: %v", err)
		}
	}
	if pid := client.PID(); pid != 0 {
		t.Fatalf("PID after Close = %d, want 0", pid)
	}
}
