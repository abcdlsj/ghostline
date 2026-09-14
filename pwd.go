package ghostline

import "strings"

// parseReportedPwd converts the raw pwd payload a program reported through the
// terminal (OSC 7 file URI, or the ConEmu/iTerm2 bare-path variants) into a
// local filesystem path.
//
// The terminal stores the payload verbatim, so this is the generic decoder for
// every shape the VT layer accepts. It returns an empty string when the value
// cannot be interpreted as a local path, which callers treat as "no report".
func parseReportedPwd(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "file://") {
		return fileURIPath(value)
	}
	return absolutePath(value)
}

// fileURIPath decodes a file:// URI that carries an absolute path. Hosts other
// than the empty host or localhost are rejected: a remote pwd does not
// describe the local filesystem.
func fileURIPath(uri string) string {
	rest := strings.TrimPrefix(uri, "file://")
	var path string
	switch {
	case strings.HasPrefix(rest, "/"):
		path = rest
	default:
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return ""
		}
		host := rest[:slash]
		if host != "" && !strings.EqualFold(host, "localhost") {
			return ""
		}
		path = rest[slash:]
	}
	decoded, ok := percentDecode(path)
	if !ok {
		return ""
	}
	return absolutePath(decoded)
}

// absolutePath trims the optional double quotes some shells wrap around the
// OSC 7 payload and requires the result to be an absolute path.
func absolutePath(value string) string {
	path := strings.Trim(value, `"`)
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	return path
}

// percentDecode expands %XX escapes. A malformed or truncated escape makes the
// whole URI unusable rather than silently dropping bytes.
func percentDecode(input string) (string, bool) {
	if !strings.Contains(input, "%") {
		return input, true
	}
	var decoded strings.Builder
	decoded.Grow(len(input))
	for index := 0; index < len(input); index++ {
		if input[index] != '%' {
			decoded.WriteByte(input[index])
			continue
		}
		if index+2 >= len(input) {
			return "", false
		}
		high, highOK := hexValue(input[index+1])
		low, lowOK := hexValue(input[index+2])
		if !highOK || !lowOK {
			return "", false
		}
		decoded.WriteByte(high<<4 | low)
		index += 2
	}
	return decoded.String(), true
}

func hexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}
