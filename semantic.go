package ghostline

import "bytes"

// osc133Prefix introduces an OSC 133 semantic prompt marker in the PTY output
// stream. libghostty-vt parses these markers and exposes the resulting cursor,
// row, and cell semantics through its public C API, but that API does not
// expose the OSC 133;D exit status payload. ghostline therefore scans the raw
// VT bytes for the completion marker. If a future vendored libghostty-vt
// exposes the payload, this scan can be deleted; the vendored archive and
// headers in third_party are intentionally left untouched.
//
// The hot-path search uses osc133Suffix rather than the full prefix: the ESC
// byte is extremely common in terminal output, while "]133;" is not. Searching
// the rare fragment keeps the per-chunk scan from being measurable against the
// VT write itself.
var (
	osc133Prefix = []byte("\x1b]133;")
	osc133Suffix = []byte("]133;")
)

// maxSemanticExitDigits bounds the OSC 133;D payload before the scan gives up.
// Exit statuses are small integers; a longer run of digits is treated as
// unknown rather than accumulated without bound.
const maxSemanticExitDigits = 6

// osc133Span is one complete OSC 133 marker located in a PTY output chunk. The
// span records where the marker ends so the write path can feed the bytes up
// to the semantic state change before it acts on it.
type osc133Span struct {
	// kind is the marker letter: 'A' (prompt start), 'B' (input start),
	// 'C' (command starts), or 'D' (command finished). Other OSC 133 letters
	// are preserved but carry no ghostline meaning.
	kind byte
	// exitCode and hasExit are the parsed OSC 133;D payload. hasExit is false
	// when D carried no integer status.
	exitCode int
	hasExit  bool
	// end is the index just past the marker terminator.
	end int
}

// findOSC133 scans data for complete OSC 133 markers. It returns nil when the
// chunk contains no marker, so the common output path allocates nothing. A
// marker whose terminator is not present in the chunk is not reported; the
// bytes are still fed to the VT parser, but the semantic state change is not
// acted on for that chunk. Shells emit a marker in a single write, so a split
// marker is a rare read-boundary artifact and the safe result is "unknown",
// never a guessed value.
func findOSC133(data []byte) []osc133Span {
	if bytes.Index(data, osc133Suffix) < 0 {
		return nil
	}
	var spans []osc133Span
	for position := 0; position < len(data); {
		index := bytes.Index(data[position:], osc133Suffix)
		if index < 0 {
			return spans
		}
		index += position
		start := index - 1
		if start < 0 || data[start] != 0x1b {
			position = index + 1
			continue
		}
		span, ok := parseOSC133At(data, start)
		if span.end <= position {
			// Defensive progress guarantee.
			position = index + len(osc133Suffix)
			continue
		}
		position = span.end
		if ok {
			spans = append(spans, span)
		}
	}
	return spans
}

// parseOSC133At parses the marker that starts at start. ok reports whether a
// complete OSC string terminator was found. end is always advanced past at
// least the prefix so callers make progress even for malformed input.
func parseOSC133At(data []byte, start int) (osc133Span, bool) {
	span := osc133Span{end: len(data)}
	body := start + len(osc133Prefix)
	if body >= len(data) {
		return span, false
	}
	span.kind = data[body]
	for index := body + 1; index < len(data); index++ {
		switch data[index] {
		case 0x07: // BEL terminates the OSC string.
			if span.kind == 'D' {
				span.exitCode, span.hasExit = parseOSC133Exit(data[body+1 : index])
			}
			span.end = index + 1
			return span, true
		case 0x1b: // ST is ESC followed by a backslash.
			if index+1 < len(data) && data[index+1] == '\\' {
				if span.kind == 'D' {
					span.exitCode, span.hasExit = parseOSC133Exit(data[body+1 : index])
				}
				span.end = index + 2
				return span, true
			}
		}
	}
	return span, false
}

// parseOSC133Exit parses the ";<n>" payload that follows an OSC 133;D marker.
// Anything that is not a bounded run of decimal digits is reported as unknown.
func parseOSC133Exit(payload []byte) (int, bool) {
	if len(payload) == 0 || payload[0] != ';' {
		return 0, false
	}
	value := 0
	digits := 0
	for _, digit := range payload[1:] {
		if digit == ';' {
			break
		}
		if digit < '0' || digit > '9' {
			return 0, false
		}
		if digits >= maxSemanticExitDigits {
			return 0, false
		}
		value = value*10 + int(digit-'0')
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	return value, true
}
