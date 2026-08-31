package ghostline

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Server owns PTY sessions in a standalone process so clients can restart
// without ending any session. The wire protocol uses bounded JSON envelopes
// with optional exact-length raw payloads over a Unix socket.
type Server struct {
	hub                  *Hub
	maxClientConnections int
	mu                   sync.Mutex
	listener             net.Listener
	admin                net.Listener
	stopping             atomic.Bool
}

// NewServer constructs a server with its own hub.
func NewServer(options Options) (*Server, error) {
	maxClientConnections := options.ServerMaxClientConnections
	if maxClientConnections < 0 {
		return nil, errors.New("ghostline: negative server max client connections")
	}
	if maxClientConnections == 0 {
		maxClientConnections = DefaultServerMaxClientConnections
	}
	hub, err := New(options)
	if err != nil {
		return nil, err
	}
	return &Server{hub: hub, maxClientConnections: maxClientConnections}, nil
}

// Serve listens on socketPath and handles requests until ctx is canceled or
// the listener fails. The socket is created with mode 0600.
func (s *Server) Serve(ctx context.Context, socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	listener, err := listenUnix(socketPath)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		closeQuietly(listener)
		removeQuietly(socketPath)
		return fmt.Errorf("chmod socket: %w", err)
	}
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	defer closeQuietly(listener)
	defer removeQuietly(socketPath)
	adminPath := socketPath + ".admin"
	adminListener, err := listenUnix(adminPath)
	if err != nil {
		return fmt.Errorf("listen admin: %w", err)
	}
	if err := os.Chmod(adminPath, 0o600); err != nil {
		closeQuietly(adminListener)
		removeQuietly(adminPath)
		return fmt.Errorf("chmod admin socket: %w", err)
	}
	s.mu.Lock()
	s.admin = adminListener
	s.mu.Unlock()
	defer closeQuietly(adminListener)
	defer removeQuietly(adminPath)
	go func() {
		<-ctx.Done()
		closeQuietly(listener)
		closeQuietly(adminListener)
	}()
	go s.adminLoop(adminListener)

	slots := make(chan struct{}, s.maxClientConnections)
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if s.stopping.Load() {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				s.handle(connection)
			}()
		default:
			closeQuietly(connection)
		}
	}
}

func (s *Server) adminLoop(listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go s.handleAdmin(connection)
	}
}

