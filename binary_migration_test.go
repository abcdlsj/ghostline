package ghostline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestBinaryV0ToV1Handoff exercises two real ghostline executables. Set
// GHOSTLINE_V0_BINARY to a v0.8 binary and GHOSTLINE_V1_BINARY to a v1 binary
// to enable it; ordinary unit and CI jobs skip this opt-in process test.
func TestBinaryV0ToV1Handoff(t *testing.T) {
	v0Binary := os.Getenv("GHOSTLINE_V0_BINARY")
	v1Binary := os.Getenv("GHOSTLINE_V1_BINARY")
	if v0Binary == "" || v1Binary == "" {
		t.Skip("set GHOSTLINE_V0_BINARY and GHOSTLINE_V1_BINARY for real binary handoff")
	}
	if runtime.GOOS == "windows" {
		t.Skip("ghostline binary integration requires Unix sockets")
	}

	root := testSocketRoot(t)
	oldSocket := filepath.Join(root, "old.sock")
	newSocket := filepath.Join(root, "new.sock")
	oldOutput := filepath.Join(root, "old-output")
	newOutput := filepath.Join(root, "new-output")
	old := startTestDaemon(t, v0Binary, oldSocket, oldOutput)
	waitDaemonSockets(t, oldSocket, old)

	if err := legacyCreateSession(oldSocket, "legacy", "printf 'v0-handoff-output\\n'; while :; do sleep 1; done"); err != nil {
		t.Fatalf("create v0 session: %v", err)
	}

	new := startTestDaemon(t, v1Binary, newSocket, newOutput, "--adopt-from", oldSocket+".admin")
	waitDaemonSockets(t, newSocket, new)
	_ = new

	client := NewClient(newSocket)
	session, err := client.Get(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("v1 client cannot find handed-off session: %v", err)
	}
	waitReplayContains(t, session, []byte("v0-handoff-output"))
	if err := session.WriteInput(context.Background(), []byte("printf 'post-handoff\\n'\r")); err != nil {
		t.Fatalf("write input after handoff: %v", err)
	}
	waitReplayContains(t, session, []byte("post-handoff"))

	if !waitProcessDone(old, 5*time.Second) {
		t.Fatal("v0 daemon did not retire after successful handoff")
	}
	if !socketReady(oldSocket) {
		// Expected: the v0 public endpoint is retired after commit.
	} else {
		t.Fatal("v0 public socket still accepts connections after handoff")
	}
}

// TestBinaryMigrationCrashWindows kills a real source daemon at each
// pre-commit phase and immediately after commit. Pre-commit failures must not
// make the target serve a partial inventory; after commit the target remains
// the owner even when source retirement is interrupted.
func TestBinaryMigrationCrashWindows(t *testing.T) {
	v1Binary := os.Getenv("GHOSTLINE_V1_BINARY")
	if v1Binary == "" {
		t.Skip("set GHOSTLINE_V1_BINARY for process crash-window rehearsal")
	}
	if runtime.GOOS == "windows" {
		t.Skip("ghostline binary integration requires Unix sockets")
	}

	for _, phase := range []migrationCrashPhase{crashAfterList, crashAfterAdopt, crashAfterSnapshot, crashAfterCommit} {
		phase := phase
		t.Run(string(phase), func(t *testing.T) {
			root := testSocketRoot(t)
			oldSocket := filepath.Join(root, "old.sock")
			proxySocket := filepath.Join(root, "proxy.admin")
			newSocket := filepath.Join(root, "new.sock")
			old := startTestDaemon(t, v1Binary, oldSocket, filepath.Join(root, "old-output"))
			waitDaemonSockets(t, oldSocket, old)
			client := NewClient(oldSocket)
			if _, err := client.Start(context.Background(), SessionOptions{
				Name:    "crash-window",
				Process: Shell("printf 'crash-window-output\\n'; while :; do sleep 1; done"),
			}); err != nil {
				t.Fatalf("create source session: %v", err)
			}

			proxy := startAdminFaultProxy(t, proxySocket, oldSocket+".admin", phase, func() {
				_ = old.kill()
			})
			_ = proxy
			new := startTestDaemon(t, v1Binary, newSocket, filepath.Join(root, "new-output"), "--adopt-from", proxySocket)

			if phase == crashAfterCommit {
				waitDaemonSockets(t, newSocket, new)
				handedOff, err := NewClient(newSocket).Get(context.Background(), "crash-window")
				if err != nil {
					t.Fatalf("target did not retain ownership after post-commit source crash: %v", err)
				}
				waitReplayContains(t, handedOff, []byte("crash-window-output"))
				if !waitProcessDone(old, 5*time.Second) {
					t.Fatal("source did not die in commit crash window")
				}
				return
			}

			if !waitProcessDone(new, 5*time.Second) {
				t.Fatal("target stayed alive after pre-commit source crash")
			}
			if socketReady(newSocket) {
				t.Fatal("target exposed a socket after an incomplete handoff")
			}
			if !waitProcessDone(old, 5*time.Second) {
				t.Fatal("source kill did not complete")
			}
		})
	}
}

