package ghostline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCreateParamsCarriesVTScrollbackLimit(t *testing.T) {
	want := uint64(4 << 20)
	encoded, err := json.Marshal(createParams{VTScrollbackMaxBytes: want})
	if err != nil {
		t.Fatalf("marshal create params: %v", err)
	}
	var decoded createParams
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal create params: %v", err)
	}
	if decoded.VTScrollbackMaxBytes != want {
		t.Fatalf("VTScrollbackMaxBytes = %d, want %d", decoded.VTScrollbackMaxBytes, want)
	}
}

func FuzzReadLine(f *testing.F) {
	f.Add([]byte("hello\n"), 1024)
	f.Add([]byte(""), 1024)
	f.Add(bytes.Repeat([]byte("a"), 2048), 1024)
	f.Fuzz(func(t *testing.T, data []byte, limit int) {
		if limit <= 0 {
			limit = 1
		}
		line, err := readLine(bufio.NewReader(bytes.NewReader(data)), limit)
		if err == nil && len(line) > limit {
			t.Fatalf("line length %d exceeds limit %d", len(line), limit)
		}
	})
}

func TestIsTransportError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "dial refused", err: &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}, want: true},
		{name: "read reset", err: &net.OpError{Op: "read", Net: "unix", Err: syscall.ECONNRESET}, want: true},
		{name: "deadline", err: &net.OpError{Op: "read", Net: "unix", Err: os.ErrDeadlineExceeded}, want: true},
		{name: "eof", err: io.EOF, want: true},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, want: true},
		{name: "wrapped eof", err: fmt.Errorf("read: %w", io.EOF), want: true},
		{name: "broken pipe", err: fmt.Errorf("write: %w", syscall.EPIPE), want: true},
		{name: "rpc session", err: fmt.Errorf("%w: gone", ErrSessionNotFound), want: false},
		{name: "rpc internal", err: decodeRPCError(&rpcError{Code: "internal", Message: "boom"}), want: false},
		{name: "plain", err: errors.New("boom"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isTransportError(test.err); got != test.want {
				t.Fatalf("isTransportError(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestListenUnixDoesNotUnlinkLiveSocket(t *testing.T) {
	dir := t.TempDir()
	socketDir, err := os.MkdirTemp("", "ghostline-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(socketDir) }()
	socket := filepath.Join(socketDir, "ghostline.sock")
	server, err := NewServer(Options{OutputDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(context.Background(), socket)
	}()
	defer func() {
		_ = server.Shutdown(context.Background())
		<-done
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !Ping(socket) {
		if time.Now().After(deadline) {
			t.Fatal("server did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := listenUnix(socket); err == nil {
		t.Fatal("listenUnix bound a live socket")
	}
	if !Ping(socket) {
		t.Fatal("live socket was unlinked")
	}
}

func TestListenUnixReplacesStaleSocket(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "ghostline-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(socketDir) }()
	socket := filepath.Join(socketDir, "ghostline.sock")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := listenUnix(socket)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}
	_ = listener.Close()
	_ = os.Remove(socket)
}
