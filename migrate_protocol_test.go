package ghostline

import (
	"encoding/json"
	"net"
	"os"
	"testing"
)

func adminConnPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	placeholder, err := os.CreateTemp("", "gl-admin-")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := placeholder.Name()
	_ = placeholder.Close()
	_ = os.Remove(path)
	t.Cleanup(func() { _ = os.Remove(path) })
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan *net.UnixConn, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		accepted <- connection
	}()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("DialUnix: %v", err)
	}
	server := <-accepted
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return server, client
}

func TestAdminTransportKeepsFramesAndFDsTogether(t *testing.T) {
	senderConn, receiverConn := adminConnPair(t)
	sender := newAdminTransport(senderConn)
	receiver := newAdminTransport(receiverConn)
	fdSource, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer fdSource.Close()

	if err := sender.write(adminResponse{ID: 1, Result: json.RawMessage(`{"first":true}`)}, int(fdSource.Fd())); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := sender.write(adminResponse{ID: 2, Result: json.RawMessage(`{"second":true}`)}, -1); err != nil {
		t.Fatalf("write second: %v", err)
	}

	var first, second adminResponse
	if err := receiver.read(&first); err != nil {
		t.Fatalf("read first: %v", err)
	}
	fd, err := receiver.takeFD()
	if err != nil {
		t.Fatalf("take first fd: %v", err)
	}
	defer os.NewFile(uintptr(fd), "received").Close()
	if first.ID != 1 {
		t.Fatalf("first id = %d, want 1", first.ID)
	}
	if err := receiver.read(&second); err != nil {
		t.Fatalf("read second: %v", err)
	}
	if second.ID != 2 {
		t.Fatalf("second id = %d, want 2", second.ID)
	}
}