type migrationCrashPhase string

const (
	crashAfterList     migrationCrashPhase = "after-list"
	crashAfterAdopt    migrationCrashPhase = "after-adopt"
	crashAfterSnapshot migrationCrashPhase = "after-snapshot"
	crashAfterCommit   migrationCrashPhase = "after-commit"
)

type testDaemon struct {
	cmd     *exec.Cmd
	done    chan struct{}
	logPath string
	mu      sync.Mutex
	err     error
}

func startTestDaemon(t *testing.T, binary, socket, output string, extra ...string) *testDaemon {
	t.Helper()
	args := []string{"serve", "--socket", socket, "--output-dir", output}
	args = append(args, extra...)
	cmd := exec.Command(binary, args...)
	logFile, err := os.CreateTemp(testSocketRoot(t), "daemon-log-")
	if err != nil {
		t.Fatalf("create daemon log: %v", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("start daemon %s: %v", binary, err)
	}
	d := &testDaemon{cmd: cmd, done: make(chan struct{}), logPath: logFile.Name()}
	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		d.mu.Lock()
		d.err = err
		d.mu.Unlock()
		close(d.done)
	}()
	t.Cleanup(func() {
		_ = d.kill()
		select {
		case <-d.done:
		case <-time.After(5 * time.Second):
			t.Errorf("daemon did not exit: %s", socket)
		}
	})
	return d
}

func (d *testDaemon) kill() error {
	if d == nil || d.cmd == nil || d.cmd.Process == nil {
		return nil
	}
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()
	err := d.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func waitProcessDone(d *testDaemon, timeout time.Duration) bool {
	if d == nil {
		return true
	}
	select {
	case <-d.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func testSocketRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ghostline-bin-")
	if err != nil {
		t.Fatalf("create socket root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func waitDaemonSockets(t *testing.T, socket string, daemons ...*testDaemon) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if socketReady(socket) && socketReady(socket+".admin") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, daemon := range daemons {
		if daemon != nil {
			if data, err := os.ReadFile(daemon.logPath); err == nil && len(data) > 0 {
				t.Fatalf("daemon sockets not ready: %s\ndaemon log:\n%s", socket, data)
			}
		}
	}
	t.Fatalf("daemon sockets not ready: %s", socket)
}

func waitReplayContains(t *testing.T, session *Session, needle []byte) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		replay, err := session.Replay(context.Background())
		if err == nil && bytes.Contains(replay, needle) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("replay did not contain %q", needle)
}

type v0RPCResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func legacyCreateSession(socket, name, command string) error {
	connection, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	request := map[string]any{
		"id": 1, "method": "create",
		"params": map[string]any{"name": name, "command": command, "cols": 120, "rows": 36},
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if _, err := connection.Write(append(encoded, '\n')); err != nil {
		return err
	}
	response, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		return err
	}
	var decoded v0RPCResponse
	if err := json.Unmarshal(response, &decoded); err != nil {
		return err
	}
	if decoded.Error != nil {
		return fmt.Errorf("v0 create: %s", decoded.Error.Message)
	}
	return nil
}

type adminFaultProxy struct {
	listener *net.UnixListener
	phase    migrationCrashPhase
	kill     func()
	trigger  sync.Once
}

func startAdminFaultProxy(t *testing.T, socket, upstream string, phase migrationCrashPhase, kill func()) *adminFaultProxy {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatalf("listen admin proxy: %v", err)
	}
	proxy := &adminFaultProxy{listener: listener, phase: phase, kill: kill}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	go func() {
		for {
			client, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			go proxy.forward(client, upstream)
		}
	}()
	return proxy
}

func (p *adminFaultProxy) forward(client *net.UnixConn, upstream string) {
	defer client.Close()
	server, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: upstream, Net: "unix"})
	if err != nil {
		return
	}
	defer server.Close()
	fromClient := newAdminTransport(client)
	toSource := newAdminTransport(server)
	for {
		var request adminRequest
		if err := fromClient.read(&request); err != nil {
			return
		}
		if err := toSource.write(request, -1); err != nil {
			return
		}
		var response adminResponse
		if err := toSource.read(&response); err != nil {
			return
		}
		fd := -1
		if request.Method == adminMethodAdopt && response.Error == "" {
			var meta sessionMeta
			if err := json.Unmarshal(response.Result, &meta); err == nil && meta.Alive {
				fd, err = toSource.takeFD()
				if err != nil {
					return
				}
			}
		}
		if err := fromClient.write(response, fd); err != nil {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			return
		}
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		if migrationPhaseForRequest(request.Method) == p.phase {
			p.trigger.Do(p.kill)
			return
		}
	}
}

func migrationPhaseForRequest(method string) migrationCrashPhase {
	switch method {
	case adminMethodList:
		return crashAfterList
	case adminMethodAdopt:
		return crashAfterAdopt
	case adminMethodSnapshot:
		return crashAfterSnapshot
	case adminMethodCommit:
		return crashAfterCommit
	default:
		return ""
	}
}
