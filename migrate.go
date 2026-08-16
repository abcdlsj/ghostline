package ghostline

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Rolling upgrade protocol (RFC 0002): a fresh server adopts sessions from an
// old server over its admin socket. Each PTY master is moved with SCM_RIGHTS,
// and the emulator state travels as an encoded snapshot.

const (
	adminMethodList     = "list"
	adminMethodAdopt    = "adopt"
	adminMethodSnapshot = "snapshot"
	adminMethodCommit   = "commit"
	adminMethodAbort    = "abort"
	adminMethodExit     = "exit"

	adoptTimeout = 2 * time.Second
)

type adminRequest struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type adminResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type adoptParams struct {
	Name string `json:"name"`
}

type adminBatchParams struct {
	Names []string `json:"names"`
}

type sessionMeta struct {
	Name      string `json:"name"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	CreatedAt int64  `json:"createdAt"`
	PID       int    `json:"pid"`
}

type adminListResult struct {
	Sessions []sessionMeta `json:"sessions"`
}

type adminSnapshotResult struct {
	Snapshot string `json:"snapshot"`
}

type adminBatchResult struct {
	Committed int `json:"committed"`
}

// sendFD moves a file descriptor over a Unix socket connection.
func sendFD(conn *net.UnixConn, fd int) error {
	oob := unix.UnixRights(fd)
	_, _, err := conn.WriteMsgUnix([]byte{0}, oob, nil)
	return err
}

// recvFD receives a file descriptor over a Unix socket connection.
func recvFD(conn *net.UnixConn) (int, error) {
	buffer := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	_, _, _, _, err := conn.ReadMsgUnix(buffer, oob)
	if err != nil {
		return -1, err
	}
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, err
	}
	for _, message := range messages {
		fds, err := unix.ParseUnixRights(&message)
		if err != nil {
			return -1, err
		}
		if len(fds) > 0 {
			return fds[0], nil
		}
	}
	return -1, errors.New("no file descriptor in control message")
}

func writeAdminRequest(writer *bufio.Writer, id int64, method string, params any) error {
	request := adminRequest{ID: id, Method: method}
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return err
		}
		request.Params = encoded
	}
	if err := json.NewEncoder(writer).Encode(request); err != nil {
		return err
	}
	return writer.Flush()
}

func readAdminResponse(reader *bufio.Reader) (json.RawMessage, error) {
	var response adminResponse
	if err := json.NewDecoder(reader).Decode(&response); err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	return response.Result, nil
}

// Adopt migrates every session from the server listening on adminSocket into
// h. The old server's children keep running; this process becomes their new
// owner. Returns the number of sessions adopted.
func Adopt(ctx context.Context, adminSocket string, h *Hub) (int, error) {
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: adminSocket, Net: "unix"})
	if err != nil {
		return 0, fmt.Errorf("connect admin socket: %w", err)
	}
	defer connection.Close()
	writer := bufio.NewWriter(connection)
	reader := bufio.NewReader(connection)

	if err := writeAdminRequest(writer, 1, adminMethodList, nil); err != nil {
		return 0, err
	}
	raw, err := readAdminResponse(reader)
	if err != nil {
		return 0, err
	}
	var listed adminListResult
	if err := json.Unmarshal(raw, &listed); err != nil {
		return 0, err
	}

	var prepared []*sessionState
	abortAll := func() {
		if len(prepared) == 0 {
			return
		}
		names := make([]string, 0, len(prepared))
		for _, state := range prepared {
			names = append(names, state.name)
		}
		_ = writeAdminRequest(writer, 9, adminMethodAbort, adminBatchParams{Names: names})
		_, _ = readAdminResponse(reader)
		for _, state := range prepared {
			state.close()
		}
		prepared = nil
	}
	for _, meta := range listed.Sessions {
		if err := ctx.Err(); err != nil {
			abortAll()
			return 0, err
		}
		if err := writeAdminRequest(writer, 2, adminMethodAdopt, adoptParams{Name: meta.Name}); err != nil {
			abortAll()
			return 0, err
		}
		if _, err := readAdminResponse(reader); err != nil {
			abortAll()
			return 0, err
		}
		masterFD, err := recvFD(connection)
		if err != nil {
			abortAll()
			return 0, err
		}
		if err := writeAdminRequest(writer, 3, adminMethodSnapshot, adoptParams{Name: meta.Name}); err != nil {
			abortAll()
			return 0, err
		}
		raw, err := readAdminResponse(reader)
		if err != nil {
			abortAll()
			return 0, err
		}
		var snapshotResult adminSnapshotResult
		if err := json.Unmarshal(raw, &snapshotResult); err != nil {
			abortAll()
			return 0, err
		}
		snapshot, err := base64.StdEncoding.DecodeString(snapshotResult.Snapshot)
		if err != nil {
			abortAll()
			return 0, err
		}
		state, err := adoptState(
			meta.Name,
			os.NewFile(uintptr(masterFD), "adopted-master"),
			snapshot,
			Size{Columns: meta.Cols, Rows: meta.Rows},
			h.spoolPath(meta.Name),
			time.Unix(meta.CreatedAt, 0),
			meta.PID,
		)
		if err != nil {
			abortAll()
			return 0, err
		}
		prepared = append(prepared, state)
	}
	names := make([]string, 0, len(prepared))
	for _, state := range prepared {
		names = append(names, state.name)
	}
	if err := writeAdminRequest(writer, 4, adminMethodCommit, adminBatchParams{Names: names}); err != nil {
		abortAll()
		return 0, err
	}
	if _, err := readAdminResponse(reader); err != nil {
		abortAll()
		return 0, err
	}
	h.mu.Lock()
	for _, state := range prepared {
		h.sessions[state.name] = state
	}
	h.mu.Unlock()
	for _, state := range prepared {
		go copyOutput(state)
	}
	_ = writeAdminRequest(writer, 5, adminMethodExit, nil)
	return len(prepared), nil
}

// Adopt migrates every session from the server listening on adminSocket into
// this server. It must be called before Serve so the new process is not
// visible to clients until adoption is complete.
func (s *Server) Adopt(ctx context.Context, adminSocket string) (int, error) {
	return Adopt(ctx, adminSocket, s.hub)
}
