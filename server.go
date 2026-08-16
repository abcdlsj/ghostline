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
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return fmt.Errorf("chmod socket: %w", err)
	}
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	defer listener.Close()
	defer os.Remove(socketPath)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
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
			_ = connection.Close()
		}
	}
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
	defer connection.Close()
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
		result, dispatchErr := s.dispatch(req.Method, req.Params)
		if err := writeResponse(writer, req.ID, result, dispatchErr); err != nil {
			return
		}
	}
}

func (s *Server) dispatch(method string, raw json.RawMessage) (any, error) {
	ctx := context.Background()
	switch method {
	case "create":
		var params struct {
			Name    string   `json:"name"`
			Dir     string   `json:"dir"`
			Command string   `json:"command"`
			Cols    int      `json:"cols"`
			Rows    int      `json:"rows"`
			Env     []string `json:"env"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
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
		return map[string]int64{"created": session.CreatedAt().Unix()}, nil

	case "status":
		name, err := nameParam(raw)
		if err != nil {
			return nil, err
		}
		session, ok := s.hub.Session(name)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, name)
		}
		return session.status(), nil

	case "close":
		name, err := nameParam(raw)
		if err != nil {
			return nil, err
		}
		session, ok := s.hub.Session(name)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, name)
		}
		return nil, session.Close()

	case "remove":
		name, err := nameParam(raw)
		if err != nil {
			return nil, err
		}
		session, ok := s.hub.Session(name)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, name)
		}
		status := session.status()
		if err := session.Remove(); err != nil {
			return nil, err
		}
		return map[string]any{"exit": status.Exit}, nil

	case "input":
		var params struct {
			Name string `json:"name"`
			Data []byte `json:"data"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		session, ok := s.hub.Session(params.Name)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, params.Name)
		}
		return nil, session.Input(ctx, params.Data)

	case "resize":
		var params struct {
			Name string `json:"name"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		session, ok := s.hub.Session(params.Name)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, params.Name)
		}
		return nil, session.Resize(ctx, Size{Columns: params.Cols, Rows: params.Rows})

	case "snapshot":
		name, err := nameParam(raw)
		if err != nil {
			return nil, err
		}
		session, ok := s.hub.Session(name)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, name)
		}
		data, err := session.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		return map[string][]byte{"data": data}, nil

	case "checkpoint":
		name, err := nameParam(raw)
		if err != nil {
			return nil, err
		}
		session, ok := s.hub.Session(name)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, name)
		}
		checkpoint, err := session.Checkpoint(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"replay": checkpoint.Replay,
			"offset": checkpoint.Offset,
		}, nil

	case "recover":
		var params struct {
			Name   string `json:"name"`
			Offset int64  `json:"offset"`
			End    int64  `json:"end"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		path := s.hub.spoolPath(params.Name)
		if path == "" {
			return nil, fmt.Errorf("%w: %s", ErrInvalidSessionName, params.Name)
		}
		data, err := readSpool(path, params.Offset, params.End)
		if err != nil {
			return nil, err
		}
		return map[string][]byte{"data": data}, nil

	case "spoolPath":
		name, err := nameParam(raw)
		if err != nil {
			return nil, err
		}
		return map[string]string{"path": s.hub.spoolPath(name)}, nil

	case "spoolSize":
		name, err := nameParam(raw)
		if err != nil {
			return nil, err
		}
		size, err := spoolSize(s.hub.spoolPath(name))
		if err != nil {
			return nil, err
		}
		return map[string]int64{"size": size}, nil

	case "truncateSpool":
		name, err := nameParam(raw)
		if err != nil {
			return nil, err
		}
		return nil, truncateSpool(s.hub.spoolPath(name))

	case "archiveSpool":
		name, err := nameParam(raw)
		if err != nil {
			return nil, err
		}
		return nil, archiveSpool(s.hub.spoolPath(name))

	case "removeSpool":
		name, err := nameParam(raw)
		if err != nil {
			return nil, err
		}
		removeSpool(s.hub.spoolPath(name))
		return nil, nil

	case "list":
		sessions := s.hub.Sessions()
		names := make([]string, 0, len(sessions))
		for _, session := range sessions {
			names = append(names, session.Name())
		}
		return map[string][]string{"sessions": names}, nil

	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

func nameParam(raw json.RawMessage) (string, error) {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", err
	}
	return params.Name, nil
}
