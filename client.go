package ghostline

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Client proxies Hub operations to a Server over a Unix socket.
type Client struct {
	socket string
}

// NewClient returns a client for the server at socketPath.
func NewClient(socketPath string) *Client {
	return &Client{socket: socketPath}
}

// Socket returns the server socket path.
func (c *Client) Socket() string { return c.socket }

// Check verifies that the server socket accepts connections.
func (c *Client) Check(ctx context.Context) error {
	connection, err := dial(ctx, c.socket)
	if err != nil {
		return fmt.Errorf("connect ghostline server: %w", err)
	}
	_ = connection.Close()
	return nil
}

func dial(ctx context.Context, socket string) (net.Conn, error) {
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		// Give ctx cancellation a chance to close the connection first so a
		// blocking call returns ctx.Err instead of a raw deadline error.
		_ = connection.SetDeadline(deadline.Add(100 * time.Millisecond))
	}
	return connection, nil
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	connection, err := dial(ctx, c.socket)
	if err != nil {
		return contextErr(ctx, fmt.Errorf("connect ghostline server: %w", err))
	}
	defer connection.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-done:
		}
	}()

	req := request{ID: 1, Method: method}
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return contextErr(ctx, err)
		}
		req.Params = encoded
	}
	writer := bufio.NewWriter(connection)
	if err := json.NewEncoder(writer).Encode(req); err != nil {
		return contextErr(ctx, err)
	}
	if err := writer.Flush(); err != nil {
		return contextErr(ctx, err)
	}
	var resp response
	if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&resp); err != nil {
		return contextErr(ctx, err)
	}
	if resp.Error != nil {
		return contextErr(ctx, decodeRPCError(resp.Error))
	}
	if result != nil && len(resp.Result) > 0 {
		return contextErr(ctx, json.Unmarshal(resp.Result, result))
	}
	return nil
}

func contextErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

type createResult struct {
	Created int64 `json:"created"`
}

// Start creates a session on the server and returns its remote handle.
func (c *Client) Start(ctx context.Context, options SessionOptions) (Session, error) {
	var result createResult
	err := c.call(ctx, "create", createParams{
		Name:    options.Name,
		Dir:     options.Directory,
		Command: options.Command,
		Cols:    options.Size.Columns,
		Rows:    options.Size.Rows,
		Env:     options.Environment,
	}, &result)
	if err != nil {
		return nil, err
	}
	return &remoteSession{
		client:    c,
		name:      options.Name,
		createdAt: time.Unix(result.Created, 0),
	}, nil
}

// List returns the names of all sessions known to the server.
func (c *Client) List(ctx context.Context) ([]string, error) {
	var result struct {
		Sessions []string `json:"sessions"`
	}
	if err := c.call(ctx, "list", nil, &result); err != nil {
		return nil, err
	}
	sort.Strings(result.Sessions)
	return result.Sessions, nil
}

func (c *Client) status(ctx context.Context, name string) (Status, error) {
	var result Status
	if err := c.call(ctx, "status", nameParams{Name: name}, &result); err != nil {
		return Status{}, err
	}
	return result, nil
}

func (c *Client) wait(ctx context.Context, name string) (Status, error) {
	var result Status
	if err := c.call(ctx, "wait", nameParams{Name: name}, &result); err != nil {
		return Status{}, err
	}
	return result, nil
}

// remoteSession is the RPC backend behind a remote Session handle.
type remoteSession struct {
	client    *Client
	name      string
	createdAt time.Time

	closed        atomic.Bool
	doneOnce      sync.Once
	doneCloseOnce sync.Once
	done          chan struct{}
	exit          atomic.Pointer[ExitError]
}

func (r *remoteSession) Name() string         { return r.name }
func (r *remoteSession) CreatedAt() time.Time { return r.createdAt }

