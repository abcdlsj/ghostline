package ghostline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRequestRejectsOversizedFrame(t *testing.T) {
	params := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, maxRPCFrame)...)
	params = append(params, '"')
	err := writeRequest(bufio.NewWriter(&bytes.Buffer{}), request{
		ID: 1, Method: rpcMethodWriteInput, Params: json.RawMessage(params),
	})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("writeRequest = %v, want ErrFrameTooLarge", err)
	}
}

func TestRawRequestEnvelopeRoundTripAndUnknownFields(t *testing.T) {
	payload := []byte("raw-input")
	var wire bytes.Buffer
	if err := writeRequestPayload(bufio.NewWriter(&wire), request{
		ID: 7, Method: rpcMethodWriteInput, Params: json.RawMessage(`{"name":"session"}`),
	}, payload); err != nil {
		t.Fatalf("writeRequestPayload: %v", err)
	}
	req, err := readRequest(bufio.NewReader(&wire))
	if err != nil {
		t.Fatalf("readRequest: %v", err)
	}
	if req.Wire != wireVersion || req.ID != 7 || req.Method != rpcMethodWriteInput || !bytes.Equal(req.payload, payload) {
		t.Fatalf("request = %+v, payload %q", req, req.payload)
	}

	unknown := bytes.NewBufferString(`{"wire":1,"id":8,"method":"future","payloadBytes":3,"futureField":true}` + "\nraw")
	req, err = readRequest(bufio.NewReader(unknown))
	if err != nil || string(req.payload) != "raw" {
		t.Fatalf("request with unknown field = (%+v, %v)", req, err)
	}
}

func TestRequestEnvelopeRejectsVersionAndPayloadLength(t *testing.T) {
	for _, test := range []struct {
		name string
		wire string
		want error
	}{
		{name: "wire", wire: `{"wire":2,"id":1,"method":"version"}` + "\n", want: ErrProtocolMismatch},
		{name: "oversized", wire: `{"wire":1,"id":1,"method":"version","payloadBytes":1048577}` + "\n", want: ErrFrameTooLarge},
		{name: "short", wire: `{"wire":1,"id":1,"method":"version","payloadBytes":4}` + "\nab", want: io.ErrUnexpectedEOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := readRequest(bufio.NewReader(bytes.NewBufferString(test.wire)))
			if !errors.Is(err, test.want) {
				t.Fatalf("readRequest = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRawResponseEnvelopeRoundTrip(t *testing.T) {
	payload := []byte("raw-output")
	var wire bytes.Buffer
	if err := writeRawResponse(bufio.NewWriter(&wire), 9, cursorResult{}, payload, nil); err != nil {
		t.Fatalf("writeRawResponse: %v", err)
	}
	reader := bufio.NewReader(&wire)
	var resp response
	if err := readResponse(reader, &resp); err != nil {
		t.Fatalf("readResponse: %v", err)
	}
	if resp.ID != 9 || resp.PayloadBytes != len(payload) {
		t.Fatalf("response = %+v", resp)
	}
	got := make([]byte, resp.PayloadBytes)
	if _, err := io.ReadFull(reader, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("payload = (%q, %v), want %q", got, err, payload)
	}
}

func TestResponseEnvelopeRejectsUnsupportedWire(t *testing.T) {
	var resp response
	err := readResponse(bufio.NewReader(bytes.NewBufferString(`{"wire":2,"id":1}`+"\n")), &resp)
	if !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("readResponse = %v, want ErrProtocolMismatch", err)
	}
}

func TestResponseEnvelopeRejectsInvalidErrorPayloadCombinations(t *testing.T) {
	for _, wire := range []string{
		`{"wire":1,"id":1,"error":{"code":"internal","message":"failed"},"result":{}}` + "\n",
		`{"wire":1,"id":1,"error":{"code":"internal","message":"failed"},"payloadBytes":1}` + "\nx",
	} {
		var resp response
		if err := readResponse(bufio.NewReader(bytes.NewBufferString(wire)), &resp); err == nil {
			t.Fatalf("readResponse(%q) succeeded", wire)
		}
	}
}

func TestResponseEnvelopeRejectsPayloadLength(t *testing.T) {
	var resp response
	err := readResponse(bufio.NewReader(bytes.NewBufferString(
		`{"wire":1,"id":1,"payloadBytes":1048577}`+"\n",
	)), &resp)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("readResponse = %v, want ErrFrameTooLarge", err)
	}
}

func TestRawResponsePayloadRejectsShortRead(t *testing.T) {
	reader := bufio.NewReader(bytes.NewBufferString(
		`{"wire":1,"id":1,"payloadBytes":4}` + "\nab",
	))
	var resp response
	if err := readResponse(reader, &resp); err != nil {
		t.Fatalf("readResponse: %v", err)
	}
	payload := make([]byte, resp.PayloadBytes)
	if _, err := io.ReadFull(reader, payload); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("payload read = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestClientRejectsMismatchedResponseID(t *testing.T) {
	socket := filepath.Join("/tmp", "ghostline-rpc-test.sock")
	_ = os.Remove(socket)
	t.Cleanup(func() { _ = os.Remove(socket) })
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		writer := bufio.NewWriter(connection)
		request, readErr := readRequest(reader)
		if readErr != nil {
			return
		}
		_ = writeResponse(writer, request.ID+1, versionResult{Version: ProtocolVersion}, nil)
	}()

	_, err = NewClient(socket).Version(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mismatched RPC response ID") {
		t.Fatalf("Version error = %v, want mismatched response ID", err)
	}
	listener.Close()
	<-done
}

func TestRPCErrorPreservesControlSentinels(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{err: ErrInvalidSignal, code: "invalid_signal"},
		{err: os.ErrProcessDone, code: "process_done"},
		{err: ErrProtocolMismatch, code: "protocol_mismatch"},
	} {
		wire := newRPCError(test.err)
		if wire.Code != test.code {
			t.Fatalf("rpc code for %v = %q, want %q", test.err, wire.Code, test.code)
		}
		if err := decodeRPCError(wire); !errors.Is(err, test.err) {
			t.Fatalf("decoded %q = %v, want %v", test.code, err, test.err)
		}
	}
}

func TestReadLineRejectsOversizedFrame(t *testing.T) {
	input := append(bytes.Repeat([]byte{'x'}, maxRPCFrame+1), '\n')
	_, err := readLine(bufio.NewReader(bytes.NewReader(input)), maxRPCFrame)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("readLine = %v, want ErrFrameTooLarge", err)
	}
}

