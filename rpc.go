package ghostline

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

const (
	maxRPCLine     = 1 << 20
	rpcIdleTimeout = time.Minute
	maxConnections = 64
)

// ProtocolVersion identifies the RPC protocol spoken by the server. Clients
// use it to detect an outdated server process during upgrades instead of
// failing on unknown methods.
const ProtocolVersion = "0.3.6"

const (
	rpcMethodCreate        = "create"
	rpcMethodStatus        = "status"
	rpcMethodCreated       = "created"
	rpcMethodVersion       = "version"
	rpcMethodWait          = "wait"
	rpcMethodClose         = "close"
	rpcMethodRemove        = "remove"
	rpcMethodInput         = "input"
	rpcMethodResize        = "resize"
	rpcMethodSnapshot      = "snapshot"
	rpcMethodCheckpoint    = "checkpoint"
	rpcMethodRecover       = "recover"
	rpcMethodSpoolPath     = "spoolPath"
	rpcMethodSpoolSize     = "spoolSize"
	rpcMethodTruncateSpool = "truncateSpool"
	rpcMethodArchiveSpool  = "archiveSpool"
	rpcMethodRemoveSpool   = "removeSpool"
	rpcMethodList          = "list"
)

type nameParams struct {
	Name string `json:"name"`
}

type createParams struct {
	Name    string   `json:"name"`
	Dir     string   `json:"dir"`
	Command string   `json:"command"`
	Cols    int      `json:"cols"`
	Rows    int      `json:"rows"`
	Env     []string `json:"env"`
}

type inputParams struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

type resizeParams struct {
	Name string `json:"name"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

type recoverParams struct {
	Name   string `json:"name"`
	Offset int64  `json:"offset"`
	End    int64  `json:"end"`
}

type createResult struct {
	Created int64 `json:"created"`
}

type versionResult struct {
	Version string `json:"version"`
}

type listResult struct {
	Sessions []string `json:"sessions"`
}

type dataResult struct {
	Data []byte `json:"data"`
}

type checkpointResult struct {
	Replay []byte `json:"replay"`
	Offset int64  `json:"offset"`
}

type spoolPathResult struct {
	Path string `json:"path"`
}

type spoolSizeResult struct {
	Size int64 `json:"size"`
}

type removeResult struct {
	Exit *ExitError `json:"exit"`
}

type request struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
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
	}
	if sentinel != nil {
		return fmt.Errorf("%w: %s", sentinel, rpcErr.Message)
	}
	return errors.New(rpcErr.Message)
}

func writeResponse(writer *bufio.Writer, id int64, result any, err error) error {
	value := response{ID: id}
	if err != nil {
		value.Error = &rpcError{Code: rpcCode(err), Message: err.Error()}
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
		return marshalErr
	}
	if _, err := writer.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return writer.Flush()
}

func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	line := make([]byte, 0, 256)
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > limit {
			return nil, errors.New("rpc message too large")
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

// Ping reports whether a ghostline server is accepting connections on
// socketPath.
func Ping(socketPath string) bool {
	connection, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if err != nil {
		return false
	}
	closeQuietly(connection)
	return true
}
