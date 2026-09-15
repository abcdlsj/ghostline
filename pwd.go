package ghostline

import (
	"os"
	"strings"
)

// parseReportedPwd converts the raw pwd payload a program reported through the
// terminal into a local filesystem path.
//
// The VT layer stores the payload verbatim, so this decodes every shape the
// ecosystem emits: a file:// URI, the Kitty/Ghostty kitty-shell-cwd:// URI, and
// the ConEmu/iTerm2 bare-path variants. It returns an empty string when the
// value cannot be interpreted as a local path, which callers treat as "no
// report".
func parseReportedPwd(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if _, rest, ok := splitReportedScheme(value); ok {
		return hostPath(rest, localHostname())
	}
	return absolutePath(value)
}

// splitReportedScheme splits "scheme://rest". A bare path has no scheme.
func splitReportedScheme(value string) (scheme, rest string, ok bool) {
	index := strings.Index(value, "://")
	if index <= 0 {
		return "", "", false
	}
	return value[:index], value[index+3:], true
}

// hostPath decodes the authority and path of a reported cwd URI. A host other
// than the empty host, localhost, or this machine is rejected: a remote cwd
// does not describe the local filesystem.
func hostPath(rest, localHost string) string {
	var path string
	if strings.HasPrefix(rest, "/") {
		path = rest
	} else {
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return ""
		}
		host := rest[:slash]
		if host != "" && !isLocalHost(host, localHost) {
			return ""
		}
		path = rest[slash:]
	}
	decoded, ok := percentDecode(path)
	if !ok {
		// A path may legitimately contain a percent sign. Keep the raw value
		// rather than dropping a valid working directory.
		decoded = path
	}
	return absolutePath(decoded)
}

// absolutePath trims the optional double quotes some shells wrap around the
// payload and requires the result to be an absolute path.
func absolutePath(value string) string {
	path := strings.Trim(value, `"`)
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	return path
}

func localHostname() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}

// isLocalHost accepts the loopback name and this machine's hostname in either
// its short or fully-qualified form.
func isLocalHost(host, local string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if local == "" {
		return false
	}
	host = strings.ToLower(host)
	local = strings.ToLower(local)
	if host == local {
		return true
	}
	return shortHostname(host) == shortHostname(local)
}

func shortHostname(value string) string {
	if index := strings.IndexByte(value, '.'); index > 0 {
		return value[:index]
	}
	return value
}

// percentDecode expands %XX escapes. A malformed or truncated escape reports
// ok=false so the caller can fall back to the raw value.
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