// handleAdmin serves the rolling-upgrade protocol (RFC 0002): list/adopt/
// snapshot/exit over JSON lines, with PTY masters transferred via SCM_RIGHTS.
func (s *Server) handleAdmin(connection net.Conn) {
	unixConn, ok := connection.(*net.UnixConn)
	if !ok {
		closeQuietly(connection)
		return
	}
	defer closeQuietly(unixConn)
	transport := newAdminTransport(unixConn)
	defer transport.closeReceived()
	pending := make(map[string]pendingAdoption)
	batchFrozen := false
	defer func() {
		for _, adoption := range pending {
			_ = s.resolveAdoption(adoption, false)
		}
		if batchFrozen {
			s.hub.endMigrationBatch()
		}
	}()
	endBatchIfIdle := func() {
		if batchFrozen && len(pending) == 0 {
			s.hub.endMigrationBatch()
			batchFrozen = false
		}
	}
	for {
		_ = unixConn.SetReadDeadline(time.Now().Add(adminTimeout))
		var request adminRequest
		if err := transport.read(&request); err != nil {
			return
		}
		switch request.Method {
		case adminMethodList:
			if batchFrozen || !s.hub.beginMigrationBatch() {
				_ = transport.write(adminResponse{ID: request.ID, Error: "migration batch already active"}, -1)
				continue
			}
			batchFrozen = true
			states := s.hub.sessionStates()
			result := adminListResult{Version: ProtocolVersion, Sessions: make([]sessionMeta, 0, len(states))}
			var listErr error
			for _, state := range states {
				meta, err := sessionMetaOf(state)
				if err != nil {
					listErr = err
					break
				}
				result.Sessions = append(result.Sessions, meta)
			}
			if listErr != nil {
				s.hub.endMigrationBatch()
				batchFrozen = false
				_ = transport.write(adminResponse{ID: request.ID, Error: listErr.Error()}, -1)
				continue
			}
			if err := writeAdminResult(transport, request.ID, result, -1); err != nil {
				return
			}
		case adminMethodAdopt:
			var params adoptParams
			if err := json.Unmarshal(request.Params, &params); err != nil {
				_ = transport.write(adminResponse{ID: request.ID, Error: err.Error()}, -1)
				continue
			}
			if !batchFrozen {
				_ = transport.write(adminResponse{ID: request.ID, Error: "send list before adopt"}, -1)
				continue
			}
			if _, exists := pending[params.Name]; exists {
				_ = transport.write(adminResponse{ID: request.ID, Error: "session already prepared"}, -1)
				continue
			}
			state := s.hub.session(params.Name)
			if state == nil {
				_ = transport.write(adminResponse{ID: request.ID, Error: ErrSessionNotFound.Error()}, -1)
				continue
			}
			ticket, err := state.beginMigration()
			if err != nil {
				_ = transport.write(adminResponse{ID: request.ID, Error: err.Error()}, -1)
				continue
			}
			adoption := pendingAdoption{state: state, ticket: ticket}
			pending[params.Name] = adoption
			select {
			case <-ticket.stable:
			case <-time.After(adminTimeout):
				_ = s.resolveAdoption(adoption, false)
				delete(pending, params.Name)
				_ = transport.write(adminResponse{ID: request.ID, Error: "migration timed out"}, -1)
				continue
			}
			if err := ticket.error(); err != nil {
				_ = s.resolveAdoption(adoption, false)
				delete(pending, params.Name)
				_ = transport.write(adminResponse{ID: request.ID, Error: err.Error()}, -1)
				continue
			}
			meta, err := sessionMetaLocked(state)
			if err != nil {
				_ = s.resolveAdoption(adoption, false)
				delete(pending, params.Name)
				_ = transport.write(adminResponse{ID: request.ID, Error: err.Error()}, -1)
				continue
			}
			fd := -1
			if ticket.alive {
				fd = int(state.master.Fd())
			}
			if err := writeAdminResult(transport, request.ID, meta, fd); err != nil {
				return
			}
		case adminMethodSnapshot:
			var params adoptParams
			if err := json.Unmarshal(request.Params, &params); err != nil {
				_ = transport.write(adminResponse{ID: request.ID, Error: err.Error()}, -1)
				continue
			}
			adoption, ok := pending[params.Name]
			if !ok {
				_ = transport.write(adminResponse{ID: request.ID, Error: "session not prepared for adoption"}, -1)
				continue
			}
			adoption.state.outputMu.Lock()
			snapshot, snapshotErr := adoption.state.vt.EncodeState()
			// Keep an ANSI display replay beside the native payload. It gives
			// a newer Ghostty build a useful fallback if it rejects an older
			// native snapshot during restore.
			replay, replayErr := adoption.state.vt.Snapshot()
			adoption.state.outputMu.Unlock()
			result := adminSnapshotResult{}
			if snapshotErr == nil {
				result.Snapshot = base64.StdEncoding.EncodeToString(snapshot)
			} else {
				result.Lossy = true
				result.Reason = snapshotErr.Error()
			}
			if replayErr == nil && len(replay) <= adminMaxReplayBytes {
				result.Replay = base64.StdEncoding.EncodeToString(replay)
			} else if snapshotErr != nil && replayErr == nil {
				result.Reason += "; ANSI replay exceeds migration limit"
			} else if snapshotErr != nil && replayErr != nil {
				result.Reason += "; ANSI replay failed: " + replayErr.Error()
			}
			trimMigrationReplayForFrame(request.ID, &result)
			if err := writeAdminResult(transport, request.ID, result, -1); err != nil {
				return
			}
		case adminMethodCommit, adminMethodAbort:
			var params adminBatchParams
			if err := json.Unmarshal(request.Params, &params); err != nil {
				_ = transport.write(adminResponse{ID: request.ID, Error: err.Error()}, -1)
				continue
			}
			adoptions := make([]pendingAdoption, 0, len(params.Names))
			seen := make(map[string]struct{}, len(params.Names))
			missing := false
			for _, name := range params.Names {
				if _, duplicate := seen[name]; duplicate {
					missing = true
					continue
				}
				seen[name] = struct{}{}
				adoption, exists := pending[name]
				if !exists {
					missing = true
					continue
				}
				adoptions = append(adoptions, adoption)
			}
			if missing {
				_ = transport.write(adminResponse{ID: request.ID, Error: "one or more sessions not prepared"}, -1)
				continue
			}
			commit := request.Method == adminMethodCommit
			if commit {
				// A ticket can become unstable if the child exits while the
				// batch is being prepared. Preflight every ticket before
				// resolving any of them so ownership cannot split halfway
				// through a commit.
				var unstable error
				for _, adoption := range adoptions {
					if err := adoption.ticket.error(); err != nil {
						unstable = err
						break
					}
					if adoption.state.currentMigration() != adoption.ticket {
						unstable = errMigrationStale
						break
					}
				}
				if unstable != nil {
					for _, adoption := range adoptions {
						if adoption.state.currentMigration() == adoption.ticket {
							_ = s.resolveAdoption(adoption, false)
						}
						delete(pending, adoption.state.name)
					}
					if batchFrozen {
						endBatchIfIdle()
					}
					_ = transport.write(adminResponse{ID: request.ID, Error: unstable.Error()}, -1)
					continue
				}
			}
			var resolveErr error
			for _, adoption := range adoptions {
				if err := s.resolveAdoption(adoption, commit); err != nil && resolveErr == nil {
					resolveErr = err
				}
			}
			if resolveErr != nil && commit {
				// This should be unreachable after preflight, but leave the old
				// hub internally consistent if a ticket changes unexpectedly.
				for _, adoption := range adoptions {
					if adoption.state.currentMigration() == adoption.ticket {
						_ = s.resolveAdoption(adoption, false)
					}
					delete(pending, adoption.state.name)
					if adoption.state.migrated.Load() {
						s.hub.remove(adoption.state.name)
					}
				}
				if batchFrozen {
					endBatchIfIdle()
				}
				_ = transport.write(adminResponse{ID: request.ID, Error: resolveErr.Error()}, -1)
				return
			}
			for _, adoption := range adoptions {
				delete(pending, adoption.state.name)
				if commit {
					s.hub.remove(adoption.state.name)
				}
			}
			if err := writeAdminResult(transport, request.ID, adminBatchResult{Committed: len(adoptions)}, -1); err != nil {
				return
			}
			if commit {
				endBatchIfIdle()
			}
		case adminMethodExit:
			if len(pending) != 0 {
				_ = transport.write(adminResponse{ID: request.ID, Error: "migration batch is not committed"}, -1)
				continue
			}
			if err := writeAdminResult(transport, request.ID, struct{}{}, -1); err != nil {
				return
			}
			s.requestExit()
			return
		default:
			_ = transport.write(adminResponse{ID: request.ID, Error: fmt.Sprintf("unknown admin method: %s", request.Method)}, -1)
		}
	}
}

