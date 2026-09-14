//go:build darwin

package ghostline

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCommStringTrimsAtNUL(t *testing.T) {
	cases := []struct {
		raw  []byte
		want string
	}{
		{[]byte("zsh\x00stale\x00"), "zsh"},
		{[]byte("bash"), "bash"},
		{[]byte("\x00"), ""},
		{nil, ""},
	}
	for _, testCase := range cases {
		if got := commString(testCase.raw); got != testCase.want {
			t.Fatalf("commString(%q) = %q, want %q", testCase.raw, got, testCase.want)
		}
	}
}

func TestParseProcArgs(t *testing.T) {
	buffer := func(execPath string, argv ...string) []byte {
		raw := make([]byte, 4)
		binary.NativeEndian.PutUint32(raw, uint32(len(argv)))
		raw = append(raw, execPath...)
		// Exec path terminator plus the alignment padding the kernel emits
		// before argv[0].
		raw = append(raw, 0, 0, 0, 0)
		for _, arg := range argv {
			raw = append(raw, arg...)
			raw = append(raw, 0)
		}
		return raw
	}

	argv := parseProcArgs(buffer("/usr/bin/npm", "/usr/bin/npm", "run", "dev"))
	if strings.Join(argv, " ") != "/usr/bin/npm run dev" {
		t.Fatalf("parseProcArgs argv = %q", argv)
	}

	if got := parseProcArgs(nil); got != nil {
		t.Fatalf("parseProcArgs(nil) = %q, want nil", got)
	}
	if got := parseProcArgs([]byte{0, 0, 0, 0}); got != nil {
		t.Fatalf("parseProcArgs(zero argc) = %q, want nil", got)
	}
}

func TestProcessCommandLineMatchesArgs(t *testing.T) {
	got := processCommandLine(unix.Getpid(), "fallback")
	want := strings.Join(os.Args, " ")
	if got != want {
		t.Fatalf("processCommandLine = %q, want %q", got, want)
	}
}
