package ghostline

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func unixAdminPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	socketDir, err := os.MkdirTemp("/tmp", "ghostline-admin-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "admin.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	accepted := make(chan *net.UnixConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- connection
	}()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatalf("DialUnix: %v", err)
	}
	var server *net.UnixConn
	select {
	case server = <-accepted:
	case err := <-acceptErr:
		_ = client.Close()
		t.Fatalf("AcceptUnix: %v", err)
	case <-time.After(time.Second):
		_ = client.Close()
		t.Fatal("accept admin connection timed out")
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		_ = listener.Close()
	})
	return client, server
}

func TestAdminTransportReadsSplitFDHandshake(t *testing.T) {
	client, server := unixAdminPair(t)
	transport := newAdminTransport(client)
	readFD, writeFD, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	_ = writeFD.Close()

	go func() {
		_ = server.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := server.Write([]byte("{\"id\":1,\"result\":{\"name\":\"x\"}}\n")); err != nil {
			t.Errorf("write adopt response: %v", err)
			return
		}
		// The descriptor can arrive as a separate one-byte NUL message.
		if _, _, err := server.WriteMsgUnix([]byte{0}, unix.UnixRights(int(readFD.Fd())), nil); err != nil {
			t.Errorf("write fd handshake: %v", err)
			return
		}
		_ = readFD.Close()
		if _, err := server.Write([]byte("{\"id\":2,\"result\":{}}\n")); err != nil {
			t.Errorf("write next response: %v", err)
		}
	}()

	var response adminResponse
	if err := transport.read(&response); err != nil {
		t.Fatalf("read adopt response: %v", err)
	}
	var meta sessionMeta
	if err := json.Unmarshal(response.Result, &meta); err != nil {
		t.Fatalf("decode adopt result: %v", err)
	}
	if meta.Name != "x" {
		t.Fatalf("meta.Name = %q, want x", meta.Name)
	}
	fd, err := transport.takeFD()
	if err != nil {
		t.Fatalf("takeFD: %v", err)
	}
	received := os.NewFile(uintptr(fd), "received")
	if received == nil {
		t.Fatal("received fd is nil")
	}
	_ = received.Close()

	var next adminResponse
	if err := transport.read(&next); err != nil {
		t.Fatalf("read next response: %v", err)
	}
	if next.ID != 2 {
		t.Fatalf("response.ID = %d, want 2", next.ID)
	}
}

func TestAdminTransportReadsCoalescedFDHandshake(t *testing.T) {
	client, server := unixAdminPair(t)
	transport := newAdminTransport(client)
	readFD, writeFD, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	_ = writeFD.Close()

	go func() {
		_ = server.SetWriteDeadline(time.Now().Add(time.Second))
		// The kernel may coalesce the JSON response and the NUL handshake
		// into one message; the fd arrives in the same SCM_RIGHTS payload.
		payload := append([]byte("{\"id\":1,\"result\":{\"name\":\"x\"}}\n"), 0)
		if _, _, err := server.WriteMsgUnix(payload, unix.UnixRights(int(readFD.Fd())), nil); err != nil {
			t.Errorf("write coalesced handshake: %v", err)
			return
		}
		_ = readFD.Close()
		if _, err := server.Write([]byte("{\"id\":2,\"result\":{}}\n")); err != nil {
			t.Errorf("write next response: %v", err)
		}
	}()

	var response adminResponse
	if err := transport.read(&response); err != nil {
		t.Fatalf("read adopt response: %v", err)
	}
	var meta sessionMeta
	if err := json.Unmarshal(response.Result, &meta); err != nil {
		t.Fatalf("decode adopt result: %v", err)
	}
	if meta.Name != "x" {
		t.Fatalf("meta.Name = %q, want x", meta.Name)
	}
	fd, err := transport.takeFD()
	if err != nil {
		t.Fatalf("takeFD: %v", err)
	}
	received := os.NewFile(uintptr(fd), "received")
	if received == nil {
		t.Fatal("received fd is nil")
	}
	_ = received.Close()

	var next adminResponse
	if err := transport.read(&next); err != nil {
		t.Fatalf("read next response after buffered NUL: %v", err)
	}
	if next.ID != 2 {
		t.Fatalf("response.ID = %d, want 2", next.ID)
	}
}

