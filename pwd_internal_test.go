package ghostline

import "testing"

func TestParseReportedPwd(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"file uri", "file:///tmp/repo", "/tmp/repo"},
		{"file uri with localhost", "file://localhost/tmp/repo", "/tmp/repo"},
		{"file uri percent encoded", "file:///tmp/herdr%20repo", "/tmp/herdr repo"},
		{"file uri uppercase host", "file://LOCALHOST/tmp/repo", "/tmp/repo"},
		{"bare absolute path", "/tmp/repo", "/tmp/repo"},
		{"quoted absolute path", `"/tmp/my repo"`, "/tmp/my repo"},
		{"trimmed whitespace", "  /tmp/repo\n", "/tmp/repo"},
		{"empty", "", ""},
		{"relative path", "tmp/repo", ""},
		{"remote host", "file://remote/tmp/repo", ""},
		{"host only", "file://remote", ""},
		{"truncated escape", "file:///tmp/%2", ""},
		{"invalid escape", "file:///tmp/%zz", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := parseReportedPwd(testCase.raw); got != testCase.want {
				t.Fatalf("parseReportedPwd(%q) = %q, want %q", testCase.raw, got, testCase.want)
			}
		})
	}
}
