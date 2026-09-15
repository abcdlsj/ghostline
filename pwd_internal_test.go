package ghostline

import (
	"strings"
	"testing"
)

func TestParseReportedPwd(t *testing.T) {
	hostname := localHostname()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"file uri", "file:///tmp/repo", "/tmp/repo"},
		{"file uri with localhost", "file://localhost/tmp/repo", "/tmp/repo"},
		{"file uri percent encoded", "file:///tmp/herdr%20repo", "/tmp/herdr repo"},
		{"file uri uppercase host", "file://LOCALHOST/tmp/repo", "/tmp/repo"},
		{"kitty cwd scheme", "kitty-shell-cwd://localhost/tmp/repo", "/tmp/repo"},
		{"bare absolute path", "/tmp/repo", "/tmp/repo"},
		{"quoted absolute path", `"/tmp/my repo"`, "/tmp/my repo"},
		{"trimmed whitespace", "  /tmp/repo\n", "/tmp/repo"},
		{"empty", "", ""},
		{"relative path", "tmp/repo", ""},
		{"remote host", "file://remote.invalid/tmp/repo", ""},
		{"host only", "file://remote.invalid", ""},
		{"truncated escape keeps raw path", "file:///tmp/%2", "/tmp/%2"},
		{"invalid escape keeps raw path", "file:///tmp/%zz", "/tmp/%zz"},
		{"literal percent keeps raw path", "file:///tmp/100%", "/tmp/100%"},
	}
	if hostname != "" {
		cases = append(cases,
			struct {
				name string
				raw  string
				want string
			}{"local hostname", "file://" + hostname + "/tmp/repo", "/tmp/repo"},
			struct {
				name string
				raw  string
				want string
			}{"local short hostname", "file://" + shortHostname(hostname) + "/tmp/repo", "/tmp/repo"},
		)
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := parseReportedPwd(testCase.raw); got != testCase.want {
				t.Fatalf("parseReportedPwd(%q) = %q, want %q", testCase.raw, got, testCase.want)
			}
		})
	}
}

func TestIsLocalHost(t *testing.T) {
	if isLocalHost("remote.invalid", "studio.local") {
		t.Fatal("remote host was treated as local")
	}
	if !isLocalHost("studio", "studio.local") {
		t.Fatal("short local hostname was rejected")
	}
	if !isLocalHost("studio.local", "studio") {
		t.Fatal("local hostname was rejected against a short local name")
	}
	if !isLocalHost("LOCALHOST", "") {
		t.Fatal("localhost was rejected")
	}
	if strings.TrimSpace(localHostname()) == "" {
		t.Skip("hostname unavailable")
	}
}
