package ghostline

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
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

// ManagedClientOptions configures how ConnectManaged starts a missing server.
// Use NewClient when process lifecycle is owned by the caller or a service
// manager.
type ManagedClientOptions struct {
	// Socket is the Unix socket path the server listens on.
	Socket string
	// Spawn is the command used to start the server when the socket is
	// missing. Arguments may contain {socket}, replaced by Socket. Empty uses
	// ["ghostline", "serve", "--socket", socket].
	Spawn []string
	// Env overrides the spawned server's environment.
	Env []string
	// ReadyTimeout bounds how long ConnectManaged waits for the socket. Zero
	// uses 5s.
	ReadyTimeout time.Duration
	// Log receives serialized writes from the spawned server's stdout and
	// stderr. Empty discards them after retaining bounded diagnostics.
	Log io.Writer
}

type clientLifecycle struct {
	mu           sync.Mutex
	spawn        []string
	env          []string
	readyTimeout time.Duration
	log          io.Writer
	cmd          *exec.Cmd
	wait         chan error
}

type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *synchronizedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}

const maxDaemonDiagnostics = 64 << 10

// diagnosticBuffer retains only the most recent daemon startup diagnostics.
// A broken spawn command must not turn stderr into unbounded client memory.
type diagnosticBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *diagnosticBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(data)
	if len(data) >= maxDaemonDiagnostics {
		b.data = append(b.data[:0], data[len(data)-maxDaemonDiagnostics:]...)
		return written, nil
	}
	overflow := len(b.data) + len(data) - maxDaemonDiagnostics
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, data...)
	return written, nil
}

