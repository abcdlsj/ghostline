package ghostline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Client proxies Hub operations to a Server over a Unix socket.
type Client struct {
	socket    string
	lifecycle *clientLifecycle
}

// NewClient returns a client for the server at socketPath.
func NewClient(socketPath string) *Client {
	return &Client{socket: socketPath}
}

// ConnectOptions configures how Connect starts a missing server.
type ConnectOptions struct {
	// Socket is the Unix socket path the server listens on.
	Socket string
	// Spawn is the command used to start the server when the socket is
	// missing. Arguments may contain {socket}, replaced by Socket. Empty uses
	// ["ghostline", "serve", "--socket", socket].
	Spawn []string
	// Env overrides the spawned server's environment.
	Env []string
	// ReadyTimeout bounds how long Connect waits for the socket. Zero uses 5s.
	ReadyTimeout time.Duration
	// Log receives the spawned server's stdout and stderr. Empty discards it.
	Log io.Writer
}

type clientLifecycle struct {
	spawn        []string
	env          []string
	readyTimeout time.Duration
	log          io.Writer
	cmd          *exec.Cmd
	wait         chan error
}

// lockedBuffer is the small bridge between os/exec's output copier and the
// readiness loop. A process can write its first diagnostic at the same time
// that waitReady formats it, so the buffer needs the same quiet discipline as
// the socket it is describing.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Connect returns a client, spawning the server when the socket is missing.
// The returned client owns the spawned process; Close stops it.
func Connect(ctx context.Context, options ConnectOptions) (*Client, error) {
	if options.Socket == "" {
		return nil, errors.New("ghostline: socket path required")
	}
	client := &Client{
		socket: options.Socket,
		lifecycle: &clientLifecycle{
			spawn:        options.Spawn,
			env:          options.Env,
			readyTimeout: options.ReadyTimeout,
			log:          options.Log,
		},
	}
	if Ping(options.Socket) {
		return client, nil
	}
	if err := client.spawnAndWait(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

// Socket returns the server socket path.
func (c *Client) Socket() string { return c.socket }

// PID returns the process ID of the server spawned by this client, or zero
// when the client attached to an existing server.
func (c *Client) PID() int {
	if c.lifecycle == nil || c.lifecycle.cmd == nil || c.lifecycle.cmd.Process == nil {
		return 0
	}
	return c.lifecycle.cmd.Process.Pid
}

// Ensure starts the server if it is missing and waits until it is ready.
func (c *Client) Ensure(ctx context.Context) error {
	if Ping(c.socket) {
		return nil
	}
	if c.lifecycle == nil {
		return fmt.Errorf("server not running at %s", c.socket)
	}
	return c.spawnAndWait(ctx)
}

// Close stops the server that this client spawned. Clients that connected to
// an existing server have nothing to stop.
func (c *Client) Close() error {
	if c.lifecycle == nil || c.lifecycle.cmd == nil {
		return nil
	}
	process := c.lifecycle.cmd.Process
	if process == nil {
		return nil
	}
	_ = process.Signal(syscall.SIGTERM)
	err := <-c.lifecycle.wait
	c.lifecycle.cmd = nil
	return err
}

// Check verifies that the server socket accepts connections.
func (c *Client) Check(ctx context.Context) error {
	connection, err := dial(ctx, c.socket)
	if err != nil {
		return fmt.Errorf("connect ghostline server: %w", err)
	}
	closeQuietly(connection)
	return nil
}

// Version returns the server's RPC protocol version. An error or an empty
// value means the server predates protocol versioning; embedders use this to
// decide whether to roll the server (see RFC 0002) or keep serving from it.
func (c *Client) Version(ctx context.Context) (string, error) {
	var result versionResult
	if err := c.call(ctx, rpcMethodVersion, nil, &result); err != nil {
		return "", err
	}
	return result.Version, nil
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
	return c.callOnce(ctx, method, params, result)
}

func (c *Client) callRetryable(ctx context.Context, method string, params, result any) error {
	err := c.callOnce(ctx, method, params, result)
	if err == nil || c.lifecycle == nil || !isTransportError(err) {
		return err
	}
	if Ping(c.socket) {
		return err
	}
	if spawnErr := c.spawnAndWait(ctx); spawnErr != nil {
		return fmt.Errorf("%v (recovery: %w)", err, spawnErr)
	}
	return c.callOnce(ctx, method, params, result)
}

func (c *Client) callOnce(ctx context.Context, method string, params, result any) error {
	connection, err := dial(ctx, c.socket)
	if err != nil {
		return contextErr(ctx, fmt.Errorf("connect ghostline server: %w", err))
	}
	defer closeQuietly(connection)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			closeQuietly(connection)
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

func (c *Client) spawnAndWait(ctx context.Context) error {
	lifecycle := c.lifecycle
	spawn := lifecycle.spawn
	if len(spawn) == 0 {
		spawn = []string{"ghostline", "serve", "--socket", c.socket}
	}
	args := make([]string, len(spawn))
	for index, arg := range spawn {
		args[index] = strings.ReplaceAll(arg, "{socket}", c.socket)
	}
	if err := validateEnvironment(lifecycle.env); err != nil {
		return fmt.Errorf("invalid spawn environment: %w", err)
	}
	command := exec.Command(args[0], args[1:]...)
	command.Env = mergeEnvironment(os.Environ(), lifecycle.env)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var output lockedBuffer
	if lifecycle.log != nil {
		command.Stdout = io.MultiWriter(lifecycle.log, &output)
		command.Stderr = io.MultiWriter(lifecycle.log, &output)
	} else {
		command.Stdout = &output
		command.Stderr = &output
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("spawn ghostline server: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	lifecycle.cmd = command
	lifecycle.wait = wait
	err, exited := c.waitReady(ctx, &output, wait)
	if err != nil {
		if !exited {
			_ = command.Process.Kill()
			<-wait
		}
		lifecycle.cmd = nil
		lifecycle.wait = nil
		return err
	}
	if exited {
		// Another concurrent spawn bound the socket first. The live server
		// is not ours to stop, so this client must not own it.
		lifecycle.cmd = nil
		lifecycle.wait = nil
	}
	return nil
}

func (c *Client) waitReady(ctx context.Context, output *lockedBuffer, wait <-chan error) (error, bool) {
	timeout := c.lifecycle.readyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case waitErr := <-wait:
			return c.readyAfterExit(waitErr, output)
		default:
		}
		if Ping(c.socket) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return ctx.Err(), false
		case <-timer.C:
			return fmt.Errorf(
				"server did not become ready at %s: %s",
				c.socket,
				strings.TrimSpace(output.String()),
			), false
		case waitErr := <-wait:
			return c.readyAfterExit(waitErr, output)
		case <-ticker.C:
		}
	}
}

func (c *Client) readyAfterExit(waitErr error, output *lockedBuffer) (error, bool) {
	if Ping(c.socket) {
		return nil, true
	}
	return fmt.Errorf(
		"spawned ghostline server exited: %v: %s",
		waitErr,
		strings.TrimSpace(output.String()),
	), true
}

// isTransportError reports whether err came from the socket transport rather
// than the RPC layer. Only transport failures are safe to retry once after a
// respawn; server responses carry their own application errors.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE)
}

func contextErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// Start creates a session on the server and returns its remote handle.
func (c *Client) Start(ctx context.Context, options SessionOptions) (Session, error) {
	var result createResult
	err := c.callRetryable(ctx, rpcMethodCreate, createParams{
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
	var result listResult
	if err := c.callRetryable(ctx, rpcMethodList, nil, &result); err != nil {
		return nil, err
	}
	sort.Strings(result.Sessions)
	return result.Sessions, nil
}

// Session returns a handle for a session known to the server, mirroring
// Hub.Session. The handle is lazy; operations fail with the server's error if
// the session disappears.
func (c *Client) Session(name string) (Session, bool) {
	sessions, err := c.List(context.Background())
	if err != nil {
		return nil, false
	}
	for _, existing := range sessions {
		if existing == name {
			return &remoteSession{client: c, name: name}, true
		}
	}
	return nil, false
}

// Sessions returns handles for all sessions known to the server, ordered by
// name, mirroring Hub.Sessions.
func (c *Client) Sessions() []Session {
	sessions, err := c.List(context.Background())
	if err != nil {
		return nil
	}
	result := make([]Session, 0, len(sessions))
	for _, name := range sessions {
		result = append(result, &remoteSession{client: c, name: name})
	}
	return result
}

func (c *Client) status(ctx context.Context, name string) (Status, error) {
	var result Status
	if err := c.callRetryable(ctx, rpcMethodStatus, nameParams{Name: name}, &result); err != nil {
		return Status{}, err
	}
	return result, nil
}

func (c *Client) wait(ctx context.Context, name string) (Status, error) {
	var result Status
	if err := c.call(ctx, rpcMethodWait, nameParams{Name: name}, &result); err != nil {
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
	createdAtOnce sync.Once
}

func (r *remoteSession) Name() string { return r.name }

func (r *remoteSession) CreatedAt() time.Time {
	r.createdAtOnce.Do(func() {
		if !r.createdAt.IsZero() {
			return
		}
		var result createResult
		if err := r.call(context.Background(), rpcMethodCreated, nameParams{Name: r.name}, &result); err == nil {
			r.createdAt = time.Unix(result.Created, 0)
		}
	})
	return r.createdAt
}

func (r *remoteSession) call(ctx context.Context, method string, params, result any) error {
	return r.client.call(ctx, method, params, result)
}

func (r *remoteSession) callRetryable(ctx context.Context, method string, params, result any) error {
	return r.client.callRetryable(ctx, method, params, result)
}

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

func (r *remoteSession) Metadata(ctx context.Context) (SessionMetadata, error) {
	var result metadataResult
	if err := r.callRetryable(ctx, rpcMethodMetadata, nameParams{Name: r.name}, &result); err != nil {
		return SessionMetadata{}, err
	}
	return SessionMetadata{Process: result.Process, Directory: result.Directory}, nil
}

func (r *remoteSession) Input(ctx context.Context, data []byte) error {
	return r.call(ctx, rpcMethodInput, inputParams{Name: r.name, Data: data}, nil)
}

func (r *remoteSession) Resize(ctx context.Context, size Size) error {
	return r.call(ctx, rpcMethodResize, resizeParams{
		Name: r.name, Cols: size.Columns, Rows: size.Rows,
	}, nil)
}

func (r *remoteSession) Snapshot(ctx context.Context) ([]byte, error) {
	var result dataResult
	if err := r.callRetryable(ctx, rpcMethodSnapshot, nameParams{Name: r.name}, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (r *remoteSession) Checkpoint(ctx context.Context) (Checkpoint, error) {
	var result checkpointResult
	if err := r.callRetryable(ctx, rpcMethodCheckpoint, nameParams{Name: r.name}, &result); err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint(result), nil
}

func (r *remoteSession) Recover(ctx context.Context, offset, end int64) ([]byte, error) {
	var result dataResult
	if err := r.callRetryable(ctx, rpcMethodRecover, recoverParams{
		Name: r.name, Offset: offset, End: end,
	}, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (r *remoteSession) SpoolPath() string {
	var result spoolPathResult
	if err := r.callRetryable(context.Background(), rpcMethodSpoolPath, nameParams{Name: r.name}, &result); err != nil {
		return ""
	}
	return result.Path
}

func (r *remoteSession) SpoolSize(ctx context.Context) (int64, error) {
	var result spoolSizeResult
	if err := r.callRetryable(ctx, rpcMethodSpoolSize, nameParams{Name: r.name}, &result); err != nil {
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
	return r.call(context.Background(), rpcMethodClose, nameParams{Name: r.name}, nil)
}

func (r *remoteSession) Remove() error {
	if r.closed.Load() {
		return nil
	}
	var result removeResult
	if err := r.call(context.Background(), rpcMethodRemove, nameParams{Name: r.name}, &result); err != nil {
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
	return r.call(ctx, rpcMethodTruncateSpool, nameParams{Name: r.name}, nil)
}

func (r *remoteSession) ArchiveSpool(ctx context.Context) error {
	return r.call(ctx, rpcMethodArchiveSpool, nameParams{Name: r.name}, nil)
}

func (r *remoteSession) RemoveSpool() {
	_ = r.call(context.Background(), rpcMethodRemoveSpool, nameParams{Name: r.name}, nil)
}
