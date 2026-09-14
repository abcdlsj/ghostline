//go:build darwin

package ghostline

import "testing"

func TestParseForegroundPSLine(t *testing.T) {
	cases := []struct {
		line    string
		pgid    int
		command string
		ok      bool
	}{
		{"  96263  96263 sleep 3", 96263, "sleep 3", true},
		{"  96263  96263   /bin/zsh -lc 'git status'", 96263, "/bin/zsh -lc 'git status'", true},
		{"  1     1 /sbin/launchd", 1, "/sbin/launchd", true},
		{"  96263  96263 npm run dev -- --port 3000", 96263, "npm run dev -- --port 3000", true},
		{"", 0, "", false},
		{"  96263", 0, "", false},
		{"  96263  notanumber sleep", 0, "", false},
		{"  96263  96263   ", 0, "", false},
	}
	for _, testCase := range cases {
		pgid, command, ok := parseForegroundPSLine(testCase.line)
		if ok != testCase.ok || pgid != testCase.pgid || command != testCase.command {
			t.Fatalf(
				"parseForegroundPSLine(%q) = (%d, %q, %v), want (%d, %q, %v)",
				testCase.line, pgid, command, ok, testCase.pgid, testCase.command, testCase.ok,
			)
		}
	}
}

func TestFirstCommandToken(t *testing.T) {
	cases := map[string]string{
		"sleep 3":               "sleep",
		"/bin/zsh -lc go build": "/bin/zsh",
		"npm run dev":           "npm",
		"solo":                  "solo",
	}
	for command, want := range cases {
		if got := firstCommandToken(command); got != want {
			t.Fatalf("firstCommandToken(%q) = %q, want %q", command, got, want)
		}
	}
}