func (b *diagnosticBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// ConnectManaged returns a client, spawning the server when the socket is
// missing. The returned client owns the spawned process; Close stops it.
// This lifecycle behavior is intentionally separate from plain NewClient.
func ConnectManaged(ctx context.Context, options ManagedClientOptions) (*Client, error) {
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
	if socketReady(options.Socket) {
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
	if c.lifecycle == nil {
		return 0
	}
	c.lifecycle.mu.Lock()
	defer c.lifecycle.mu.Unlock()
	if c.lifecycle.cmd == nil || c.lifecycle.cmd.Process == nil {
		return 0
	}
	return c.lifecycle.cmd.Process.Pid
}

// Ensure starts the server if it is missing and waits until it is ready.
func (c *Client) Ensure(ctx context.Context) error {
	if socketReady(c.socket) {
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
	if c.lifecycle == nil {
		return nil
	}
	c.lifecycle.mu.Lock()
	defer c.lifecycle.mu.Unlock()
	if c.lifecycle.cmd == nil {
		return nil
	}
	process := c.lifecycle.cmd.Process
	if process == nil {
		return nil
	}
	signalErr := process.Signal(syscall.SIGTERM)
	if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
		return fmt.Errorf("stop managed ghostline server: %w", signalErr)
	}
	waitErr := <-c.lifecycle.wait
	c.lifecycle.cmd = nil
	c.lifecycle.wait = nil
	if signalErr == nil && exitedBySignal(waitErr, syscall.SIGTERM) {
		waitErr = nil
	}
	return errors.Join(signalErr, waitErr)
}

func exitedBySignal(err error, signal syscall.Signal) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == signal
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

// VersionInfo describes the protocol and release tag reported by a v1 server.
type VersionInfo struct {
	// ProtocolVersion is the server's RPC protocol identifier.
	ProtocolVersion string
	// TagVersion is the server module's release tag, or empty for a
	// development build or local replacement.
	TagVersion string
	// Capabilities contains stable feature names understood by the server.
	// Clients must ignore names they do not recognize.
	Capabilities []string
	// Limits contains the server's enforced wire framing limits.
	Limits ProtocolLimits
	// MaxClientConnections is the maximum number of active client sockets the
	// daemon accepts. Long-lived streams count against this limit.
	MaxClientConnections int
}

// VersionInfo returns the server's RPC protocol version and release tag.
func (c *Client) VersionInfo(ctx context.Context) (VersionInfo, error) {
	var result versionResult
	if err := c.call(ctx, rpcMethodVersion, nil, &result); err != nil {
		return VersionInfo{}, err
	}
	return VersionInfo{
		ProtocolVersion:      result.Version,
		TagVersion:           result.TagVersion,
		Capabilities:         append([]string(nil), result.Capabilities...),
		Limits:               result.Limits,
		MaxClientConnections: result.MaxClientConnections,
	}, nil
}

// Version returns the server's RPC protocol version. Use VersionInfo when the
// release tag is also needed.
func (c *Client) Version(ctx context.Context) (string, error) {
	info, err := c.VersionInfo(ctx)
	return info.ProtocolVersion, err
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
	if socketReady(c.socket) {
		return err
	}
	if spawnErr := c.spawnAndWait(ctx); spawnErr != nil {
		return fmt.Errorf("%v (recovery: %w)", err, spawnErr)
	}
	return c.callOnce(ctx, method, params, result)
}

func (c *Client) callOnce(ctx context.Context, method string, params, result any) error {
	return c.callOncePayload(ctx, method, params, nil, result)
}

func (c *Client) callOncePayload(ctx context.Context, method string, params any, payload []byte, result any) error {
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
	if err := writeRequestPayload(writer, req, payload); err != nil {
		return contextErr(ctx, err)
	}
	var resp response
	if err := readResponse(bufio.NewReader(connection), &resp); err != nil {
		return contextErr(ctx, err)
	}
	if resp.ID != req.ID {
		return contextErr(ctx, errors.New("ghostline: mismatched RPC response ID"))
	}
	if resp.Error != nil {
		return contextErr(ctx, decodeRPCError(resp.Error))
	}
	if resp.PayloadBytes != 0 {
		return contextErr(ctx, errors.New("ghostline: unexpected RPC response payload"))
	}
	if result != nil && len(resp.Result) > 0 {
		return contextErr(ctx, json.Unmarshal(resp.Result, result))
	}
	return nil
}

func writeRequest(writer *bufio.Writer, req request) error {
	return writeRequestPayload(writer, req, nil)
}

func writeRequestPayload(writer *bufio.Writer, req request, payload []byte) error {
	if len(payload) > maxRPCPayload {
		return ErrFrameTooLarge
	}
	req.Wire = wireVersion
	req.PayloadBytes = len(payload)
	encoded, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(encoded)+1 > maxRPCFrame {
		return ErrFrameTooLarge
	}
	if _, err := writer.Write(append(encoded, '\n')); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := writer.Write(payload); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func readResponse(reader *bufio.Reader, resp *response) error {
	line, err := readLine(reader, maxRPCFrame)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, resp); err != nil {
		return err
	}
	if resp.Wire != wireVersion {
		return fmt.Errorf("%w: wire %d", ErrProtocolMismatch, resp.Wire)
	}
	if resp.ID <= 0 && resp.ID != -1 {
		return errors.New("ghostline: invalid RPC response ID")
	}
	if resp.PayloadBytes < 0 || resp.PayloadBytes > maxRPCPayload {
		return ErrFrameTooLarge
	}
	if resp.Error != nil && (len(resp.Result) != 0 || resp.PayloadBytes != 0) {
		return errors.New("ghostline: invalid RPC error response")
	}
	return nil
}

func (c *Client) spawnAndWait(ctx context.Context) error {
	lifecycle := c.lifecycle
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if socketReady(c.socket) {
		return nil
	}
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
	var output diagnosticBuffer
	var sink io.Writer = &output
	if lifecycle.log != nil {
		sink = &synchronizedWriter{writer: io.MultiWriter(lifecycle.log, &output)}
	}
	command.Stdout = sink
	command.Stderr = sink
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

func (c *Client) waitReady(ctx context.Context, output *diagnosticBuffer, wait <-chan error) (error, bool) {
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
		if socketReady(c.socket) {
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

func (c *Client) readyAfterExit(waitErr error, output *diagnosticBuffer) (error, bool) {
	if socketReady(c.socket) {
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
func (c *Client) Start(ctx context.Context, options SessionOptions) (*Session, error) {
	var result createResult
	err := c.callRetryable(ctx, rpcMethodCreate, createParams{
		Name:                 options.Name,
		Dir:                  options.Process.Directory,
		Path:                 options.Process.Path,
		Args:                 options.Process.Args,
		ShellCommand:         options.Process.ShellCommand,
		Cols:                 options.Size.Columns,
		Rows:                 options.Size.Rows,
		Env:                  options.Process.Environment,
		VTScrollbackMaxBytes: options.VTScrollbackMaxBytes,
	}, &result)
	if err != nil {
		return nil, err
	}
	backend := &remoteSession{client: c, name: options.Name}
	return newSession(backend, result.Info), nil
}

// Get returns a daemon-owned session handle or ErrSessionNotFound.
func (c *Client) Get(ctx context.Context, name string) (*Session, error) {
	var result createResult
	if err := c.callRetryable(ctx, rpcMethodCreated, nameParams{Name: name}, &result); err != nil {
		return nil, err
	}
	backend := &remoteSession{client: c, name: name}
	return newSession(backend, result.Info), nil
}

// List returns daemon-owned sessions in the server's stable order.
func (c *Client) List(ctx context.Context) ([]*Session, error) {
	var listed listResult
	if err := c.callRetryable(ctx, rpcMethodList, nil, &listed); err != nil {
		return nil, err
	}
	result := make([]*Session, 0, len(listed.Sessions))
	for _, info := range listed.Sessions {
		backend := &remoteSession{client: c, name: info.Name}
		result = append(result, newSession(backend, info))
	}
	return result, nil
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
	client *Client
	name   string
}

func (r *remoteSession) call(ctx context.Context, method string, params, result any) error {
	return r.client.call(ctx, method, params, result)
}

func (r *remoteSession) callRetryable(ctx context.Context, method string, params, result any) error {
	return r.client.callRetryable(ctx, method, params, result)
}

func (r *remoteSession) callPayload(ctx context.Context, method string, params any, payload []byte, result any) error {
	return r.client.callOncePayload(ctx, method, params, payload, result)
}

func (r *remoteSession) wait(ctx context.Context) error {
	status, err := r.client.wait(ctx, r.name)
	if err != nil {
		return err
	}
	if status.Exit != nil {
		return status.Exit
	}
	return nil
}

func (r *remoteSession) status(ctx context.Context) (Status, error) {
	return r.client.status(ctx, r.name)
}

func (r *remoteSession) metadata(ctx context.Context) (SessionMetadata, error) {
	var result metadataResult
	if err := r.callRetryable(ctx, rpcMethodMetadata, nameParams{Name: r.name}, &result); err != nil {
		return SessionMetadata{}, err
	}
	return SessionMetadata{Process: result.Process, Directory: result.Directory}, nil
}

func (r *remoteSession) size(ctx context.Context) (Size, error) {
	var result sizeResult
	if err := r.callRetryable(ctx, rpcMethodSize, nameParams{Name: r.name}, &result); err != nil {
		return Size{}, err
	}
	return Size{Columns: result.Columns, Rows: result.Rows}, nil
}

func (r *remoteSession) signal(ctx context.Context, signal syscall.Signal) error {
	return r.call(ctx, rpcMethodSignal, signalParams{Name: r.name, Signal: int(signal)}, nil)
}

func (r *remoteSession) writeInput(ctx context.Context, data []byte) error {
	return r.callPayload(ctx, rpcMethodWriteInput, inputParams{Name: r.name}, data, nil)
}

func (r *remoteSession) resize(ctx context.Context, size Size) error {
	return r.call(ctx, rpcMethodResize, resizeParams{
		Name: r.name, Cols: size.Columns, Rows: size.Rows,
	}, nil)
}

func (r *remoteSession) replay(ctx context.Context) ([]byte, error) {
	data, _, err := r.client.readBlobRetryable(ctx, rpcMethodReplay, nameParams{Name: r.name})
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (r *remoteSession) checkpoint(ctx context.Context) (Checkpoint, error) {
	replay, cursor, err := r.client.readBlobRetryable(ctx, rpcMethodCheckpoint, nameParams{Name: r.name})
	if err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{Replay: replay, Cursor: cursor}, nil
}

func (c *Client) readBlobRetryable(ctx context.Context, method string, params any) ([]byte, Cursor, error) {
	data, cursor, err := c.readBlobOnce(ctx, method, params)
	if err == nil || c.lifecycle == nil || !isTransportError(err) {
		return data, cursor, err
	}
	if socketReady(c.socket) {
		return nil, Cursor{}, err
	}
	if spawnErr := c.spawnAndWait(ctx); spawnErr != nil {
		return nil, Cursor{}, fmt.Errorf("%v (recovery: %w)", err, spawnErr)
	}
	return c.readBlobOnce(ctx, method, params)
}

func (c *Client) readBlobOnce(ctx context.Context, method string, params any) ([]byte, Cursor, error) {
	connection, err := dial(ctx, c.socket)
	if err != nil {
		return nil, Cursor{}, contextErr(ctx, fmt.Errorf("connect ghostline server: %w", err))
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
		req.Params, err = json.Marshal(params)
		if err != nil {
			return nil, Cursor{}, err
		}
	}
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	if err := writeRequest(writer, req); err != nil {
		return nil, Cursor{}, contextErr(ctx, err)
	}
	var resp response
	if err := readResponse(reader, &resp); err != nil {
		return nil, Cursor{}, contextErr(ctx, err)
	}
	if resp.ID != req.ID {
		return nil, Cursor{}, errors.New("ghostline: mismatched blob-open response ID")
	}
	if resp.Error != nil {
		return nil, Cursor{}, contextErr(ctx, decodeRPCError(resp.Error))
	}
	if resp.PayloadBytes != 0 {
		return nil, Cursor{}, errors.New("ghostline: unexpected blob-open payload")
	}
	var opened blobOpenResult
	if err := json.Unmarshal(resp.Result, &opened); err != nil {
		return nil, Cursor{}, err
	}
	if opened.Size < 0 {
		return nil, Cursor{}, errors.New("ghostline: server returned invalid blob size")
	}
	capacity := opened.Size
	if capacity > maxRPCFrame {
		capacity = maxRPCFrame
	}
	data := make([]byte, 0, capacity)
	for {
		encoded, err := json.Marshal(chunkReadParams{MaxBytes: maxRPCChunk})
		if err != nil {
			return nil, Cursor{}, err
		}
		next := request{ID: 1, Method: rpcMethodBlobRead, Params: encoded}
		if err := writeRequest(writer, next); err != nil {
			return nil, Cursor{}, contextErr(ctx, err)
		}
		resp = response{}
		if err := readResponse(reader, &resp); err != nil {
			return nil, Cursor{}, contextErr(ctx, err)
		}
		if resp.ID != next.ID {
			return nil, Cursor{}, errors.New("ghostline: mismatched blob response ID")
		}
		if resp.Error != nil {
			return nil, Cursor{}, contextErr(ctx, decodeRPCError(resp.Error))
		}
		var chunk blobReadResult
		if err := json.Unmarshal(resp.Result, &chunk); err != nil {
			return nil, Cursor{}, err
		}
		chunkBytes := resp.PayloadBytes
		if chunkBytes > maxRPCChunk || len(data) > opened.Size-chunkBytes {
			return nil, Cursor{}, errors.New("ghostline: server returned invalid blob chunk")
		}
		start := len(data)
		data = append(data, make([]byte, chunkBytes)...)
		if _, err := io.ReadFull(reader, data[start:]); err != nil {
			return nil, Cursor{}, contextErr(ctx, err)
		}
		if chunk.EOF {
			if len(data) != opened.Size {
				return nil, Cursor{}, errors.New("ghostline: server returned incomplete blob")
			}
			return data, opened.Cursor, nil
		}
		if chunkBytes == 0 {
			return nil, Cursor{}, io.ErrNoProgress
		}
	}
}

func (r *remoteSession) output(ctx context.Context, from Cursor) (*OutputReader, error) {
	source, err := r.client.openOutput(ctx, r.name, from)
	if err != nil {
		return nil, err
	}
	return newOutputReader(source), nil
}

func (r *remoteSession) outputCursor(ctx context.Context) (Cursor, error) {
	var result cursorResult
	if err := r.callRetryable(ctx, rpcMethodOutputCursor, nameParams{Name: r.name}, &result); err != nil {
		return Cursor{}, err
	}
	return result.Cursor, nil
}

func (r *remoteSession) rotateOutput(ctx context.Context) (Cursor, error) {
	var result cursorResult
	if err := r.call(ctx, rpcMethodRotateOutput, nameParams{Name: r.name}, &result); err != nil {
		return Cursor{}, err
	}
	return result.Cursor, nil
}

func (r *remoteSession) pruneOutput(ctx context.Context, before Cursor) error {
	return r.call(ctx, rpcMethodPruneOutput, pruneOutputParams{Name: r.name, Before: before}, nil)
}

func (r *remoteSession) terminate(ctx context.Context) error {
	return r.call(ctx, rpcMethodTerminate, nameParams{Name: r.name}, nil)
}

func (r *remoteSession) delete(ctx context.Context) error {
	return r.call(ctx, rpcMethodDelete, nameParams{Name: r.name}, nil)
}

type remoteOutputSource struct {
	ctx       context.Context
	conn      net.Conn
	reader    *bufio.Reader
	writer    *bufio.Writer
	mu        sync.Mutex
	cursor    Cursor
	closed    atomic.Bool
	closeOnce sync.Once
	done      chan struct{}
}

func (c *Client) openOutput(ctx context.Context, name string, from Cursor) (*remoteOutputSource, error) {
	connection, err := dial(ctx, c.socket)
	if err != nil {
		return nil, contextErr(ctx, fmt.Errorf("connect ghostline server: %w", err))
	}
	source := &remoteOutputSource{
		ctx:    ctx,
		conn:   connection,
		reader: bufio.NewReader(connection),
		writer: bufio.NewWriter(connection),
		done:   make(chan struct{}),
	}
	req := request{ID: 1, Method: rpcMethodOutput}
	req.Params, err = json.Marshal(outputParams{Name: name, Cursor: from})
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	if err := writeRequest(source.writer, req); err != nil {
		_ = source.Close()
		return nil, contextErr(ctx, err)
	}
	var resp response
	if err := readResponse(source.reader, &resp); err != nil {
		_ = source.Close()
		return nil, contextErr(ctx, err)
	}
	if resp.ID != req.ID {
		_ = source.Close()
		return nil, errors.New("ghostline: mismatched output-open response ID")
	}
	if resp.Error != nil {
		_ = source.Close()
		return nil, decodeRPCError(resp.Error)
	}
	if resp.PayloadBytes != 0 {
		_ = source.Close()
		return nil, errors.New("ghostline: unexpected output-open payload")
	}
	var opened cursorResult
	if err := json.Unmarshal(resp.Result, &opened); err != nil {
		_ = source.Close()
		return nil, err
	}
	source.cursor = opened.Cursor
	go func() {
		select {
		case <-ctx.Done():
			_ = source.Close()
		case <-source.done:
		}
	}()
	return source, nil
}

func (r *remoteOutputSource) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if r.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	maxBytes := len(buffer)
	if maxBytes > maxRPCChunk {
		maxBytes = maxRPCChunk
	}
	params, err := json.Marshal(chunkReadParams{MaxBytes: maxBytes})
	if err != nil {
		return 0, err
	}
	next := request{ID: 1, Method: rpcMethodOutputRead, Params: params}
	if err := writeRequest(r.writer, next); err != nil {
		return 0, contextErr(r.ctx, err)
	}
	var resp response
	if err := readResponse(r.reader, &resp); err != nil {
		return 0, contextErr(r.ctx, err)
	}
	if resp.ID != next.ID {
		return 0, errors.New("ghostline: mismatched output response ID")
	}
	if resp.Error != nil {
		return 0, decodeRPCError(resp.Error)
	}
	var result outputReadResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return 0, err
	}
	chunkBytes := resp.PayloadBytes
	if chunkBytes > len(buffer) || chunkBytes > maxRPCChunk {
		return 0, errors.New("ghostline: server returned oversized output chunk")
	}
	if _, err := io.ReadFull(r.reader, buffer[:chunkBytes]); err != nil {
		return 0, contextErr(r.ctx, err)
	}
	r.cursor = result.Cursor
	if chunkBytes != 0 {
		return chunkBytes, nil
	}
	if result.EOF {
		return 0, io.EOF
	}
	return 0, io.ErrNoProgress
}

func (r *remoteOutputSource) Cursor() Cursor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursor
}

func (r *remoteOutputSource) Close() error {
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		close(r.done)
		closeQuietly(r.conn)
	})
	return nil
}
