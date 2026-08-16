// Command minimux is a tiny in-process terminal multiplexer built on ghostline.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/abcdlsj/ghostline"
	"golang.org/x/term"
)

const commandPrefix = byte(0x02) // Ctrl-B

type window struct {
	session   *ghostline.Session
	watcher   *ghostline.SpoolWatcher
	responder *ghostline.QueryResponder
}

type minimux struct {
	manager *ghostline.Manager
	stdin   *os.File
	stdout  *os.File
	dir     string

	windows []*window
	current int
	nextID  int
	size    ghostline.Size
	prefix  bool

	active atomic.Pointer[window]
	output sync.Mutex
	done   chan *window
	closed chan struct{}
}

func main() {
	flag.Parse()
	if err := run(strings.Join(flag.Args(), " ")); err != nil {
		fmt.Fprintln(os.Stderr, "minimux:", err)
		os.Exit(1)
	}
}

func run(initialCommand string) error {
	stdin, stdout := os.Stdin, os.Stdout
	inputFD := int(stdin.Fd())
	if !term.IsTerminal(inputFD) {
		return errors.New("stdin is not a terminal")
	}
	size, err := terminalSize(stdin)
	if err != nil {
		return fmt.Errorf("read terminal size: %w", err)
	}
	raw, err := term.MakeRaw(inputFD)
	if err != nil {
		return fmt.Errorf("enable raw mode: %w", err)
	}
	defer term.Restore(inputFD, raw)

	outputDir, err := os.MkdirTemp("", "ghostline-minimux-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(outputDir)
	manager, err := ghostline.New(ghostline.Options{
		OutputDir:   outputDir,
		DefaultSize: size,
	})
	if err != nil {
		return err
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		_ = manager.Close()
		return err
	}
	app := &minimux{
		manager: manager,
		stdin:   stdin,
		stdout:  stdout,
		dir:     workingDirectory,
		current: -1,
		size:    size,
		done:    make(chan *window),
		closed:  make(chan struct{}),
	}
	defer app.shutdown()

	if err := app.createWindow(initialCommand); err != nil {
		return err
	}
	if err := app.switchTo(0); err != nil {
		return err
	}

	input := make(chan []byte)
	inputErrors := make(chan error, 1)
	go readInput(stdin, input, inputErrors)
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGWINCH, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	for {
		select {
		case data := <-input:
			quit, err := app.handleInput(data)
			if err != nil {
				return err
			}
			if quit {
				return nil
			}
		case err := <-inputErrors:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case exited := <-app.done:
			quit, err := app.removeWindow(exited)
			if err != nil {
				return err
			}
			if quit {
				return nil
			}
		case received := <-signals:
			if received != syscall.SIGWINCH {
				return nil
			}
			if err := app.resize(); err != nil {
				return err
			}
		}
	}
}

func readInput(reader io.Reader, output chan<- []byte, failures chan<- error) {
	buffer := make([]byte, 4096)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			data := append([]byte(nil), buffer[:count]...)
			output <- data
		}
		if err != nil {
			failures <- err
			return
		}
	}
}

func (m *minimux) createWindow(command string) error {
	m.nextID++
	session, err := m.manager.Start(context.Background(), ghostline.SessionOptions{
		Name:      fmt.Sprintf("window-%d", m.nextID),
		Directory: m.dir,
		Command:   command,
		Size:      m.size,
	})
	if err != nil {
		return err
	}
	w := &window{session: session, responder: ghostline.NewQueryResponder()}
	w.responder.Resize(m.size.Columns, m.size.Rows)
	w.watcher, err = session.WatchOutput(ghostline.WatchOptions{
		OnOutput: func(data []byte) { m.handleOutput(w, data) },
	})
	if err != nil {
		_ = session.Close()
		return err
	}
	m.windows = append(m.windows, w)
	go func() {
		<-session.Done()
		select {
		case m.done <- w:
		case <-m.closed:
		}
	}()
	return nil
}

func (m *minimux) handleOutput(w *window, data []byte) {
	m.output.Lock()
	if m.active.Load() == w {
		_, _ = m.stdout.Write(data)
		m.output.Unlock()
		return
	}
	m.output.Unlock()
	for _, reply := range w.responder.Feed(data) {
		_ = w.session.Input(context.Background(), reply)
	}
}

