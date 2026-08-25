package ghostline

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

const (
	maxRPCFrame        = 1 << 20
	maxRPCPayload      = 1 << 20
	maxRPCChunk        = 64 << 10
	maxRPCErrorMessage = 4 << 10
	rpcIdleTimeout     = time.Minute
	maxConnections     = 64
	wireVersion        = 1
)

// ProtocolVersion identifies the RPC protocol spoken by the server. Clients
// use it to detect an outdated server process during upgrades instead of
// failing on unknown methods.
const ProtocolVersion = "1.0.0"

const (
	// CapabilityRawPayload indicates that envelopes may be followed by an
	// exact-length unencoded payload.
	CapabilityRawPayload = "raw-payload-v1"
	// CapabilityStreams indicates support for the v1 pull-stream state machine.
	CapabilityStreams = "pull-stream-v1"
)

var protocolCapabilities = []string{CapabilityRawPayload, CapabilityStreams}

const (
	rpcMethodCreate       = "create"
	rpcMethodStatus       = "status"
	rpcMethodMetadata     = "metadata"
	rpcMethodSize         = "size"
	rpcMethodSignal       = "signal"
	rpcMethodCreated      = "created"
	rpcMethodVersion      = "version"
	rpcMethodWait         = "wait"
	rpcMethodTerminate    = "terminate"
	rpcMethodDelete       = "delete"
	rpcMethodWriteInput   = "writeInput"
	rpcMethodResize       = "resize"
	rpcMethodReplay       = "replay"
	rpcMethodCheckpoint   = "checkpoint"
	rpcMethodOutput       = "output"
	rpcMethodOutputCursor = "output.cursor"
	rpcMethodOutputRead   = "output.read"
	rpcMethodOutputClose  = "output.close"
	rpcMethodBlobRead     = "blob.read"
	rpcMethodBlobClose    = "blob.close"
	rpcMethodRotateOutput = "output.rotate"
	rpcMethodPruneOutput  = "output.prune"
	rpcMethodList         = "list"
)

type nameParams struct {
	Name string `json:"name"`
}

type createParams struct {
	Name                 string   `json:"name"`
	Dir                  string   `json:"dir"`
	Path                 string   `json:"path,omitempty"`
	Args                 []string `json:"args,omitempty"`
	ShellCommand         string   `json:"shellCommand,omitempty"`
	Cols                 int      `json:"cols"`
	Rows                 int      `json:"rows"`
	Env                  []string `json:"env"`
	VTScrollbackMaxBytes uint64   `json:"vtScrollbackMaxBytes,omitempty"`
}

type inputParams struct {
	Name string `json:"name"`
}

type resizeParams struct {
	Name string `json:"name"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

type signalParams struct {
	Name   string `json:"name"`
	Signal int    `json:"signal"`
}

type outputParams struct {
	Name   string `json:"name"`
	Cursor Cursor `json:"cursor"`
}

type chunkReadParams struct {
	MaxBytes int `json:"maxBytes"`
}

type outputReadResult struct {
	Cursor Cursor `json:"cursor"`
	EOF    bool   `json:"eof,omitempty"`
}

type blobOpenResult struct {
	Size   int    `json:"size"`
	Cursor Cursor `json:"cursor,omitempty"`
}

type blobReadResult struct {
	EOF bool `json:"eof,omitempty"`
}

type pruneOutputParams struct {
	Name   string `json:"name"`
	Before Cursor `json:"before"`
}

type cursorResult struct {
	Cursor Cursor `json:"cursor"`
}

type sizeResult struct {
	Columns int `json:"columns"`
	Rows    int `json:"rows"`
}

type createResult struct {
	Info SessionInfo `json:"info"`
}

type versionResult struct {
	Version      string         `json:"version"`
	TagVersion   string         `json:"tagVersion,omitempty"`
	Capabilities []string       `json:"capabilities"`
	Limits       ProtocolLimits `json:"limits"`
}

type listResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

type metadataResult struct {
	Process   string `json:"process"`
	Directory string `json:"directory"`
}

type request struct {
	Wire         int             `json:"wire"`
	ID           int64           `json:"id"`
	Method       string          `json:"method"`
	Params       json.RawMessage `json:"params,omitempty"`
	PayloadBytes int             `json:"payloadBytes,omitempty"`
	payload      []byte
}

type response struct {
	Wire         int             `json:"wire"`
	ID           int64           `json:"id"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        *rpcError       `json:"error,omitempty"`
	PayloadBytes int             `json:"payloadBytes,omitempty"`
}

// ProtocolLimits are the framing limits advertised by VersionInfo.
type ProtocolLimits struct {
	MaxHeaderBytes  int `json:"maxHeaderBytes"`
	MaxPayloadBytes int `json:"maxPayloadBytes"`
	MaxChunkBytes   int `json:"maxChunkBytes"`
}

