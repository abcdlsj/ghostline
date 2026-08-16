package ghostline

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
)

// Server owns PTY sessions in a standalone process so clients (for example a
// headless daemon) can restart without ending any session. The server writes
// raw PTY bytes to the same append-only spool files; clients read those files
// directly for incremental output and recovery.
//
// The wire protocol is one JSON object per line on a Unix socket. Binary
// payloads (input, snapshots) are base64 fields.
type Server struct {
	hub      *Hub
	listener net.Listener
}

func NewServer(options Options) (*Server, error) {
	hub, err := New(options)
	if err != nil {
		return nil, err
	}
	return &Server{hub: hub}, nil
}

// Serve listens on socketPath and handles requests until the listener closes.
// The socket directory must exist and be private.
func (s *Server) Serve(socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("create ghostline socket directory: %w", err)
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen ghostline socket: %w", err)
	}
	s.listener = listener
	defer listener.Close()
	defer os.Remove(socketPath)
	for {
		connection, err := listener.Accept()
		if err != nil {
			return err
		}
		go s.handle(connection)
	}
}

// Close stops accepting connections. In-flight handlers finish before the
// process exits.
func (s *Server) Close() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

type request struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func (s *Server) handle(connection net.Conn) {
	defer connection.Close()
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				_, _ = writer.WriteString(marshalResponse(-1, nil, err))
				_ = writer.Flush()
			}
			return
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			_, _ = writer.WriteString(marshalResponse(-1, nil, err))
			_ = writer.Flush()
			return
		}
		result, err := s.dispatch(req.Method, req.Params)
		_, _ = writer.WriteString(marshalResponse(req.ID, result, err))
		_ = writer.Flush()
	}
}

func (s *Server) dispatch(method string, raw json.RawMessage) (any, error) {
	ctx := context.Background()
	switch method {
	case "create":
		var params struct {
			Name    string `json:"name"`
			Dir     string `json:"dir"`
			Command string `json:"command"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		return nil, s.hub.Create(ctx, params.Name, params.Dir, params.Command)
	case "input":
		var params struct {
			Name string `json:"name"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(params.Data)
		if err != nil {
			return nil, err
		}
		return nil, s.hub.Input(ctx, params.Name, data)
	case "resize":
		var params struct {
			Name string `json:"name"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		return nil, s.hub.Resize(ctx, params.Name, params.Cols, params.Rows)
	case "capture":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		snapshot, err := s.hub.Capture(ctx, params.Name)
		if err != nil {
			return nil, err
		}
		return map[string]string{"data": base64.StdEncoding.EncodeToString(snapshot)}, nil
	case "checkpoint":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		session, ok := s.hub.Session(params.Name)
		if !ok {
			return nil, ErrSessionNotFound
		}
		checkpoint, err := session.Checkpoint(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"replay": base64.StdEncoding.EncodeToString(checkpoint.Replay),
			"offset": checkpoint.Offset,
		}, nil
	case "createdAt":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		session, ok := s.hub.Session(params.Name)
		if !ok {
			return nil, ErrSessionNotFound
		}
		return map[string]int64{"created": session.CreatedAt().Unix()}, nil
	case "kill":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		return nil, s.hub.Kill(ctx, params.Name)
	case "exists":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		return map[string]bool{"exists": s.hub.Exists(ctx, params.Name)}, nil
	case "list":
		sessions, err := s.hub.List(ctx)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(sessions))
		for name := range sessions {
			names = append(names, name)
		}
		return map[string][]string{"sessions": names}, nil
	case "ensurePipe":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		return nil, s.hub.EnsurePipe(ctx, params.Name)
	case "spoolPath":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		return map[string]string{"path": s.hub.SpoolPath(params.Name)}, nil
	case "spoolSize":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		size, err := s.hub.SpoolSize(ctx, params.Name)
		if err != nil {
			return nil, err
		}
		return map[string]int64{"size": size}, nil
	case "truncateSpool":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		return nil, s.hub.TruncateSpool(ctx, params.Name)
	case "archiveSpool":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		return nil, s.hub.ArchiveSpool(ctx, params.Name)
	case "removeSpool":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		s.hub.RemoveSpool(params.Name)
		return nil, nil
	case "recover":
		var params struct {
			Name   string `json:"name"`
			Offset int64  `json:"offset"`
			End    int64  `json:"end"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		data, err := s.hub.Recover(ctx, params.Name, params.Offset, params.End)
		if err != nil {
			return nil, err
		}
		return map[string]string{"data": base64.StdEncoding.EncodeToString(data)}, nil
	case "listCreated":
		created, err := s.hub.ListCreated(ctx)
		if err != nil {
			return nil, err
		}
		result := make(map[string]int64, len(created))
		for name, when := range created {
			result[name] = when.Unix()
		}
		return map[string]map[string]int64{"created": result}, nil
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

func marshalResponse(id int64, result any, err error) string {
	value := response{ID: id}
	if err != nil {
		value.Error = err.Error()
	} else if result != nil {
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			value.Error = marshalErr.Error()
		} else {
			value.Result = encoded
		}
	}
	encoded, _ := json.Marshal(value)
	return string(encoded) + "\n"
}

// Ping reports whether a ghostline server is accepting connections on the
// socket. It is also used by the client bootstrap to wait for a server.
func Ping(socketPath string) bool {
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}