func (m *minimux) switchTo(index int) error {
	if len(m.windows) == 0 {
		return errors.New("no windows")
	}
	index = (index + len(m.windows)) % len(m.windows)
	next := m.windows[index]
	next.watcher.Pause()
	defer next.watcher.Resume()
	if err := next.session.Resize(context.Background(), m.size); err != nil {
		return err
	}
	checkpoint, err := next.session.Checkpoint(context.Background())
	if err != nil {
		return err
	}
	if err := next.watcher.SkipTo(checkpoint.Offset); err != nil {
		return err
	}

	m.output.Lock()
	m.active.Store(next)
	m.current = index
	_, _ = fmt.Fprintf(m.stdout, "\x1b]0;minimux:%s\x07", next.session.Name())
	_, _ = m.stdout.Write(checkpoint.Replay)
	m.output.Unlock()
	return nil
}

func (m *minimux) handleInput(data []byte) (bool, error) {
	plain := make([]byte, 0, len(data))
	flush := func() error {
		if len(plain) == 0 || m.current < 0 {
			return nil
		}
		err := m.windows[m.current].session.Input(context.Background(), plain)
		plain = plain[:0]
		return err
	}
	for _, value := range data {
		if !m.prefix {
			if value == commandPrefix {
				if err := flush(); err != nil {
					return false, err
				}
				m.prefix = true
				continue
			}
			plain = append(plain, value)
			continue
		}

		m.prefix = false
		switch value {
		case commandPrefix:
			plain = append(plain, value)
		case 'c':
			if err := m.createWindow(""); err != nil {
				return false, err
			}
			if err := m.switchTo(len(m.windows) - 1); err != nil {
				return false, err
			}
		case 'n':
			if err := m.switchTo(m.current + 1); err != nil {
				return false, err
			}
		case 'p':
			if err := m.switchTo(m.current - 1); err != nil {
				return false, err
			}
		case 'x':
			quit, err := m.removeWindow(m.windows[m.current])
			if quit || err != nil {
				return quit, err
			}
		case 'q':
			return true, nil
		default:
			plain = append(plain, commandPrefix, value)
		}
	}
	return false, flush()
}

func (m *minimux) removeWindow(target *window) (bool, error) {
	index := -1
	for candidate, w := range m.windows {
		if w == target {
			index = candidate
			break
		}
	}
	if index < 0 {
		return false, nil
	}
	m.output.Lock()
	if m.active.Load() == target {
		m.active.Store(nil)
	}
	m.output.Unlock()
	target.watcher.Close()
	if err := target.session.Close(); err != nil {
		return false, err
	}
	m.windows = append(m.windows[:index], m.windows[index+1:]...)
	if len(m.windows) == 0 {
		m.current = -1
		return true, nil
	}
	if index >= len(m.windows) {
		index = 0
	}
	return false, m.switchTo(index)
}

func (m *minimux) resize() error {
	size, err := terminalSize(m.stdin)
	if err != nil {
		return err
	}
	m.size = size
	for _, w := range m.windows {
		w.responder.Resize(size.Columns, size.Rows)
		if err := w.session.Resize(context.Background(), m.size); err != nil && w.session.Alive() {
			return err
		}
	}
	return nil
}

func terminalSize(terminal *os.File) (ghostline.Size, error) {
	columns, rows, err := term.GetSize(int(terminal.Fd()))
	if err != nil {
		return ghostline.Size{}, err
	}
	if columns <= 0 || rows <= 0 {
		return ghostline.Size{Columns: 120, Rows: 36}, nil
	}
	return ghostline.Size{Columns: columns, Rows: rows}, nil
}

func (m *minimux) shutdown() {
	close(m.closed)
	m.output.Lock()
	m.active.Store(nil)
	_, _ = m.stdout.Write([]byte("\x1b[0m\x1b[?25h\r\n"))
	m.output.Unlock()
	for _, w := range m.windows {
		w.watcher.Close()
	}
	_ = m.manager.Close()
}