func (r *remoteSession) Done() <-chan struct{} {
	r.doneOnce.Do(func() {
		r.done = make(chan struct{})
		go func() {
			for {
				status, err := r.client.wait(context.Background(), r.name)
				if err == nil {
					if status.Exit != nil {
						r.exit.Store(status.Exit)
					}
					r.closeDone()
					return
				}
				if errors.Is(err, ErrSessionNotFound) {
					r.closeDone()
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
	})
	return r.done
}

func (r *remoteSession) Wait(ctx context.Context) error {
	status, err := r.client.wait(ctx, r.name)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, ErrSessionNotFound) {
			if exit := r.exit.Load(); exit != nil {
				return exit
			}
			return ErrSessionClosed
		}
		return err
	}
	if status.Exit != nil {
		r.exit.Store(status.Exit)
		return status.Exit
	}
	return nil
}

func (r *remoteSession) Alive() bool {
	status, err := r.Status(context.Background())
	return err == nil && status.Alive
}

func (r *remoteSession) Status(ctx context.Context) (Status, error) {
	status, err := r.client.status(ctx, r.name)
	if err != nil {
		return Status{}, err
	}
	if status.Exit != nil {
		r.exit.Store(status.Exit)
	}
	return status, nil
}

func (r *remoteSession) Input(ctx context.Context, data []byte) error {
	return r.client.call(ctx, "input", inputParams{Name: r.name, Data: data}, nil)
}

func (r *remoteSession) Resize(ctx context.Context, size Size) error {
	return r.client.call(ctx, "resize", resizeParams{
		Name: r.name, Cols: size.Columns, Rows: size.Rows,
	}, nil)
}

func (r *remoteSession) Snapshot(ctx context.Context) ([]byte, error) {
	var result struct {
		Data []byte `json:"data"`
	}
	if err := r.client.call(ctx, "snapshot", nameParams{Name: r.name}, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (r *remoteSession) Checkpoint(ctx context.Context) (Checkpoint, error) {
	var result struct {
		Replay []byte `json:"replay"`
		Offset int64  `json:"offset"`
	}
	if err := r.client.call(ctx, "checkpoint", nameParams{Name: r.name}, &result); err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{Replay: result.Replay, Offset: result.Offset}, nil
}

func (r *remoteSession) Recover(ctx context.Context, offset, end int64) ([]byte, error) {
	var result struct {
		Data []byte `json:"data"`
	}
	if err := r.client.call(ctx, "recover", recoverParams{
		Name: r.name, Offset: offset, End: end,
	}, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (r *remoteSession) SpoolPath() string {
	var result struct {
		Path string `json:"path"`
	}
	if err := r.client.call(context.Background(), "spoolPath", nameParams{Name: r.name}, &result); err != nil {
		return ""
	}
	return result.Path
}

func (r *remoteSession) SpoolSize(ctx context.Context) (int64, error) {
	var result struct {
		Size int64 `json:"size"`
	}
	if err := r.client.call(ctx, "spoolSize", nameParams{Name: r.name}, &result); err != nil {
		return 0, err
	}
	return result.Size, nil
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
	if r.closed.Load() {
		return nil
	}
	return r.client.call(context.Background(), "close", nameParams{Name: r.name}, nil)
}

func (r *remoteSession) Remove() error {
	if r.closed.Load() {
		return nil
	}
	var result struct {
		Exit *ExitError `json:"exit"`
	}
	if err := r.client.call(context.Background(), "remove", nameParams{Name: r.name}, &result); err != nil {
		return err
	}
	if result.Exit != nil {
		r.exit.Store(result.Exit)
	}
	r.closed.Store(true)
	r.closeDone()
	return nil
}

func (r *remoteSession) closeDone() {
	r.doneOnce.Do(func() {
		if r.done == nil {
			r.done = make(chan struct{})
		}
	})
	r.doneCloseOnce.Do(func() {
		close(r.done)
	})
}

func (r *remoteSession) TruncateSpool(ctx context.Context) error {
	return r.client.call(ctx, "truncateSpool", nameParams{Name: r.name}, nil)
}

func (r *remoteSession) ArchiveSpool(ctx context.Context) error {
	return r.client.call(ctx, "archiveSpool", nameParams{Name: r.name}, nil)
}

func (r *remoteSession) RemoveSpool() {
	_ = r.client.call(context.Background(), "removeSpool", nameParams{Name: r.name}, nil)
}
