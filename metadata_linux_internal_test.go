//go:build linux

package ghostline

import "testing"

func TestFormatCmdline(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"args", []byte("sleep\x0030\x00"), "sleep 30"},
		{"trailing nul only", []byte("npm\x00run\x00dev\x00"), "npm run dev"},
		{"empty", nil, ""},
		{"nuls only", []byte{0, 0}, ""},
		{"argument with spaces", []byte("sh\x00-c\x00echo hello world\x00"), "sh -c echo hello world"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := formatCmdline(testCase.raw); got != testCase.want {
				t.Fatalf("formatCmdline(%q) = %q, want %q", testCase.raw, got, testCase.want)
			}
		})
	}
}
