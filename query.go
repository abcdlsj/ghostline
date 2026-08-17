package ghostline

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// ColorQueryKind identifies the terminal color requested by an OSC query.
type ColorQueryKind uint8

const (
	// ColorQueryForeground is the default text color (OSC 10).
	ColorQueryForeground ColorQueryKind = 10
	// ColorQueryBackground is the default background color (OSC 11).
	ColorQueryBackground ColorQueryKind = 11
)

// ColorQueryCallback supplies a color for an OSC 10 or OSC 11 query.
//
// The callback should return a six-digit RGB value with an optional leading
// '#'. It returns false when the requested color is not available. A callback
// is optional; without one, unknown colors receive no reply.
type ColorQueryCallback func(ColorQueryKind) (color string, ok bool)

// QueryResponder answers terminal capability queries while a session has no
// attached terminal client. TUIs such as Codex send DA/DSR/OSC/kitty
// keyboard queries at startup. A raw PTY has nobody to answer until a client
// attaches, so the application may downgrade itself (for example disabling
// colors). Replies are written back into the PTY as input, never into output.
type QueryResponder struct {
	mu         sync.Mutex
	pending    []byte
	rows       int
	cols       int
	colorQuery ColorQueryCallback
}

// NewQueryResponder returns a responder initialized to a 120x36 terminal.
func NewQueryResponder() *QueryResponder {
	return NewQueryResponderWithColorQuery(nil)
}

// NewQueryResponderWithColorQuery returns a responder that uses callback to
// answer OSC 10 and OSC 11 color queries.
func NewQueryResponderWithColorQuery(callback ColorQueryCallback) *QueryResponder {
	return &QueryResponder{rows: 36, cols: 120, colorQuery: callback}
}

// Resize updates the window size reported in XTWINOPS replies.
func (r *QueryResponder) Resize(columns, rows int) {
	if columns <= 0 || rows <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cols = columns
	r.rows = rows
}

// Feed scans output bytes for complete terminal queries and returns the
// replies to write back into the PTY. Queries split across chunks are
// buffered until complete or until they prove not to be queries.
func (r *QueryResponder) Feed(data []byte) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, data...)
	var replies [][]byte
	for {
		index := bytes.IndexByte(r.pending, 0x1b)
		if index < 0 {
			r.pending = nil
			return replies
		}
		if index > 0 {
			r.pending = r.pending[index:]
		}
		if len(r.pending) < 2 {
			return replies
		}
		switch r.pending[1] {
		case '[':
			final, complete := csiFinal(r.pending[2:])
			if !complete {
				if final >= 0 {
					// A byte that cannot belong to a CSI sequence; drop the
					// leading escape and keep scanning the rest.
					r.pending = r.pending[1:]
					continue
				}
				if len(r.pending) > 256 {
					r.pending = nil
				}
				return replies
			}
			sequence := r.pending[2 : 2+final+1]
			r.pending = r.pending[2+final+1:]
			if reply := r.csiReply(sequence); len(reply) > 0 {
				replies = append(replies, reply)
			}
		case ']':
			end, complete := oscEnd(r.pending[2:])
			if !complete {
				if end >= 0 {
					r.pending = r.pending[1:]
					continue
				}
				if len(r.pending) > 4096 {
					r.pending = nil
				}
				return replies
			}
			sequence := r.pending[2 : 2+end]
			r.pending = r.pending[2+end:]
			if reply := r.oscReply(sequence); len(reply) > 0 {
				replies = append(replies, reply)
			}
		default:
			r.pending = r.pending[1:]
		}
	}
}

// csiFinal finds the final byte of a CSI sequence in body (the bytes after
// ESC [). A negative index with complete=false means the buffer is still
// incomplete; an index with complete=false means the byte at that index
// cannot be part of a CSI sequence.
func csiFinal(body []byte) (index int, complete bool) {
	for i := 0; i < len(body); i++ {
		if body[i] >= 0x40 && body[i] <= 0x7e {
			return i, true
		}
		if body[i] < 0x20 || body[i] == 0x7f {
			return i, false
		}
	}
	return -1, false
}

