package ghostline

import "testing"

func TestFindOSC133Markers(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []osc133Span
	}{
		{
			name:  "no marker",
			input: "plain output\r\n",
		},
		{
			name:  "prompt and input",
			input: "\x1b]133;A\x07$ \x1b]133;B\x07echo hi",
			want: []osc133Span{
				{kind: 'A', end: 8},
				{kind: 'B', end: 18},
			},
		},
		{
			name:  "command start and exit with bel",
			input: "\x1b]133;C\x07out\x1b]133;D;0\x07",
			want: []osc133Span{
				{kind: 'C', end: 8},
				{kind: 'D', exitCode: 0, hasExit: true, end: 21},
			},
		},
		{
			name:  "exit with st terminator",
			input: "\x1b]133;D;130\x1b\\",
			want: []osc133Span{
				{kind: 'D', exitCode: 130, hasExit: true, end: 13},
			},
		},
		{
			name:  "exit without code",
			input: "\x1b]133;D\x07",
			want: []osc133Span{
				{kind: 'D', end: 8},
			},
		},
		{
			name:  "exit with trailing parameter",
			input: "\x1b]133;D;7;ignored\x07",
			want: []osc133Span{
				{kind: 'D', exitCode: 7, hasExit: true, end: 18},
			},
		},
		{
			name:  "non numeric exit is unknown",
			input: "\x1b]133;D;abc\x07",
			want: []osc133Span{
				{kind: 'D', end: 12},
			},
		},
		{
			name:  "unterminated marker is not reported",
			input: "\x1b]133;A",
		},
		{
			name:  "other osc is ignored",
			input: "\x1b]7;file://localhost/tmp\x07\x1b]0;title\x07",
		},
		{
			name:  "unknown semantic letter is reported without meaning",
			input: "\x1b]133;P;k=v\x07",
			want: []osc133Span{
				{kind: 'P', end: 12},
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := findOSC133([]byte(testCase.input))
			if len(got) != len(testCase.want) {
				t.Fatalf("findOSC133(%q) = %+v, want %+v", testCase.input, got, testCase.want)
			}
			for index := range got {
				if got[index] != testCase.want[index] {
					t.Fatalf("marker %d = %+v, want %+v", index, got[index], testCase.want[index])
				}
			}
		})
	}
}

func TestParseOSC133ExitBoundsDigits(t *testing.T) {
	if code, ok := parseOSC133Exit([]byte(";123456")); !ok || code != 123456 {
		t.Fatalf("bounded exit = (%d,%v), want (123456,true)", code, ok)
	}
	if _, ok := parseOSC133Exit([]byte(";1234567")); ok {
		t.Fatal("exit with too many digits was accepted")
	}
	if _, ok := parseOSC133Exit([]byte("")); ok {
		t.Fatal("empty exit payload was accepted")
	}
	if _, ok := parseOSC133Exit([]byte("123")); ok {
		t.Fatal("exit payload without separator was accepted")
	}
}