var currentProtocolLimits = ProtocolLimits{
	MaxHeaderBytes: maxRPCFrame, MaxPayloadBytes: maxRPCPayload, MaxChunkBytes: maxRPCChunk,
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func rpcCode(err error) string {
	switch {
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	case errors.Is(err, ErrClosed):
		return "closed"
	case errors.Is(err, ErrSessionExists):
		return "session_exists"
	case errors.Is(err, ErrSessionNotFound):
		return "session_not_found"
	case errors.Is(err, ErrSessionClosed):
		return "session_closed"
	case errors.Is(err, ErrInvalidSessionName):
		return "invalid_name"
	case errors.Is(err, ErrInvalidSignal):
		return "invalid_signal"
	case errors.Is(err, os.ErrProcessDone):
		return "process_done"
	case errors.Is(err, ErrInvalidCursor):
		return "invalid_cursor"
	case errors.Is(err, ErrCursorExpired):
		return "cursor_expired"
	case errors.Is(err, ErrFrameTooLarge):
		return "frame_too_large"
	case errors.Is(err, ErrProtocolMismatch):
		return "protocol_mismatch"
	default:
		return "internal"
	}
}

func decodeRPCError(rpcErr *rpcError) error {
	if rpcErr == nil {
		return nil
	}
	var sentinel error
	switch rpcErr.Code {
	case "unavailable":
		sentinel = ErrUnavailable
	case "closed":
		sentinel = ErrClosed
	case "session_exists":
		sentinel = ErrSessionExists
	case "session_not_found":
		sentinel = ErrSessionNotFound
	case "session_closed":
		sentinel = ErrSessionClosed
	case "invalid_name":
		sentinel = ErrInvalidSessionName
	case "invalid_signal":
		sentinel = ErrInvalidSignal
	case "process_done":
		sentinel = os.ErrProcessDone
	case "invalid_cursor":
		sentinel = ErrInvalidCursor
	case "cursor_expired":
		sentinel = ErrCursorExpired
	case "frame_too_large":
		sentinel = ErrFrameTooLarge
	case "protocol_mismatch":
		sentinel = ErrProtocolMismatch
	}
	if sentinel != nil {
		return fmt.Errorf("%w: %s", sentinel, rpcErr.Message)
	}
	return errors.New(rpcErr.Message)
}

func writeResponse(writer *bufio.Writer, id int64, result any, err error) error {
	encoded, marshalErr := encodeResponse(id, result, 0, err)
	if marshalErr != nil {
		return marshalErr
	}
	if _, err := writer.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return writer.Flush()
}

// writeRawResponse writes one bounded JSON metadata line followed immediately
// by the exact raw payload length declared in result. It keeps control frames
// inspectable without base64-expanding terminal data or allocating a second
// encoded copy of every chunk.
func writeRawResponse(writer *bufio.Writer, id int64, result any, data []byte, err error) error {
	if len(data) > maxRPCChunk {
		return ErrFrameTooLarge
	}
	if err != nil {
		data = nil
	}
	encoded, marshalErr := encodeResponse(id, result, len(data), err)
	if marshalErr != nil {
		return marshalErr
	}
	if _, err := writer.Write(append(encoded, '\n')); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := writer.Write(data); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func encodeResponse(id int64, result any, payloadBytes int, err error) ([]byte, error) {
	value := response{Wire: wireVersion, ID: id, PayloadBytes: payloadBytes}
	if err != nil {
		value.Error = newRPCError(err)
	} else if result != nil {
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			value.Error = &rpcError{Code: "internal", Message: marshalErr.Error()}
		} else {
			value.Result = encoded
		}
	}
	encoded, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return nil, marshalErr
	}
	if len(encoded)+1 > maxRPCFrame {
		value = response{Wire: wireVersion, ID: id, Error: newRPCError(ErrFrameTooLarge)}
		encoded, marshalErr = json.Marshal(value)
		if marshalErr != nil {
			return nil, marshalErr
		}
	}
	return encoded, nil
}

func readRequest(reader *bufio.Reader) (request, error) {
	line, err := readLine(reader, maxRPCFrame)
	if err != nil {
		return request{}, err
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return request{}, fmt.Errorf("invalid request: %w", err)
	}
	if req.Wire != wireVersion {
		return request{}, fmt.Errorf("%w: wire %d", ErrProtocolMismatch, req.Wire)
	}
	if req.ID <= 0 || req.Method == "" {
		return request{}, errors.New("invalid request envelope")
	}
	if req.PayloadBytes < 0 || req.PayloadBytes > maxRPCPayload {
		return request{}, ErrFrameTooLarge
	}
	if req.PayloadBytes > 0 {
		req.payload = make([]byte, req.PayloadBytes)
		if _, err := io.ReadFull(reader, req.payload); err != nil {
			return request{}, fmt.Errorf("read request payload: %w", err)
		}
	}
	return req, nil
}

func newRPCError(err error) *rpcError {
	message := err.Error()
	if len(message) > maxRPCErrorMessage {
		message = message[:maxRPCErrorMessage]
	}
	return &rpcError{Code: rpcCode(err), Message: message}
}

func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	line := make([]byte, 0, 256)
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > limit {
			return nil, ErrFrameTooLarge
		}
		if err == nil {
			return line, nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return nil, err
	}
}

func decode[T any](raw json.RawMessage) (T, error) {
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, err
	}
	return value, nil
}

func socketReady(socketPath string) bool {
	connection, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if err != nil {
		return false
	}
	closeQuietly(connection)
	return true
}