// oscEnd finds the end of an OSC sequence in body (the bytes after ESC ]).
// Returns the index of the terminating BEL or ST start; the terminator
// itself is not part of the returned range.
func oscEnd(body []byte) (index int, complete bool) {
	for i := 0; i < len(body); i++ {
		if body[i] == 0x07 {
			return i, true
		}
		if body[i] == 0x1b {
			if i+1 < len(body) && body[i+1] == '\\' {
				return i, true
			}
			return i, false
		}
		if body[i] < 0x20 {
			return i, false
		}
	}
	return -1, false
}

func (r *QueryResponder) csiReply(sequence []byte) []byte {
	final := sequence[len(sequence)-1]
	params := sequence[:len(sequence)-1]
	switch final {
	case 'c':
		// Primary DA. xterm-256color identifiers; the exact feature list is
		// less important than answering, otherwise TUIs assume a dumb
		// terminal and suppress colors.
		if len(params) > 0 && params[0] == '>' {
			return []byte("\x1b[>0;0;0c")
		}
		return []byte("\x1b[?62;c")
	case 'n':
		switch string(params) {
		case "5":
			return []byte("\x1b[0n")
		case "6":
			return []byte("\x1b[1;1R")
		}
	case 'u':
		// kitty keyboard protocol query: CSI ? u. Answering with the query
		// itself means "supported".
		if string(params) == "?" {
			return []byte("\x1b[?u")
		}
	case 'p':
		// DECRQM: CSI ? Ps $ p. Report the mode as set; this covers the
		// queries TUIs actually send (2004 bracketed paste, 2026
		// synchronized output, focus/mouse modes).
		if len(params) >= 2 && params[0] == '?' && params[len(params)-1] == '$' {
			mode := params[1 : len(params)-1]
			reply := make([]byte, 0, len(mode)+6)
			reply = append(reply, "\x1b[?"...)
			reply = append(reply, mode...)
			reply = append(reply, ";1$y"...)
			return reply
		}
	case 't':
		switch string(params) {
		case "14":
			return []byte(fmt.Sprintf("\x1b[4;%d;%dt", r.rows, r.cols))
		case "16":
			return []byte(fmt.Sprintf("\x1b[4;%d;%dt", r.rows*24, r.cols*8))
		case "18":
			return []byte(fmt.Sprintf("\x1b[8;%d;%dt", r.rows, r.cols))
		}
	}
	return nil
}

func (r *QueryResponder) oscReply(sequence []byte) []byte {
	if r.colorQuery == nil {
		return nil
	}
	var kind ColorQueryKind
	switch string(sequence) {
	case "10;?":
		kind = ColorQueryForeground
	case "11;?":
		kind = ColorQueryBackground
	default:
		return nil
	}
	color, ok := r.colorQuery(kind)
	if !ok {
		return nil
	}
	value, ok := formatOSCColor(color)
	if !ok {
		return nil
	}
	return []byte(fmt.Sprintf("\x1b]%d;%s\x1b\\", kind, value))
}

func formatOSCColor(color string) (string, bool) {
	value := strings.TrimSpace(color)
	if strings.HasPrefix(strings.ToLower(value), "rgb:") {
		components := strings.Split(value[4:], "/")
		if len(components) != 3 {
			return "", false
		}
		for _, component := range components {
			if len(component) == 0 || len(component) > 4 {
				return "", false
			}
			if _, err := strconv.ParseUint(component, 16, 16); err != nil {
				return "", false
			}
		}
		return "rgb:" + strings.ToLower(strings.Join(components, "/")), true
	}
	value = strings.TrimPrefix(value, "#")
	if len(value) == 3 {
		value = strings.Repeat(string(value[0]), 2) +
			strings.Repeat(string(value[1]), 2) +
			strings.Repeat(string(value[2]), 2)
	}
	if len(value) != 6 {
		return "", false
	}
	for _, component := range []string{value[0:2], value[2:4], value[4:6]} {
		if _, err := strconv.ParseUint(component, 16, 8); err != nil {
			return "", false
		}
	}
	return fmt.Sprintf("rgb:%[1]s%[1]s/%[2]s%[2]s/%[3]s%[3]s", strings.ToLower(value[0:2]), strings.ToLower(value[2:4]), strings.ToLower(value[4:6])), true
}