func trimMigrationReplayForFrame(id int64, result *adminSnapshotResult) {
	if result == nil || result.Replay == "" {
		if migrationSnapshotFits(id, result) {
			return
		}
		result.Snapshot = ""
		result.Lossy = true
		if result.Reason == "" {
			result.Reason = "native snapshot exceeds migration limit"
		}
		return
	}
	if migrationSnapshotFits(id, result) {
		return
	}
	// The native payload alone is still useful and remains bounded by the
	// regular admin frame check. Drop only the optional recovery hint first.
	result.Replay = ""
	if migrationSnapshotFits(id, result) {
		return
	}
	// A very large native snapshot is less useful than retaining the PTY with
	// a blank terminal. Let the target take the normal lossy recovery path.
	result.Snapshot = ""
	result.Lossy = true
	if result.Reason == "" {
		result.Reason = "native snapshot exceeds migration limit"
	}
}

func migrationSnapshotFits(id int64, result *adminSnapshotResult) bool {
	if result == nil {
		return true
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return false
	}
	frame, err := json.Marshal(adminResponse{ID: id, Result: encodedResult})
	return err == nil && len(frame)+1 <= adminMaxFrame
}

func writeAdminResult(transport *adminTransport, id int64, result any, fd int) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return transport.write(adminResponse{ID: id, Error: err.Error()}, -1)
	}
	return transport.write(adminResponse{ID: id, Result: encoded}, fd)
}

func (s *Server) resolveAdoption(adoption pendingAdoption, commit bool) error {
	err := adoption.state.finishMigration(adoption.ticket, commit)
	if err != nil && commit {
		return err
	}
	return nil
}

// requestExit stops both listeners so Serve returns and the process exits
// after a rolling upgrade.
func (s *Server) requestExit() {
	s.stopping.Store(true)
	_ = s.Close()
	s.mu.Lock()
	admin := s.admin
	s.mu.Unlock()
	closeQuietly(admin)
}

