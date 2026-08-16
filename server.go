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
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Server owns PTY sessions in a standalone process so clients can restart
// without ending any session. The wire protocol is one JSON object per line
// on a Unix socket; []byte fields use JSON base64 encoding automatically.
type Server struct {
	hub      *Hub
	mu       sync.Mutex
	listener net.Listener
	admin    net.Listener
	stopping atomic.Bool
}

// NewServer constructs a server with its own hub.
func NewServer(options Options) (*Server, error) {
	hub, err := New(options)
	if err != nil {
		return nil, err
	}
	return &Server{hub: hub}, nil
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

	slots := make(chan struct{}, maxConnections)
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
	pending := make(map[string]*sessionState)
	defer func() {
		for _, state := range pending {
			state.abortMigration()
		}
	}()
	reader := bufio.NewReader(unixConn)
	writer := bufio.NewWriter(unixConn)
	for {
		var request adminRequest
		if err := json.NewDecoder(reader).Decode(&request); err != nil {
			return
		}
		switch request.Method {
		case adminMethodList:
			states := s.hub.sessionStates()
			result := adminListResult{Sessions: make([]sessionMeta, 0, len(states))}
			for _, state := range states {
				result.Sessions = append(result.Sessions, sessionMeta{
					Name:      state.name,
					Cols:      state.size.Columns,
					Rows:      state.size.Rows,
					CreatedAt: state.createdAt.Unix(),
					PID:       state.pid,
				})
			}
			writeAdminResult(writer, request.ID, result)
		case adminMethodAdopt:
			var params adoptParams
			if err := json.Unmarshal(request.Params, &params); err != nil {
				writeAdminError(writer, request.ID, err)
				continue
			}
			state := s.hub.session(params.Name)
			if state == nil {
				writeAdminError(writer, request.ID, ErrSessionNotFound)
				continue
			}
			state.beginMigration()
			select {
			case <-state.migrationStable():
			case <-time.After(adoptTimeout):
				state.abortMigration()
				writeAdminError(writer, request.ID, errors.New("migration timed out"))
				continue
			}
			pending[params.Name] = state
			meta := sessionMeta{
				Name:      state.name,
				Cols:      state.size.Columns,
				Rows:      state.size.Rows,
				CreatedAt: state.createdAt.Unix(),
				PID:       state.pid,
			}
			writeAdminResult(writer, request.ID, meta)
			if err := sendFD(unixConn, int(state.master.Fd())); err != nil {
				return
			}
		case adminMethodSnapshot:
			var params adoptParams
			if err := json.Unmarshal(request.Params, &params); err != nil {
				writeAdminError(writer, request.ID, err)
				continue
			}
			state := pending[params.Name]
			if state == nil {
				writeAdminError(writer, request.ID, errors.New("session not prepared for adoption"))
				continue
			}
			state.outputMu.Lock()
			snapshot, err := state.vt.EncodeState()
			state.outputMu.Unlock()
			if err != nil {
				delete(pending, params.Name)
				state.abortMigration()
				writeAdminError(writer, request.ID, err)
				continue
			}
			writeAdminResult(writer, request.ID, adminSnapshotResult{
				Snapshot: base64.StdEncoding.EncodeToString(snapshot),
			})
		case adminMethodCommit, adminMethodAbort:
			var params adminBatchParams
			if err := json.Unmarshal(request.Params, &params); err != nil {
				writeAdminError(writer, request.ID, err)
				continue
			}
			states := make([]*sessionState, 0, len(params.Names))
			missing := false
			for _, name := range params.Names {
				state := pending[name]
				if state == nil {
					missing = true
					continue
				}
				states = append(states, state)
			}
			if missing {
				writeAdminError(writer, request.ID, errors.New("one or more sessions not prepared"))
				continue
			}
			for _, name := range params.Names {
				delete(pending, name)
			}
			for _, state := range states {
				if request.Method == adminMethodCommit {
					state.commitMigration()
				} else {
					state.abortMigration()
				}
			}
			writeAdminResult(writer, request.ID, adminBatchResult{Committed: len(states)})
		case adminMethodExit:
			s.requestExit()
			return
		default:
			writeAdminError(writer, request.ID, fmt.Errorf("unknown admin method: %s", request.Method))
		}
	}
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

func writeAdminResult(writer *bufio.Writer, id int64, result any) {
	encoded, _ := json.Marshal(result)
	_ = json.NewEncoder(writer).Encode(adminResponse{ID: id, Result: encoded})
	_ = writer.Flush()
}

func writeAdminError(writer *bufio.Writer, id int64, err error) {
	_ = json.NewEncoder(writer).Encode(adminResponse{ID: id, Error: err.Error()})
	_ = writer.Flush()
}

// listenUnix binds socketPath without unlinking a live server's socket.
func listenUnix(socketPath string) (net.Listener, error) {
	if _, err := os.Lstat(socketPath); err == nil {
		if Ping(socketPath) {
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
		line, err := readLine(reader, maxRPCLine)
		if err != nil {
			return
		}
		_ = connection.SetReadDeadline(time.Time{})
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = writeResponse(writer, -1, nil, fmt.Errorf("invalid request: %w", err))
			continue
		}

		result, dispatchErr := s.dispatchRequest(connection, req)
		if err := writeResponse(writer, req.ID, result, dispatchErr); err != nil {
			return
		}
	}
}

func (s *Server) dispatchRequest(connection net.Conn, req request) (any, error) {
	if req.Method != rpcMethodWait {
		return s.dispatch(context.Background(), req.Method, req.Params)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	go monitorConnection(connection, cancel, stop)
	result, err := s.dispatch(ctx, req.Method, req.Params)
	close(stop)
	_ = connection.SetReadDeadline(time.Now())
	cancel()
	return result, err
}

// monitorConnection cancels wait dispatch when the client stops reading.
func monitorConnection(connection net.Conn, cancel context.CancelFunc, stop <-chan struct{}) {
	buffer := make([]byte, 1)
	for {
		select {
		case <-stop:
			return
		default:
		}
		_ = connection.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err := connection.Read(buffer)
		if err == nil {
			cancel()
			return
		}
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			continue
		}
		cancel()
		return
	}
}

func (s *Server) session(name string) (Session, error) {
	session, ok := s.hub.Session(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, name)
	}
	return session, nil
}

