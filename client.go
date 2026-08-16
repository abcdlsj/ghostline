package ghostline

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Client proxies Hub operations to a ghostline Server over a Unix socket.
// Sessions returned by Start are remote handles with the same API as local
// ones, so an embedding process can restart and reconnect without ending any
// session.
type Client struct {
	Socket string
}

func NewClient(socketPath string) *Client {
	return &Client{Socket: socketPath}
}

// Check reports whether the server socket is reachable.
func (c *Client) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !Ping(c.Socket) {
		return fmt.Errorf("ghostline server is not reachable at %s", c.Socket)
	}
	return nil
}

// WaitReady polls the server socket until it accepts connections or the
// context is done.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if Ping(c.Socket) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ghostline server did not become ready at %s", c.Socket)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *Client) call(method string, params any, result any) error {
	connection, err := net.Dial("unix", c.Socket)
	if err != nil {
		return fmt.Errorf("connect ghostline server: %w", err)
	}
	defer connection.Close()
	req := request{ID: 1, Method: method}
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = encoded
	}
	writer := bufio.NewWriter(connection)
	if err := json.NewEncoder(writer).Encode(req); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	var resp response
	if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if result != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}

// Start creates a session on the server and returns its remote handle.
func (c *Client) Start(ctx context.Context, options SessionOptions) (*Session, error) {
	if err := c.call("create", map[string]any{
		"name": options.Name, "dir": options.Directory, "command": options.Command,
	}, nil); err != nil {
		return nil, err
	}
	return &Session{&remoteSession{client: c, name: options.Name}}, nil
}

// remoteSession is the RPC backend behind a remote Session handle.
type remoteSession struct {
	client *Client
	name   string

	doneOnce sync.Once
	done     chan struct{}
}

func (r *remoteSession) Name() string         { return r.name }
func (r *remoteSession) CreatedAt() time.Time { return time.Unix(r.createdAtUnix(), 0) }
func (r *remoteSession) Alive() bool {
	return r.client.Exists(context.Background(), r.name)
}

func (r *remoteSession) Done() <-chan struct{} {
	r.doneOnce.Do(func() {
		r.done = make(chan struct{})
		go func() {
			for r.Alive() {
				time.Sleep(200 * time.Millisecond)
			}
			close(r.done)
		}()
	})
	return r.done
}

func (r *remoteSession) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.Done():
		return nil
	}
}

func (r *remoteSession) Input(ctx context.Context, data []byte) error {
	return r.client.Input(ctx, r.name, data)
}

func (r *remoteSession) Resize(ctx context.Context, size Size) error {
	return r.client.Resize(ctx, r.name, size.Columns, size.Rows)
}

func (r *remoteSession) Snapshot(ctx context.Context) ([]byte, error) {
	return r.client.Capture(ctx, r.name)
}

func (r *remoteSession) Checkpoint(ctx context.Context) (Checkpoint, error) {
	return r.client.Checkpoint(ctx, r.name)
}

func (r *remoteSession) SpoolPath() string {
	return r.client.SpoolPath(r.name)
}

func (r *remoteSession) SpoolSize(ctx context.Context) (int64, error) {
	return r.client.SpoolSize(ctx, r.name)
}

func (r *remoteSession) WatchOutput(options WatchOptions) (*SpoolWatcher, error) {
	watcher, err := NewSpoolWatcher(
		r.SpoolPath(),
		options.Offset,
		options.OnOutput,
		options.OnTruncate,
		options.OnOverflow,
	)
	if err != nil {
		return nil, err
	}
	watcher.SetMaxBytes(options.MaxBytes)
	watcher.Start()
	return watcher, nil
}

func (r *remoteSession) Close() error {
	return r.client.Kill(context.Background(), r.name)
}

// Name-based convenience methods below mirror the Hub API for callers that
// manage sessions by name (for example a daemon implementing a runtime
// interface).

func (c *Client) Create(ctx context.Context, name, directory, command string) error {
	return c.call("create", map[string]any{
		"name": name, "dir": directory, "command": command,
	}, nil)
}

