module github.com/abcdlsj/ghostline/examples/minimux

go 1.25

require (
	github.com/abcdlsj/ghostline v0.0.0
	golang.org/x/term v0.32.0
)

require (
	github.com/creack/pty v1.1.24 // indirect
	golang.org/x/sys v0.35.0 // indirect
)

replace github.com/abcdlsj/ghostline => ../..
