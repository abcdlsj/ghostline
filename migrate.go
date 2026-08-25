package ghostline

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The admin protocol is deliberately tiny. JSON gives the control plane a
// readable shape; one Unix message carries each response and, when needed,
// its SCM_RIGHTS payload. Keeping those two pieces together is the important
// part: a stream reader must never consume the byte that carries a file fd.
const (
	adminMethodList     = "list"
	adminMethodAdopt    = "adopt"
	adminMethodSnapshot = "snapshot"
	adminMethodCommit   = "commit"
	adminMethodAbort    = "abort"
	adminMethodExit     = "exit"

	adminTimeout  = 2 * time.Second
	adminMaxFrame = 64 << 20
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

type exitMeta struct {
	Code    int    `json:"code"`
	Signal  string `json:"signal,omitempty"`
	Unknown bool   `json:"unknown,omitempty"`
}

type sessionMeta struct {
	Name                 string    `json:"name"`
	Cols                 int       `json:"cols"`
	Rows                 int       `json:"rows"`
	CreatedAt            int64     `json:"createdAt"`
	PID                  int       `json:"pid"`
	Alive                bool      `json:"alive"`
	VTScrollbackMaxBytes uint64    `json:"vtScrollbackMaxBytes,omitempty"`
	OutputDirectory      string    `json:"outputDirectory"`
	OutputGeneration     uint64    `json:"outputGeneration"`
	Exit                 *exitMeta `json:"exit,omitempty"`
}

type adminListResult struct {
	Version  string        `json:"version"`
	Sessions []sessionMeta `json:"sessions"`
}

type adminSnapshotResult struct {
	Snapshot string `json:"snapshot"`
}

type adminBatchResult struct {
	Committed int `json:"committed"`
}

type pendingAdoption struct {
	state  *sessionState
	ticket *migrationTicket
}

// adminTransport is the only code allowed to read or write the management
// socket. It owns frame buffering and fd reception, so callers can stay at
// the level of typed request/response values.
type adminTransport struct {
	conn     *net.UnixConn
	readBuf  []byte
	received []int
}

func newAdminTransport(conn *net.UnixConn) *adminTransport {
	return &adminTransport{conn: conn}
}

func (t *adminTransport) write(value any, fd int) error {
	frame, err := json.Marshal(value)
	if err != nil {
		return err
	}
	frame = append(frame, '\n')
	if len(frame) > adminMaxFrame {
		return errors.New("admin message too large")
	}

	var oob []byte
	if fd >= 0 {
		oob = unix.UnixRights(fd)
	}
	if err := t.conn.SetWriteDeadline(time.Now().Add(adminTimeout)); err != nil {
		return err
	}
	written, _, err := t.conn.WriteMsgUnix(frame, oob, nil)
	if err != nil {
		return err
	}
	for written < len(frame) {
		n, err := t.conn.Write(frame[written:])
		written += n
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (t *adminTransport) read(value any) error {
	for {
		// An fd handshake may arrive as a separate one-byte NUL message. When
		// the kernel coalesces that message with the JSON response, the NUL
		// remains buffered after the newline; strip it so the next frame still
		// starts at valid JSON.
		t.readBuf = bytes.TrimLeft(t.readBuf, "\x00")
		if index := bytes.IndexByte(t.readBuf, '\n'); index >= 0 {
			if index > adminMaxFrame {
				return errors.New("admin message too large")
			}
			frame := t.readBuf[:index]
			t.readBuf = t.readBuf[index+1:]
			if err := json.Unmarshal(frame, value); err != nil {
				return err
			}
			return nil
		}
		if len(t.readBuf) > adminMaxFrame {
			return errors.New("admin message too large")
		}
		if err := t.conn.SetReadDeadline(time.Now().Add(adminTimeout)); err != nil {
			return err
		}
		payload := make([]byte, 64*1024)
		oob := make([]byte, unix.CmsgSpace(4))
		n, oobn, flags, _, err := t.conn.ReadMsgUnix(payload, oob)
		if flags&unix.MSG_CTRUNC != 0 {
			return errors.New("admin control message truncated")
		}
		if n > 0 {
			t.readBuf = append(t.readBuf, payload[:n]...)
		}
		if oobn > 0 {
			messages, parseErr := unix.ParseSocketControlMessage(oob[:oobn])
			if parseErr != nil {
				return parseErr
			}
			for _, message := range messages {
				fds, parseErr := unix.ParseUnixRights(&message)
				if parseErr != nil {
					return parseErr
				}
				t.received = append(t.received, fds...)
			}
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}
}

func (t *adminTransport) takeFD() (int, error) {
	if len(t.received) == 0 {
		// The fd handshake may arrive after the JSON response instead of in the
		// same message. Read until the descriptor arrives, discarding the NUL
		// padding without losing any JSON that shares the read.
		for {
			if err := t.conn.SetReadDeadline(time.Now().Add(adminTimeout)); err != nil {
				return -1, err
			}
			payload := make([]byte, 64*1024)
			oob := make([]byte, unix.CmsgSpace(4))
			n, oobn, flags, _, err := t.conn.ReadMsgUnix(payload, oob)
			if flags&unix.MSG_CTRUNC != 0 {
				return -1, errors.New("admin control message truncated")
			}
			if oobn > 0 {
				messages, parseErr := unix.ParseSocketControlMessage(oob[:oobn])
				if parseErr != nil {
					return -1, parseErr
				}
				for _, message := range messages {
					fds, parseErr := unix.ParseUnixRights(&message)
					if parseErr != nil {
						return -1, parseErr
					}
					t.received = append(t.received, fds...)
				}
			}
			if n > 0 {
				if trimmed := bytes.TrimLeft(payload[:n], "\x00"); len(trimmed) > 0 {
					t.readBuf = append(t.readBuf, trimmed...)
				}
			}
			if len(t.received) > 0 {
				break
			}
			if err != nil {
				return -1, err
			}
			if n == 0 {
				return -1, io.ErrUnexpectedEOF
			}
		}
	}
	fd := t.received[0]
	t.received = t.received[1:]
	return fd, nil
}

func (t *adminTransport) closeReceived() {
	for _, fd := range t.received {
		_ = unix.Close(fd)
	}
	t.received = nil
}

type adminClient struct {
	transport *adminTransport
	nextID    int64
}

func (c *adminClient) call(ctx context.Context, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := c.nextID
	c.nextID++
	request := adminRequest{ID: id, Method: method}
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return err
		}
		request.Params = encoded
	}
	if err := c.transport.write(request, -1); err != nil {
		return contextErr(ctx, err)
	}
	var response adminResponse
	if err := c.transport.read(&response); err != nil {
		return contextErr(ctx, err)
	}
	if response.ID != id {
		return fmt.Errorf("admin response id %d, want %d", response.ID, id)
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	if result != nil && len(response.Result) > 0 {
		if err := json.Unmarshal(response.Result, result); err != nil {
			return err
		}
	}
	return nil
}

func sessionMetaOf(state *sessionState) (sessionMeta, error) {
	state.operationMu.RLock()
	defer state.operationMu.RUnlock()
	return sessionMetaLocked(state)
}

func sessionMetaLocked(state *sessionState) (sessionMeta, error) {
	state.outputMu.Lock()
	defer state.outputMu.Unlock()
	if err := state.output.terminalError(); err != nil {
		return sessionMeta{}, err
	}
	outputDirectory, outputGeneration := state.output.metadata()
	meta := sessionMeta{
		Name:                 state.name,
		Cols:                 state.size.Columns,
		Rows:                 state.size.Rows,
		CreatedAt:            state.createdAt.Unix(),
		PID:                  state.pid,
		Alive:                true,
		VTScrollbackMaxBytes: state.scrollbackMaxBytes,
		OutputDirectory:      outputDirectory,
		OutputGeneration:     outputGeneration,
	}
	select {
	case <-state.done:
		meta.Alive = false
	default:
	}
	if !meta.Alive {
		state.waitMu.Lock()
		meta.Exit = exitMetaOf(state.waitErr)
		state.waitMu.Unlock()
	}
	return meta, nil
}

func exitMetaOf(err error) *exitMeta {
	if err == nil {
		return nil
	}
	exit, ok := convertExit(err).(*ExitError)
	if !ok {
		return &exitMeta{Code: -1, Unknown: true}
	}
	return &exitMeta{Code: exit.Code, Signal: exit.Signal, Unknown: exit.Unknown}
}

func (m *exitMeta) error() *ExitError {
	if m == nil {
		return nil
	}
	return &ExitError{Code: m.Code, Signal: m.Signal, Unknown: m.Unknown}
}

func dialAdmin(ctx context.Context, socket string) (*net.UnixConn, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("connect admin socket: %w", err)
	}
	unixConn, ok := connection.(*net.UnixConn)
	if !ok {
		closeQuietly(connection)
		return nil, errors.New("admin socket is not a Unix connection")
	}
	return unixConn, nil
}

func cancelAdminOnContext(ctx context.Context, connection *net.UnixConn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeQuietly(connection)
		case <-done:
		}
	}()
	return func() { close(done) }
}