func (s *Server) namedSession(raw json.RawMessage) (Session, error) {
	params, err := decode[nameParams](raw)
	if err != nil {
		return nil, err
	}
	return s.session(params.Name)
}

func (s *Server) spoolPath(name string) (string, error) {
	path := s.hub.spoolPath(name)
	if path == "" {
		return "", fmt.Errorf("%w: %s", ErrInvalidSessionName, name)
	}
	return path, nil
}

func (s *Server) dispatch(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case rpcMethodVersion:
		return versionResult{Version: ProtocolVersion}, nil

	case rpcMethodCreate:
		params, err := decode[createParams](raw)
		if err != nil {
			return nil, err
		}
		session, err := s.hub.Start(ctx, SessionOptions{
			Name:        params.Name,
			Directory:   params.Dir,
			Command:     params.Command,
			Size:        Size{Columns: params.Cols, Rows: params.Rows},
			Environment: params.Env,
		})
		if err != nil {
			return nil, err
		}
		return createResult{Created: session.CreatedAt().Unix()}, nil

	case rpcMethodCreated:
		session, err := s.namedSession(raw)
		if err != nil {
			return nil, err
		}
		return createResult{Created: session.CreatedAt().Unix()}, nil

	case rpcMethodStatus:
		session, err := s.namedSession(raw)
		if err != nil {
			return nil, err
		}
		return session.Status(ctx)

	case rpcMethodWait:
		session, err := s.namedSession(raw)
		if err != nil {
			return nil, err
		}
		_ = session.Wait(ctx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return session.Status(ctx)

	case rpcMethodClose:
		session, err := s.namedSession(raw)
		if err != nil {
			return nil, err
		}
		return nil, session.Close()

	case rpcMethodRemove:
		session, err := s.namedSession(raw)
		if err != nil {
			return nil, err
		}
		status, err := session.Status(ctx)
		if err != nil {
			return nil, err
		}
		if err := session.Remove(); err != nil {
			return nil, err
		}
		return removeResult{Exit: status.Exit}, nil

	case rpcMethodInput:
		params, err := decode[inputParams](raw)
		if err != nil {
			return nil, err
		}
		session, err := s.session(params.Name)
		if err != nil {
			return nil, err
		}
		return nil, session.Input(ctx, params.Data)

	case rpcMethodResize:
		params, err := decode[resizeParams](raw)
		if err != nil {
			return nil, err
		}
		session, err := s.session(params.Name)
		if err != nil {
			return nil, err
		}
		return nil, session.Resize(ctx, Size{Columns: params.Cols, Rows: params.Rows})

	case rpcMethodSnapshot:
		session, err := s.namedSession(raw)
		if err != nil {
			return nil, err
		}
		data, err := session.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		return dataResult{Data: data}, nil

	case rpcMethodCheckpoint:
		session, err := s.namedSession(raw)
		if err != nil {
			return nil, err
		}
		checkpoint, err := session.Checkpoint(ctx)
		if err != nil {
			return nil, err
		}
		return checkpointResult(checkpoint), nil

	case rpcMethodRecover:
		params, err := decode[recoverParams](raw)
		if err != nil {
			return nil, err
		}
		path, err := s.spoolPath(params.Name)
		if err != nil {
			return nil, err
		}
		data, err := readSpool(path, params.Offset, params.End)
		if err != nil {
			return nil, err
		}
		return dataResult{Data: data}, nil

	case rpcMethodSpoolPath:
		params, err := decode[nameParams](raw)
		if err != nil {
			return nil, err
		}
		path, err := s.spoolPath(params.Name)
		if err != nil {
			return nil, err
		}
		return spoolPathResult{Path: path}, nil

	case rpcMethodSpoolSize:
		params, err := decode[nameParams](raw)
		if err != nil {
			return nil, err
		}
		path, err := s.spoolPath(params.Name)
		if err != nil {
			return nil, err
		}
		size, err := spoolSize(path)
		if err != nil {
			return nil, err
		}
		return spoolSizeResult{Size: size}, nil

	case rpcMethodTruncateSpool:
		params, err := decode[nameParams](raw)
		if err != nil {
			return nil, err
		}
		path, err := s.spoolPath(params.Name)
		if err != nil {
			return nil, err
		}
		return nil, truncateSpool(path)

	case rpcMethodArchiveSpool:
		params, err := decode[nameParams](raw)
		if err != nil {
			return nil, err
		}
		path, err := s.spoolPath(params.Name)
		if err != nil {
			return nil, err
		}
		return nil, archiveSpool(path)

	case rpcMethodRemoveSpool:
		params, err := decode[nameParams](raw)
		if err != nil {
			return nil, err
		}
		path, err := s.spoolPath(params.Name)
		if err != nil {
			return nil, err
		}
		removeSpool(path)
		return nil, nil

	case rpcMethodList:
		sessions := s.hub.Sessions()
		names := make([]string, 0, len(sessions))
		for _, session := range sessions {
			names = append(names, session.Name())
		}
		return listResult{Sessions: names}, nil

	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}
