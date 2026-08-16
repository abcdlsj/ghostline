package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

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
	fmt.Fprintln(os.Stderr, "usage: ghostline serve --socket <path> [--output-dir <dir>] [--default-term <term>] [--adopt-from <admin-socket>] [--probe-foreground]")
}

// serveCommand runs the standalone session server. The server owns PTY
// processes and the libghostty-vt emulator state; clients reconnect to the
// socket across restarts, so sessions survive client upgrades.
func serveCommand(args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	socket := flags.String("socket", "", "unix socket path (required)")
	outputDir := flags.String("output-dir", "", "output spool directory (default ~/.ghostline/output)")
	defaultTerm := flags.String("default-term", "", "TERM for sessions without one (default xterm-256color)")
	adoptFrom := flags.String("adopt-from", "", "old server admin socket to adopt sessions from before serving")
	probeForeground := flags.Bool("probe-foreground", false, "probe OS-level foreground process/cwd metadata (off by default)")
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
	server, err := ghostline.NewServer(ghostline.Options{
		OutputDir:       dir,
		DefaultTerm:     *defaultTerm,
		ProbeForeground: *probeForeground,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghostline serve: %v\n", err)
		os.Exit(1)
	}
	if *adoptFrom != "" {
		adoptContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		adopted, adoptErr := server.Adopt(adoptContext, *adoptFrom)
		cancel()
		if adoptErr != nil {
			fmt.Fprintf(os.Stderr, "ghostline serve: adopt from %s: %v\n", *adoptFrom, adoptErr)
			os.Exit(1)
		}
		if adopted > 0 {
			fmt.Fprintf(os.Stderr, "ghostline serve: adopted %d session(s)\n", adopted)
		}
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