func TestAdoptRejectsDifferentProtocolBeforePreparingSessions(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "ghostline-version-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "old.admin")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		transport := newAdminTransport(connection)
		var req adminRequest
		if transport.read(&req) != nil {
			return
		}
		result, _ := json.Marshal(adminListResult{Version: "0.9.0"})
		_ = transport.write(adminResponse{ID: req.ID, Result: result}, -1)
	}()
	hub, err := New(Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	if _, err := adoptSessions(context.Background(), socket, hub); err == nil || !strings.Contains(err.Error(), ProtocolVersion) {
		t.Fatalf("Adopt error = %v, want protocol mismatch", err)
	}
}

func startAdminServerWithSnapshot(t *testing.T, socket string, meta sessionMeta, fd *os.File, snapshot []byte, replyToExit bool) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer connection.Close()
		transport := newAdminTransport(connection)
		for {
			var request adminRequest
			if err := transport.read(&request); err != nil {
				return
			}
			switch request.Method {
			case adminMethodList:
				list := adminListResult{Version: ProtocolVersion, Sessions: []sessionMeta{meta}}
				if meta.SpoolPath != "" {
					list.Version = v0CompatibilityProtocolVersion
					list.HandoffVersion = V0HandoffProtocolVersion
				}
				raw, _ := json.Marshal(list)
				_ = transport.write(adminResponse{ID: request.ID, Result: raw}, -1)
			case adminMethodAdopt:
				raw, _ := json.Marshal(meta)
				fdToSend := -1
				if meta.Alive && fd != nil {
					fdToSend = int(fd.Fd())
				}
				_ = transport.write(adminResponse{ID: request.ID, Result: raw}, fdToSend)
				if fd != nil {
					_ = fd.Close()
				}
			case adminMethodSnapshot:
				raw, _ := json.Marshal(adminSnapshotResult{Snapshot: base64.StdEncoding.EncodeToString(snapshot)})
				_ = transport.write(adminResponse{ID: request.ID, Result: raw}, -1)
			case adminMethodCommit:
				raw, _ := json.Marshal(adminBatchResult{Committed: 1})
				_ = transport.write(adminResponse{ID: request.ID, Result: raw}, -1)
			case adminMethodExit:
				if replyToExit {
					_ = transport.write(adminResponse{ID: request.ID, Result: json.RawMessage("{}")}, -1)
				}
				return
			default:
				_ = transport.write(adminResponse{ID: request.ID, Error: "unknown admin method"}, -1)
			}
		}
	}()
}

func attachMetaOutput(t *testing.T, meta *sessionMeta, root string) {
	t.Helper()
	output, err := createOutputLog(root, meta.Name)
	if err != nil {
		t.Fatalf("create output log: %v", err)
	}
	meta.OutputDirectory, meta.OutputGeneration = output.metadata()
	output.close(nil)
	t.Cleanup(func() { _ = os.RemoveAll(meta.OutputDirectory) })
}

