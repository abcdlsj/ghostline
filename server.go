package ghostline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Server owns PTY sessions in a standalone process so clients can restart
// without ending any session. The wire protocol is one JSON object per line
// on a Unix socket; []byte fields use JSON base64 encoding automatically.
type Server struct {
	hub      *Hub
	mu       sync.Mutex
	listener net.Listener
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
	go func() {
		<-ctx.Done()
		closeQuietly(listener)
	}()

	slots := make(chan struct{}, maxConnections)
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
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
