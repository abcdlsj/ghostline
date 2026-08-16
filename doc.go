// Package ghostline provides embeddable Unix pseudo-terminal sessions backed
// by libghostty-vt screen replays and append-only output spools. A Hub runs
// sessions in-process; a Server and Client expose the same Session API over a
// Unix socket.
package ghostline