func (c *Client) Exists(ctx context.Context, name string) bool {
	var result struct {
		Exists bool `json:"exists"`
	}
	if err := c.call("exists", map[string]any{"name": name}, &result); err != nil {
		return false
	}
	return result.Exists
}

func (c *Client) Capture(ctx context.Context, name string) ([]byte, error) {
	var result struct {
		Data string `json:"data"`
	}
	if err := c.call("capture", map[string]any{"name": name}, &result); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(result.Data)
}

func (c *Client) Checkpoint(ctx context.Context, name string) (Checkpoint, error) {
	var result struct {
		Replay string `json:"replay"`
		Offset int64  `json:"offset"`
	}
	if err := c.call("checkpoint", map[string]any{"name": name}, &result); err != nil {
		return Checkpoint{}, err
	}
	replay, err := base64.StdEncoding.DecodeString(result.Replay)
	if err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{Replay: replay, Offset: result.Offset}, nil
}

func (c *Client) Input(ctx context.Context, name string, data []byte) error {
	return c.call("input", map[string]any{
		"name": name,
		"data": base64.StdEncoding.EncodeToString(data),
	}, nil)
}

func (c *Client) Resize(ctx context.Context, name string, columns, rows int) error {
	return c.call("resize", map[string]any{
		"name": name, "cols": columns, "rows": rows,
	}, nil)
}

func (c *Client) Kill(ctx context.Context, name string) error {
	return c.call("kill", map[string]any{"name": name}, nil)
}

func (c *Client) List(ctx context.Context) (map[string]bool, error) {
	var result struct {
		Sessions []string `json:"sessions"`
	}
	if err := c.call("list", nil, &result); err != nil {
		return nil, err
	}
	sessions := make(map[string]bool, len(result.Sessions))
	for _, name := range result.Sessions {
		sessions[name] = true
	}
	return sessions, nil
}

func (c *Client) ListCreated(ctx context.Context) (map[string]time.Time, error) {
	var result struct {
		Created map[string]int64 `json:"created"`
	}
	if err := c.call("listCreated", nil, &result); err != nil {
		return nil, err
	}
	created := make(map[string]time.Time, len(result.Created))
	for name, seconds := range result.Created {
		created[name] = time.Unix(seconds, 0)
	}
	return created, nil
}

func (c *Client) EnsurePipe(ctx context.Context, name string) error {
	return c.call("ensurePipe", map[string]any{"name": name}, nil)
}

func (c *Client) SpoolPath(name string) string {
	var result struct {
		Path string `json:"path"`
	}
	if err := c.call("spoolPath", map[string]any{"name": name}, &result); err != nil {
		return ""
	}
	return result.Path
}

func (c *Client) SpoolSize(ctx context.Context, name string) (int64, error) {
	var result struct {
		Size int64 `json:"size"`
	}
	if err := c.call("spoolSize", map[string]any{"name": name}, &result); err != nil {
		return 0, err
	}
	return result.Size, nil
}

func (c *Client) TruncateSpool(ctx context.Context, name string) error {
	return c.call("truncateSpool", map[string]any{"name": name}, nil)
}

func (c *Client) ArchiveSpool(ctx context.Context, name string) error {
	return c.call("archiveSpool", map[string]any{"name": name}, nil)
}

func (c *Client) RemoveSpool(name string) {
	_ = c.call("removeSpool", map[string]any{"name": name}, nil)
}

func (c *Client) Recover(ctx context.Context, name string, offset, end int64) ([]byte, error) {
	var result struct {
		Data string `json:"data"`
	}
	if err := c.call("recover", map[string]any{
		"name": name, "offset": offset, "end": end,
	}, &result); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(result.Data)
}

func (r *remoteSession) createdAtUnix() int64 {
	var result struct {
		Created int64 `json:"created"`
	}
	if err := r.client.call("createdAt", map[string]any{"name": r.name}, &result); err != nil {
		return 0
	}
	return result.Created
}