func TestRollingAdoptRestoresSnapshot(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()
	socketDir, err := os.MkdirTemp("/tmp", "ghostline-snapshot-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(socketDir)

	vt, err := newVTTerminal(80, 24)
	if err != nil {
		t.Fatalf("newVTTerminal: %v", err)
	}
	vt.Feed([]byte("\x1b[5;10Hhello"))
	nativeSnapshot, err := vt.EncodeState()
	vt.Close()
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	meta := sessionMeta{
		Name:                 "warren_snapshot",
		Cols:                 80,
		Rows:                 24,
		CreatedAt:            time.Now().Unix(),
		PID:                  4242,
		Alive:                false,
		VTScrollbackMaxBytes: 3 << 20,
		Exit:                 &exitMeta{Code: 0},
	}
	attachMetaOutput(t, &meta, outputDir)
	adminSocket := filepath.Join(socketDir, "old.admin")
	startAdminServerWithSnapshot(t, adminSocket, meta, nil, nativeSnapshot, true)

	hub, err := New(Options{
		OutputDir:            outputDir,
		VTScrollbackMaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hub.Close()
	adopted, err := adoptSessions(ctx, adminSocket, hub)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted != 1 {
		t.Fatalf("adopted = %d, want 1", adopted)
	}
	session, err := hub.Get(ctx, "warren_snapshot")
	if err != nil {
		t.Fatal("adopted session missing")
	}
	if got := session.backend.(*localSession).state.scrollbackMaxBytes; got != 3<<20 {
		t.Fatalf("adopted scrollback = %d, want %d", got, 3<<20)
	}
	snapshot, err := session.Replay(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !bytes.Contains(snapshot, []byte("hello")) {
		t.Fatalf("snapshot restore lost content: %q", snapshot)
	}
	if !bytes.HasSuffix(snapshot, []byte("\x1b[5;15H")) {
		t.Fatalf("snapshot restore did not preserve cursor: %q", snapshot[len(snapshot)-min(len(snapshot), 40):])
	}
}

func TestAdoptIgnoresRetirementErrorAfterCommit(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()
	socketDir, err := os.MkdirTemp("/tmp", "ghostline-retirement-error-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(socketDir)

	name := "retirement_error"
	meta := sessionMeta{
		Name:      name,
		Cols:      80,
		Rows:      24,
		CreatedAt: time.Now().Unix(),
		PID:       4242,
		Alive:     false,
		Exit:      &exitMeta{Code: 0},
	}
	attachMetaOutput(t, &meta, outputDir)
	adminSocket := filepath.Join(socketDir, "old.admin")
	// The source confirms commit, then closes the admin connection without a
	// retirement response. The adopted state itself still comes from the
	// snapshot, not by replaying raw output.
	vt, err := newVTTerminal(80, 24)
	if err != nil {
		t.Fatalf("newVTTerminal: %v", err)
	}
	nativeSnapshot, err := vt.EncodeState()
	vt.Close()
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	startAdminServerWithSnapshot(t, adminSocket, meta, nil, nativeSnapshot, false)

	hub, err := New(Options{OutputDir: outputDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hub.Close()
	adopted, err := adoptSessions(ctx, adminSocket, hub)
	if err != nil {
		t.Fatalf("Adopt returned an error after commit: %v", err)
	}
	if adopted != 1 {
		t.Fatalf("adopted = %d, want 1", adopted)
	}
	if _, err := hub.Get(ctx, name); err != nil {
		t.Fatalf("adopted session %q missing after retirement error", name)
	}
}

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
		if socketReady(socket) && socketReady(socket+".admin") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !socketReady(socket) || !socketReady(socket+".admin") {
		t.Fatalf("server %s not ready", tag)
	}
	return server, socket
}

func waitSessionOutput(t *testing.T, session *Session, needle string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := session.Replay(context.Background())
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
	session, err := oldServer.hub.Start(ctx, SessionOptions{Name: "mig", Process: ProcessSpec{Path: "sh"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(ctx, []byte("echo before-migrate\r")); err != nil {
		t.Fatalf("Input before migrate: %v", err)
	}
	waitSessionOutput(t, session, "before-migrate")
	if err := session.WriteInput(ctx, []byte("trap 'printf \"migrated-%s\\r\\n\" signal' USR1; printf 'migrated-%s\\r\\n' ready\r")); err != nil {
		t.Fatalf("install signal trap: %v", err)
	}
	waitSessionOutput(t, session, "migrated-ready")
	checkpoint, err := session.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("Checkpoint before migrate: %v", err)
	}

	newServer, _ := startMigrateServer(t, outputDir, "new")
	adopted, err := adoptSessions(ctx, oldSocket+".admin", newServer.hub)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted != 1 {
		t.Fatalf("adopted = %d, want 1", adopted)
	}
	deadline := time.Now().Add(3 * time.Second)
	for socketReady(oldSocket) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if socketReady(oldSocket) {
		t.Fatal("old server still serving after adoption")
	}

	adoptedSession, err := newServer.hub.Get(ctx, "mig")
	if err != nil {
		t.Fatal("adopted session missing on new server")
	}
	status, err := adoptedSession.Status(ctx)
	if err != nil {
		t.Fatalf("Status after migrate: %v", err)
	}
	if !status.Alive {
		t.Fatal("child died during migration")
	}
	if err := adoptedSession.Signal(ctx, syscall.SIGUSR1); err != nil {
		t.Fatalf("Signal after migrate: %v", err)
	}
	waitSessionOutput(t, adoptedSession, "migrated-signal")
	readerCtx, cancelReader := context.WithTimeout(ctx, 5*time.Second)
	defer cancelReader()
	reader, err := adoptedSession.Output(readerCtx, checkpoint.Cursor)
	if err != nil {
		t.Fatalf("Output after migrate: %v", err)
	}
	defer reader.Close()
	if err := adoptedSession.WriteInput(ctx, []byte("echo after-migrate\r")); err != nil {
		t.Fatalf("Input after migrate: %v", err)
	}
	waitSessionOutput(t, adoptedSession, "after-migrate")

	// The adopted session must still render the pre-migration history.
	snapshot, err := adoptedSession.Replay(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !bytes.Contains(snapshot, []byte("before-migrate")) {
		t.Fatalf("snapshot lost pre-migration history: %q", snapshot)
	}
	buffer := make([]byte, 256)
	var raw []byte
	for !bytes.Contains(raw, []byte("after-migrate")) {
		n, readErr := reader.Read(buffer)
		raw = append(raw, buffer[:n]...)
		if readErr != nil {
			t.Fatalf("read migrated output: %v", readErr)
		}
	}
}

func TestRollingAdoptPreservesStoppedExit(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()
	oldServer, oldSocket := startMigrateServer(t, outputDir, "stopped")
	stopped, err := oldServer.hub.Start(ctx, SessionOptions{Name: "stopped", Process: Shell("exit 7")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var original *ExitError
	if err := stopped.Wait(ctx); !errors.As(err, &original) || original.Code != 7 {
		t.Fatalf("original Wait = %v, want exit code 7", err)
	}

	newServer, _ := startMigrateServer(t, outputDir, "stopped-new")
	if adopted, err := adoptSessions(ctx, oldSocket+".admin", newServer.hub); err != nil || adopted != 1 {
		t.Fatalf("Adopt = (%d, %v), want (1, nil)", adopted, err)
	}
	adoptedSession, err := newServer.hub.Get(ctx, "stopped")
	if err != nil {
		t.Fatal("stopped session missing after adoption")
	}
	var transferred *ExitError
	if err := adoptedSession.Wait(ctx); !errors.As(err, &transferred) || transferred.Code != 7 {
		t.Fatalf("adopted Wait = %v, want exit code 7", err)
	}
}

func TestMigratedChildReportsUnknownExit(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()
	oldServer, oldSocket := startMigrateServer(t, outputDir, "unknown")
	if _, err := oldServer.hub.Start(ctx, SessionOptions{Name: "unknown", Process: ProcessSpec{Path: "sh"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	newServer, _ := startMigrateServer(t, outputDir, "unknown-new")
	if adopted, err := adoptSessions(ctx, oldSocket+".admin", newServer.hub); err != nil || adopted != 1 {
		t.Fatalf("Adopt = (%d, %v), want (1, nil)", adopted, err)
	}
	adoptedSession, err := newServer.hub.Get(ctx, "unknown")
	if err != nil {
		t.Fatal("session missing after adoption")
	}
	if err := adoptedSession.WriteInput(ctx, []byte("exit 9\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	var exit *ExitError
	if err := adoptedSession.Wait(ctx); !errors.As(err, &exit) || !exit.Unknown {
		t.Fatalf("adopted Wait = %v, want unknown exit", err)
	}
}

func TestAdminSocketIsPrivate(t *testing.T) {
	_, socket := startMigrateServer(t, t.TempDir(), "permissions")
	info, err := os.Stat(socket + ".admin")
	if err != nil {
		t.Fatalf("Stat admin socket: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("admin socket mode = %o, want 600", got)
	}
}

func TestAdoptRollsBackPreparedSessions(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()
	oldServer, oldSocket := startMigrateServer(t, outputDir, "rollback")
	for _, name := range []string{"first", "second"} {
		if _, err := oldServer.hub.Start(ctx, SessionOptions{Name: name, Process: ProcessSpec{Path: "sh"}}); err != nil {
			t.Fatalf("Start %s: %v", name, err)
		}
	}
	// Keep the source's active descriptor alive but make the destination
	// unable to open the second session's active segment. The first state is
	// prepared before the second one fails, exercising the whole abort path.
	second := oldServer.hub.session("second")
	secondDirectory, secondGeneration := second.output.metadata()
	if err := os.Remove(outputSegmentPath(secondDirectory, secondGeneration)); err != nil {
		t.Fatalf("remove second output segment: %v", err)
	}
	target, _ := startMigrateServer(t, t.TempDir(), "rollback-new")
	if _, err := adoptSessions(ctx, oldSocket+".admin", target.hub); err == nil {
		t.Fatal("Adopt succeeded despite missing output segment")
	}
	if !socketReady(oldSocket) {
		t.Fatal("old server stopped after failed adoption")
	}
	first, err := oldServer.hub.Get(ctx, "first")
	if err != nil {
		t.Fatal("first session disappeared after rollback")
	}
	if err := first.WriteInput(ctx, []byte("echo rollback-ok\r")); err != nil {
		t.Fatalf("Input after rollback: %v", err)
	}
	waitSessionOutput(t, first, "rollback-ok")
}

func TestAdoptHonorsCanceledContext(t *testing.T) {
	_, oldSocket := startMigrateServer(t, t.TempDir(), "canceled")
	target, _ := startMigrateServer(t, t.TempDir(), "canceled-new")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := adoptSessions(ctx, oldSocket+".admin", target.hub); !errors.Is(err, context.Canceled) {
		t.Fatalf("Adopt error = %v, want context canceled", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled Adopt took %s", elapsed)
	}
	if !socketReady(oldSocket) {
		t.Fatal("old server stopped after canceled adoption")
	}
}

// TestMigrationAbortKeepsServing verifies that a failed adoption resumes the
// old server's copy loop instead of leaving the session paused forever.
func TestMigrationAbortKeepsServing(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()
	oldServer, oldSocket := startMigrateServer(t, outputDir, "abort")
	session, err := oldServer.hub.Start(ctx, SessionOptions{Name: "keep", Process: ProcessSpec{Path: "sh"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	state := oldServer.hub.session("keep")
	ticket, err := state.beginMigration()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ticket.stable:
	case <-time.After(2 * time.Second):
		t.Fatal("migration did not reach stable point")
	}
	if err := state.finishMigration(ticket, false); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteInput(ctx, []byte("echo still-alive\r")); err != nil {
		t.Fatalf("Input after abort: %v", err)
	}
	waitSessionOutput(t, session, "still-alive")
	if !socketReady(oldSocket) {
		t.Fatal("old server stopped after aborted migration")
	}
}

// TestFinishMigrationDoesNotHangWhenCopyLoopIsGone covers the race where a
// child exits and starts reaping at the same moment a migration ticket is
// created: the copy loop has already stopped reading, so it can never mark
// the ticket stopped, and finishMigration must settle for done closing
// instead of waiting forever.
func TestFinishMigrationDoesNotHangWhenCopyLoopIsGone(t *testing.T) {
	state := &sessionState{
		done:   make(chan struct{}),
		reaped: make(chan struct{}),
	}
	state.operationMu.Lock()
	ticket := newMigrationTicket(true)
	state.migration = ticket

	finished := make(chan error, 1)
	go func() {
		finished <- state.finishMigration(ticket, false)
	}()

	close(state.done)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("finishMigration = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("finishMigration blocked forever after copy loop exited")
	}
	state.migrationMu.Lock()
	defer state.migrationMu.Unlock()
	if state.migration != nil {
		t.Fatal("migration ticket not cleared")
	}
}