// listenUnix binds socketPath without unlinking a live server's socket.
func listenUnix(socketPath string) (net.Listener, error) {
	if _, err := os.Lstat(socketPath); err == nil {
		if socketReady(socketPath) {
			return nil, fmt.Errorf("socket already in use: %s", socketPath)
		}
		removeQuietly(socketPath)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return net.Listen("unix", socketPath)
}

// Close stops accepting connections. Sessions keep running.
func (s *Server) Close() error {
	s.mu.Lock()
	listener := s.listener
	s.mu.Unlock()
	if listener == nil {
		return nil
	}
	return listener.Close()
}

// Shutdown stops accepting connections and terminates every session.
func (s *Server) Shutdown(ctx context.Context) error {
	_ = s.Close()
	done := make(chan error, 1)
	go func() { done <- s.hub.Close() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) handle(connection net.Conn) {
	defer closeQuietly(connection)
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	for {
		_ = connection.SetReadDeadline(time.Now().Add(rpcIdleTimeout))
		req, err := readRequest(reader)
		if err != nil {
			_ = writeResponse(writer, -1, nil, err)
			return
		}
		_ = connection.SetReadDeadline(time.Time{})
		if req.Method == rpcMethodOutput {
			if len(req.payload) != 0 {
				_ = writeResponse(writer, req.ID, nil, errors.New("output open request cannot contain a payload"))
				return
			}
			s.serveOutput(connection, reader, writer, req)
			return
		}
		if req.Method == rpcMethodReplay || req.Method == rpcMethodCheckpoint || req.Method == rpcMethodAtomicState {
			if len(req.payload) != 0 {
				_ = writeResponse(writer, req.ID, nil, errors.New("blob open request cannot contain a payload"))
				return
			}
			s.serveBlob(connection, reader, writer, req)
			return
		}

		result, dispatchErr := s.dispatchRequest(connection, req)
		if err := writeResponse(writer, req.ID, result, dispatchErr); err != nil {
			return
		}
	}
}

func (s *Server) serveBlob(connection net.Conn, wireReader *bufio.Reader, writer *bufio.Writer, req request) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	monitored := make(chan struct{})
	go func() {
		defer close(monitored)
		monitorConnection(connection, cancel, stop)
	}()

	session, err := s.namedSession(ctx, req.Params)
	var data []byte
	opened := blobOpenResult{}
	if err == nil {
		switch req.Method {
		case rpcMethodReplay:
			data, err = session.Replay(ctx)
		case rpcMethodCheckpoint:
			var checkpoint Checkpoint
			checkpoint, err = session.Checkpoint(ctx)
			data = checkpoint.Replay
			opened.Cursor = checkpoint.Cursor
		case rpcMethodAtomicState:
			var state AtomicState
			state, err = session.AtomicState(ctx)
			data = state.Payload
			opened.Cursor = state.Cursor
			opened.Format = state.Format
		}
	}
	close(stop)
	_ = connection.SetReadDeadline(time.Now())
	<-monitored
	_ = connection.SetReadDeadline(time.Time{})
	if err != nil {
		_ = writeResponse(writer, req.ID, nil, err)
		return
	}
	opened.Size = len(data)
	if err := writeResponse(writer, req.ID, opened, nil); err != nil {
		return
	}

	offset := 0
	for {
		_ = connection.SetReadDeadline(time.Now().Add(rpcIdleTimeout))
		next, err := readRequest(wireReader)
		if err != nil {
			_ = writeResponse(writer, -1, nil, err)
			return
		}
		_ = connection.SetReadDeadline(time.Time{})
		if len(next.payload) != 0 {
			_ = writeResponse(writer, next.ID, nil, errors.New("blob control request cannot contain a payload"))
			continue
		}
		switch next.Method {
		case rpcMethodBlobClose:
			_ = writeResponse(writer, next.ID, nil, nil)
			return
		case rpcMethodBlobRead:
			readParams, err := decode[chunkReadParams](next.Params)
			if err != nil {
				_ = writeResponse(writer, next.ID, nil, err)
				continue
			}
			if readParams.MaxBytes <= 0 || readParams.MaxBytes > maxRPCChunk {
				_ = writeResponse(writer, next.ID, nil, errors.New("invalid blob chunk size"))
				continue
			}
			end := offset + readParams.MaxBytes
			if end > len(data) {
				end = len(data)
			}
			chunk := data[offset:end]
			result := blobReadResult{EOF: end == len(data)}
			offset = end
			if err := writeRawResponse(writer, next.ID, result, chunk, nil); err != nil {
				return
			}
			if result.EOF {
				return
			}
		default:
			_ = writeResponse(writer, next.ID, nil, fmt.Errorf("unknown blob method: %s", next.Method))
		}
	}
}

func (s *Server) serveOutput(connection net.Conn, wireReader *bufio.Reader, writer *bufio.Writer, req request) {
	params, err := decode[outputParams](req.Params)
	if err != nil {
		_ = writeResponse(writer, req.ID, nil, err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := s.hub.Get(ctx, params.Name)
	if err != nil {
		_ = writeResponse(writer, req.ID, nil, err)
		return
	}
	output, err := session.Output(ctx, params.Cursor)
	if err != nil {
		_ = writeResponse(writer, req.ID, nil, err)
		return
	}
	defer output.Close()
	if err := writeResponse(writer, req.ID, cursorResult{Cursor: output.Cursor()}, nil); err != nil {
		return
	}
	for {
		_ = connection.SetReadDeadline(time.Now().Add(rpcIdleTimeout))
		next, err := readRequest(wireReader)
		if err != nil {
			_ = writeResponse(writer, -1, nil, err)
			return
		}
		_ = connection.SetReadDeadline(time.Time{})
		if len(next.payload) != 0 {
			_ = writeResponse(writer, next.ID, nil, errors.New("output control request cannot contain a payload"))
			continue
		}
		switch next.Method {
		case rpcMethodOutputClose:
			_ = writeResponse(writer, next.ID, nil, nil)
			return
		case rpcMethodOutputRead:
			readParams, err := decode[chunkReadParams](next.Params)
			if err != nil {
				_ = writeResponse(writer, next.ID, nil, err)
				continue
			}
			if readParams.MaxBytes <= 0 || readParams.MaxBytes > maxRPCChunk {
				_ = writeResponse(writer, next.ID, nil, errors.New("invalid output chunk size"))
				continue
			}
			buffer := make([]byte, readParams.MaxBytes)
			stop := make(chan struct{})
			monitored := make(chan struct{})
			go func() {
				defer close(monitored)
				monitorConnection(connection, cancel, stop)
			}()
			n, readErr := output.Read(buffer)
			close(stop)
			_ = connection.SetReadDeadline(time.Now())
			<-monitored
			_ = connection.SetReadDeadline(time.Time{})
			result := outputReadResult{Cursor: output.Cursor()}
			if errors.Is(readErr, io.EOF) {
				result.EOF = true
				readErr = nil
			}
			if err := writeRawResponse(writer, next.ID, result, buffer[:n], readErr); err != nil {
				return
			}
			if result.EOF {
				return
			}
		default:
			_ = writeResponse(writer, next.ID, nil, fmt.Errorf("unknown output method: %s", next.Method))
		}
	}
}

func (s *Server) dispatchRequest(connection net.Conn, req request) (any, error) {
	if req.Method != rpcMethodWait {
		return s.dispatch(context.Background(), req.Method, req.Params, req.payload)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	monitored := make(chan struct{})
	go func() {
		defer close(monitored)
		monitorConnection(connection, cancel, stop)
	}()
	result, err := s.dispatch(ctx, req.Method, req.Params, req.payload)
	close(stop)
	_ = connection.SetReadDeadline(time.Now())
	<-monitored
	_ = connection.SetReadDeadline(time.Time{})
	cancel()
	return result, err
}

// monitorConnection cancels wait dispatch when the client stops reading.
func monitorConnection(connection net.Conn, cancel context.CancelFunc, stop <-chan struct{}) {
	buffer := make([]byte, 1)
	select {
	case <-stop:
		return
	default:
	}
	// The protocol is strictly serial: while an operation is running, any
	// client byte is either a disconnect indication or an invalid pipelined
	// request. Block without periodic deadlines. The operation owner closes
	// stop and sets an immediate deadline to wake this read when work finishes.
	_ = connection.SetReadDeadline(time.Time{})
	select {
	case <-stop:
		return
	default:
	}
	_, _ = connection.Read(buffer)
	select {
	case <-stop:
		return
	default:
		cancel()
	}
}

func (s *Server) session(ctx context.Context, name string) (*Session, error) {
	return s.hub.Get(ctx, name)
}

func (s *Server) namedSession(ctx context.Context, raw json.RawMessage) (*Session, error) {
	params, err := decode[nameParams](raw)
	if err != nil {
		return nil, err
	}
	return s.session(ctx, params.Name)
}

func (s *Server) dispatch(ctx context.Context, method string, raw json.RawMessage, payload []byte) (any, error) {
	if method != rpcMethodWriteInput && len(payload) != 0 {
		return nil, errors.New("RPC method does not accept a payload")
	}
	switch method {
	case rpcMethodVersion:
		return versionResult{
			Version: ProtocolVersion, TagVersion: TagVersion(),
			Capabilities: append([]string(nil), protocolCapabilities...), Limits: currentProtocolLimits,
			MaxClientConnections: s.maxClientConnections,
		}, nil

	case rpcMethodCreate:
		params, err := decode[createParams](raw)
		if err != nil {
			return nil, err
		}
		session, err := s.hub.Start(ctx, SessionOptions{
			Name: params.Name,
			Process: ProcessSpec{
				Path:         params.Path,
				Args:         params.Args,
				Directory:    params.Dir,
				Environment:  params.Env,
				ShellCommand: params.ShellCommand,
			},
			Size:                 Size{Columns: params.Cols, Rows: params.Rows},
			VTScrollbackMaxBytes: params.VTScrollbackMaxBytes,
		})
		if err != nil {
			return nil, err
		}
		return createResult{Info: session.Info()}, nil

	case rpcMethodCreated:
		session, err := s.namedSession(ctx, raw)
		if err != nil {
			return nil, err
		}
		return createResult{Info: session.Info()}, nil

	case rpcMethodStatus:
		session, err := s.namedSession(ctx, raw)
		if err != nil {
			return nil, err
		}
		return session.Status(ctx)

	case rpcMethodMetadata:
		session, err := s.namedSession(ctx, raw)
		if err != nil {
			return nil, err
		}
		metadata, err := session.Metadata(ctx)
		if err != nil {
			return nil, err
		}
		return metadataResult{Process: metadata.Process, Directory: metadata.Directory}, nil

	case rpcMethodSize:
		session, err := s.namedSession(ctx, raw)
		if err != nil {
			return nil, err
		}
		size, err := session.Size(ctx)
		if err != nil {
			return nil, err
		}
		return sizeResult{Columns: size.Columns, Rows: size.Rows}, nil

	case rpcMethodSignal:
		params, err := decode[signalParams](raw)
		if err != nil {
			return nil, err
		}
		session, err := s.session(ctx, params.Name)
		if err != nil {
			return nil, err
		}
		return nil, session.Signal(ctx, syscall.Signal(params.Signal))

	case rpcMethodWait:
		session, err := s.namedSession(ctx, raw)
		if err != nil {
			return nil, err
		}
		_ = session.Wait(ctx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return session.Status(ctx)

	case rpcMethodTerminate:
		session, err := s.namedSession(ctx, raw)
		if err != nil {
			return nil, err
		}
		return nil, session.Terminate(ctx)

	case rpcMethodDelete:
		session, err := s.namedSession(ctx, raw)
		if err != nil {
			return nil, err
		}
		if err := session.Delete(ctx); err != nil {
			return nil, err
		}
		return nil, nil

	case rpcMethodWriteInput:
		params, err := decode[inputParams](raw)
		if err != nil {
			return nil, err
		}
		session, err := s.session(ctx, params.Name)
		if err != nil {
			return nil, err
		}
		return nil, session.WriteInput(ctx, payload)

	case rpcMethodResize:
		params, err := decode[resizeParams](raw)
		if err != nil {
			return nil, err
		}
		session, err := s.session(ctx, params.Name)
		if err != nil {
			return nil, err
		}
		return nil, session.Resize(ctx, Size{Columns: params.Cols, Rows: params.Rows})

	case rpcMethodRotateOutput:
		session, err := s.namedSession(ctx, raw)
		if err != nil {
			return nil, err
		}
		cursor, err := session.RotateOutput(ctx)
		if err != nil {
			return nil, err
		}
		return cursorResult{Cursor: cursor}, nil

	case rpcMethodOutputCursor:
		session, err := s.namedSession(ctx, raw)
		if err != nil {
			return nil, err
		}
		cursor, err := session.OutputCursor(ctx)
		if err != nil {
			return nil, err
		}
		return cursorResult{Cursor: cursor}, nil

	case rpcMethodPruneOutput:
		params, err := decode[pruneOutputParams](raw)
		if err != nil {
			return nil, err
		}
		session, err := s.session(ctx, params.Name)
		if err != nil {
			return nil, err
		}
		return nil, session.PruneOutput(ctx, params.Before)

	case rpcMethodList:
		sessions, err := s.hub.List(ctx)
		if err != nil {
			return nil, err
		}
		infos := make([]SessionInfo, 0, len(sessions))
		for _, session := range sessions {
			infos = append(infos, session.Info())
		}
		return listResult{Sessions: infos}, nil

	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}
