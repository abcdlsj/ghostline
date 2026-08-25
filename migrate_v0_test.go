package ghostline

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestV0HandoffRebuildsV1OutputLog(t *testing.T) {
	sourceDir := t.TempDir()
	spoolPath := filepath.Join(sourceDir, "legacy.out")
	writeGzipFile(t, spoolPath+".100.gz", []byte("archive-one\n"))
	writeGzipFile(t, spoolPath+".200.gz", []byte("archive-two\n"))
	if err := os.WriteFile(spoolPath, []byte("live-output\n"), 0o600); err != nil {
		t.Fatalf("write live spool: %v", err)
	}

	vt, err := newVTTerminal(80, 24)
	if err != nil {
		t.Fatalf("newVTTerminal: %v", err)
	}
	snapshot, err := vt.EncodeState()
	vt.Close()
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	meta := sessionMeta{
		Name:        "legacy",
		Cols:        80,
		Rows:        24,
		CreatedAt:   time.Now().Unix(),
		PID:         4242,
		SpoolPath:   spoolPath,
		SpoolSize:   int64(len("live-output\n")),
		SpoolFormat: v0SpoolFormat,
		Alive:       false,
		Exit:        &exitMeta{Code: 0},
	}
	socketDir, err := os.MkdirTemp("/tmp", "ghostline-v0-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(socketDir)
	adminSocket := filepath.Join(socketDir, "v0.admin")
	startAdminServerWithSnapshot(t, adminSocket, meta, nil, snapshot, true)

	hub, err := New(Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hub.Close()
	adopted, err := adoptSessions(context.Background(), adminSocket, hub)
	if err != nil {
		t.Fatalf("adoptSessions: %v", err)
	}
	if adopted != 1 {
		t.Fatalf("adopted = %d, want 1", adopted)
	}

	session, err := hub.Get(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reader, err := session.Output(context.Background(), Cursor{})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("read rebuilt output: %v", err)
	}
	want := "archive-one\narchive-two\nlive-output\n"
	if string(data) != want {
		t.Fatalf("rebuilt output = %q, want %q", data, want)
	}
	if got := reader.Cursor().String(); got != "v1:1:36" {
		t.Fatalf("rebuilt cursor = %q, want v1 generation one at offset 36", got)
	}
}

func TestV1RejectsUnknownV0Handoff(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "ghostline-v0-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(socketDir)
	adminSocket := filepath.Join(socketDir, "unknown.admin")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: adminSocket, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		transport := newAdminTransport(connection)
		var request adminRequest
		if transport.read(&request) != nil {
			return
		}
		result, _ := json.Marshal(adminListResult{
			Version:        v0CompatibilityProtocolVersion,
			HandoffVersion: "ghostline-v0-to-v1-unknown",
		})
		_ = transport.write(adminResponse{ID: request.ID, Result: result}, -1)
	}()

	hub, err := New(Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hub.Close()
	if _, err := adoptSessions(context.Background(), adminSocket, hub); err == nil {
		t.Fatal("adoptSessions accepted an unknown v0 handoff")
	}
}

func writeGzipFile(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	writer := gzip.NewWriter(file)
	if _, err := writer.Write(data); err != nil {
		file.Close()
		t.Fatalf("write archive: %v", err)
	}
	if err := writer.Close(); err != nil {
		file.Close()
		t.Fatalf("close archive: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close archive file: %v", err)
	}
}