// adoptSessions migrates every source session into h as one transaction.
func adoptSessions(ctx context.Context, adminSocket string, h *Hub) (int, error) {
	if h == nil {
		return 0, errors.New("adopt target hub is nil")
	}

	// A target hub is a destination, not a merge point. Holding this write
	// lock also keeps Close and Start from racing the final map insertion.
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0, ErrClosed
	}
	if len(h.sessions) != 0 || len(h.pending) != 0 {
		h.mu.Unlock()
		return 0, errors.New("adopt target hub is not empty")
	}
	h.mu.Unlock()
	if err := os.MkdirAll(h.outputDir, 0o700); err != nil {
		return 0, fmt.Errorf("create output dir: %w", err)
	}

	connection, err := dialAdmin(ctx, adminSocket)
	if err != nil {
		return 0, err
	}
	stopContextWatcher := cancelAdminOnContext(ctx, connection)
	defer stopContextWatcher()
	defer closeQuietly(connection)

	transport := newAdminTransport(connection)
	defer transport.closeReceived()
	client := &adminClient{transport: transport, nextID: 1}

	var listed adminListResult
	if err := client.call(ctx, adminMethodList, nil, &listed); err != nil {
		return 0, err
	}
	if listed.Version != ProtocolVersion {
		return 0, fmt.Errorf("rolling upgrade protocol %q is not %q", listed.Version, ProtocolVersion)
	}
	prepared := make([]*sessionState, 0, len(listed.Sessions))
	preparedNames := make([]string, 0, len(listed.Sessions))
	committed := false
	abort := func() {
		if committed || len(preparedNames) == 0 {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.Background(), adminTimeout)
		_ = client.call(abortCtx, adminMethodAbort, adminBatchParams{Names: preparedNames}, nil)
		cancel()
		for _, state := range prepared {
			state.close()
		}
	}
	defer abort()

	for _, meta := range listed.Sessions {
		var adopted sessionMeta
		// The process may exit between list and adopt. Ask for the response
		// first, then decide whether that response should carry an fd from its
		// own Alive bit rather than trusting the initial list snapshot.
		if err := client.call(ctx, adminMethodAdopt, adoptParams{Name: meta.Name}, &adopted); err != nil {
			return 0, err
		}
		preparedNames = append(preparedNames, meta.Name)

		var master *os.File
		if adopted.Alive {
			masterFD, err := transport.takeFD()
			if err != nil {
				return 0, err
			}
			master = os.NewFile(uintptr(masterFD), "adopted-master")
		}
		var snapshotResult adminSnapshotResult
		if err := client.call(ctx, adminMethodSnapshot, adoptParams{Name: adopted.Name}, &snapshotResult); err != nil {
			closeFileQuietly(master)
			return 0, fmt.Errorf("encode snapshot for %s: %w", meta.Name, err)
		}
		snapshot, err := base64.StdEncoding.DecodeString(snapshotResult.Snapshot)
		if err != nil {
			closeFileQuietly(master)
			return 0, fmt.Errorf("decode snapshot for %s: %w", meta.Name, err)
		}
		state, err := adoptState(
			adopted.Name,
			master,
			snapshot,
			Size{Columns: adopted.Cols, Rows: adopted.Rows},
			adopted.OutputDirectory,
			adopted.OutputGeneration,
			time.Unix(adopted.CreatedAt, 0),
			adopted.PID,
			adopted.Exit.error(),
			resolveVTScrollbackMaxBytes(adopted.VTScrollbackMaxBytes, h.defaultVTScrollbackMaxBytes),
		)
		if err != nil {
			return 0, fmt.Errorf("restore snapshot for %s: %w", meta.Name, err)
		}
		prepared = append(prepared, state)
	}

	var batchResult adminBatchResult
	if err := client.call(ctx, adminMethodCommit, adminBatchParams{Names: preparedNames}, &batchResult); err != nil {
		return 0, err
	}
	if batchResult.Committed != len(prepared) {
		return 0, fmt.Errorf("admin committed %d sessions, want %d", batchResult.Committed, len(prepared))
	}
	committed = true

	h.mu.Lock()
	for _, state := range prepared {
		h.sessions[state.name] = state
	}
	h.mu.Unlock()
	for _, state := range prepared {
		if state.master != nil {
			go copyOutput(state)
		}
	}

	// A successful commit transfers ownership. Retiring the old listener is a
	// second, best-effort step: the old endpoint may close without replying to
	// the admin request, so an EOF here is expected. More generally, once
	// ownership has moved, a retirement failure must never make the new server
	// abandon the sessions it now owns. Callers can still independently
	// check/stop the old process if it remains alive.
	retireCtx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	var ignored struct{}
	_ = client.call(retireCtx, adminMethodExit, nil, &ignored)
	_ = waitOldServerGone(retireCtx, adminSocket)
	cancel()
	return len(prepared), nil
}

func waitOldServerGone(ctx context.Context, adminSocket string) error {
	publicSocket := strings.TrimSuffix(adminSocket, ".admin")
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !socketReady(publicSocket) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Adopt migrates sessions into this server. Call it before Serve so the target
// never accepts a client while its session map is being rebuilt.
func (s *Server) Adopt(ctx context.Context, adminSocket string) (int, error) {
	return adoptSessions(ctx, adminSocket, s.hub)
}