func TestWriteResponseReplacesOversizedResultWithSentinel(t *testing.T) {
	var wire bytes.Buffer
	if err := writeResponse(bufio.NewWriter(&wire), 7, struct {
		Data []byte `json:"data"`
	}{Data: bytes.Repeat([]byte{'x'}, maxRPCFrame)}, nil); err != nil {
		t.Fatalf("writeResponse: %v", err)
	}
	var resp response
	if err := readResponse(bufio.NewReader(&wire), &resp); err != nil {
		t.Fatalf("readResponse: %v", err)
	}
	if resp.ID != 7 {
		t.Fatalf("response ID = %d, want 7", resp.ID)
	}
	if err := decodeRPCError(resp.Error); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("response error = %v, want ErrFrameTooLarge", err)
	}
}

func TestWriteResponseBoundsErrorMessage(t *testing.T) {
	var wire bytes.Buffer
	errText := string(bytes.Repeat([]byte{'x'}, maxRPCFrame))
	if err := writeResponse(bufio.NewWriter(&wire), 1, nil, errors.New(errText)); err != nil {
		t.Fatalf("writeResponse: %v", err)
	}
	if wire.Len() > maxRPCFrame {
		t.Fatalf("error response = %d bytes, limit %d", wire.Len(), maxRPCFrame)
	}
}

func TestDiagnosticBufferRetainsBoundedTail(t *testing.T) {
	var buffer diagnosticBuffer
	prefix := bytes.Repeat([]byte{'a'}, maxDaemonDiagnostics)
	suffix := bytes.Repeat([]byte{'b'}, 1024)
	if n, err := buffer.Write(prefix); err != nil || n != len(prefix) {
		t.Fatalf("write prefix = (%d, %v)", n, err)
	}
	if n, err := buffer.Write(suffix); err != nil || n != len(suffix) {
		t.Fatalf("write suffix = (%d, %v)", n, err)
	}
	got := buffer.String()
	if len(got) != maxDaemonDiagnostics {
		t.Fatalf("diagnostic buffer = %d bytes, want %d", len(got), maxDaemonDiagnostics)
	}
	if !bytes.HasSuffix([]byte(got), suffix) {
		t.Fatal("diagnostic buffer did not retain the newest output")
	}
}

func FuzzRPCResponseDecoding(f *testing.F) {
	f.Add([]byte(`{"wire":1,"id":1,"result":{"ok":true}}`))
	f.Add([]byte(`{"wire":1,"id":1,"error":{"code":"frame_too_large","message":"too large"}}`))
	f.Add([]byte(`not-json`))
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > maxRPCFrame {
			t.Skip()
		}
		wire := append(append([]byte(nil), input...), '\n')
		var resp response
		_ = readResponse(bufio.NewReader(bytes.NewReader(wire)), &resp)
	})
}

func FuzzRPCRequestEnvelope(f *testing.F) {
	f.Add([]byte(`{"wire":1,"id":1,"method":"version"}` + "\n"))
	f.Add([]byte(`{"wire":1,"id":1,"method":"writeInput","payloadBytes":3}` + "\nabc"))
	f.Add([]byte(`not-json` + "\n"))
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > maxRPCFrame+maxRPCPayload {
			t.Skip()
		}
		_, _ = readRequest(bufio.NewReader(bytes.NewReader(input)))
	})
}

func FuzzRPCRequestPayloadFraming(f *testing.F) {
	f.Add([]byte("hello"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > maxRPCPayload {
			t.Skip()
		}
		var wire bytes.Buffer
		if err := writeRequestPayload(bufio.NewWriter(&wire), request{ID: 1, Method: rpcMethodWriteInput}, payload); err != nil {
			t.Fatalf("writeRequestPayload: %v", err)
		}
		req, err := readRequest(bufio.NewReader(&wire))
		if err != nil {
			t.Fatalf("readRequest: %v", err)
		}
		if !bytes.Equal(req.payload, payload) {
			t.Fatalf("payload mismatch: got %d bytes, want %d", len(req.payload), len(payload))
		}
	})
}

func FuzzRPCRawResponsePayload(f *testing.F) {
	f.Add([]byte("output"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > maxRPCChunk {
			t.Skip()
		}
		var wire bytes.Buffer
		if err := writeRawResponse(bufio.NewWriter(&wire), 1, nil, payload, nil); err != nil {
			t.Fatalf("writeRawResponse: %v", err)
		}
		reader := bufio.NewReader(&wire)
		var resp response
		if err := readResponse(reader, &resp); err != nil {
			t.Fatalf("readResponse: %v", err)
		}
		got := make([]byte, resp.PayloadBytes)
		if _, err := io.ReadFull(reader, got); err != nil {
			t.Fatalf("read payload: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
		}
	})
}
