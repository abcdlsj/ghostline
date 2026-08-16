package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/abcdlsj/ghostline"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serveCommand(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "ghostline: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ghostline serve --socket <path> [--output-dir <dir>] [--default-term <term>]")
}

// serveCommand runs the standalone session server. The server owns PTY
// processes and the libghostty-vt emulator state; clients reconnect to the
// socket across restarts, so sessions survive client upgrades.
func serveCommand(args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	socket := flags.String("socket", "", "unix socket path (required)")
	outputDir := flags.String("output-dir", "", "output spool directory (default ~/.ghostline/output)")
	defaultTerm := flags.String("default-term", "", "TERM for sessions without one (default xterm-256color)")
	_ = flags.Parse(args)
	if *socket == "" {
		fmt.Fprintln(os.Stderr, "ghostline serve: --socket is required")
		os.Exit(2)
	}
	dir := *outputDir
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".ghostline", "output")
	}
	server, err := ghostline.NewServer(ghostline.Options{OutputDir: dir, DefaultTerm: *defaultTerm})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghostline serve: %v\n", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Serve(ctx, *socket); err != nil {
		fmt.Fprintf(os.Stderr, "ghostline serve: %v\n", err)
		os.Exit(1)
	}
	if ctx.Err() != nil {
		_ = server.Shutdown(context.Background())
	}
}
