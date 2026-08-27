package ghostline_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abcdlsj/ghostline"
)

func startTestServer(t *testing.T) (string, *ghostline.Client) {
	return startTestServerWithOptions(t, ghostline.Options{OutputDir: t.TempDir()})
}

func requireScaleTest(t *testing.T) {
	t.Helper()
	if testing.Short() || os.Getenv("GHOSTLINE_SCALE") != "1" {
		t.Skip("scale test; set GHOSTLINE_SCALE=1")
	}
}

func startTestServerWithOptions(t *testing.T, options ghostline.Options) (string, *ghostline.Client) {
	t.Helper()
	socketDir, err := os.MkdirTemp("", "ghostline-")
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	socket := filepath.Join(socketDir, "ghostline.sock")
	server, err := ghostline.NewServer(options)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(context.Background(), socket)
	}()
	client := ghostline.NewClient(socket)
	deadline := time.Now().Add(3 * time.Second)
	err = client.Check(context.Background())
	for err != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		err = client.Check(context.Background())
	}
	if err != nil {
		t.Fatal("server did not become ready")
	}
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		<-done
		_ = os.RemoveAll(socketDir)
	})
	return socket, client
}

// TestServerHelperProcess is the subprocess spawned by Connect tests.
func TestServerHelperProcess(t *testing.T) {
	if os.Getenv("GHOSTLINE_HELPER") != "1" {
		return
	}
	server, err := ghostline.NewServer(ghostline.Options{
		OutputDir: os.Getenv("GHOSTLINE_HELPER_DIR"),
	})
	if err != nil {
		os.Exit(1)
	}
	if err := server.Serve(context.Background(), os.Getenv("GHOSTLINE_HELPER_SOCKET")); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func waitRemoteReplay(t *testing.T, session *ghostline.Session, needle string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := session.Replay(context.Background())
		if err == nil && bytes.Contains(snapshot, []byte(needle)) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("remote output did not contain %q", needle)
}

func TestClientLifecycle(t *testing.T) {
	_, client := startTestServer(t)
	ctx := context.Background()
	session, err := client.Start(ctx, ghostline.SessionOptions{
		Name:    "serve",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	status, err := session.Status(ctx)
	if err != nil || !status.Alive {
		t.Fatal("session should be alive")
	}
	if err := session.WriteInput(ctx, []byte("echo hello-serve\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitRemoteReplay(t, session, "hello-serve")
	sessions, err := client.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Name() != "serve" {
		t.Fatalf("List = %v", sessions)
	}
	if err := session.Terminate(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	status, err = session.Status(ctx)
	if err != nil || status.Alive {
		t.Fatal("session alive after Close")
	}
	if err := session.Delete(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestClientStartSendsSizeAndEnvironment(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "configured",
		Process: ghostline.ProcessSpec{
			Path: "sh", Directory: t.TempDir(),
			Environment: []string{"GHOSTLINE_TEST=remote"},
		},
		Size: ghostline.Size{Columns: 90, Rows: 28},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("stty size; echo env=$GHOSTLINE_TEST\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitRemoteReplay(t, session, "28 90")
	waitRemoteReplay(t, session, "env=remote")
}

func TestClientSessionByNameAndSessions(t *testing.T) {
	_, client := startTestServer(t)
	ctx := context.Background()
	if _, err := client.Start(ctx, ghostline.SessionOptions{
		Name:    "ghost_test_named",
		Process: ghostline.ProcessSpec{Path: "sh"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	session, err := client.Get(ctx, "ghost_test_named")
	if err != nil {
		t.Fatal("Session should find the started session")
	}
	if session.Name() != "ghost_test_named" {
		t.Fatalf("session name = %q", session.Name())
	}
	if session.CreatedAt().IsZero() {
		t.Fatal("Session handle should resolve CreatedAt lazily")
	}
	if err := session.WriteInput(ctx, []byte("echo named-ok\r")); err != nil {
		t.Fatalf("Input on named session: %v", err)
	}

	if _, err := client.Get(ctx, "ghost_test_missing"); !errors.Is(err, ghostline.ErrSessionNotFound) {
		t.Fatal("Session should not find a missing session")
	}

	sessions, err := client.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, existing := range sessions {
		if existing.Name() == "ghost_test_named" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Sessions missing the started session: %d handles", len(sessions))
	}
}

func TestClientVersionReportsProtocol(t *testing.T) {
	_, client := startTestServer(t)
	version, err := client.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version != ghostline.ProtocolVersion {
		t.Fatalf("Version = %q, want %q", version, ghostline.ProtocolVersion)
	}
}

func TestClientVersionInfoReportsProtocolAndTag(t *testing.T) {
	_, client := startTestServer(t)
	info, err := client.VersionInfo(context.Background())
	if err != nil {
		t.Fatalf("VersionInfo: %v", err)
	}
	if info.ProtocolVersion != ghostline.ProtocolVersion {
		t.Fatalf("VersionInfo protocol = %q, want %q", info.ProtocolVersion, ghostline.ProtocolVersion)
	}
	if info.TagVersion != ghostline.TagVersion() {
		t.Fatalf("VersionInfo tag = %q, want %q", info.TagVersion, ghostline.TagVersion())
	}
	capabilities := make(map[string]bool, len(info.Capabilities))
	for _, capability := range info.Capabilities {
		capabilities[capability] = true
	}
	if !capabilities[ghostline.CapabilityRawPayload] || !capabilities[ghostline.CapabilityStreams] || !capabilities[ghostline.CapabilityAtomicState] {
		t.Fatalf("VersionInfo capabilities = %v", info.Capabilities)
	}
	if info.Limits.MaxHeaderBytes <= 0 || info.Limits.MaxPayloadBytes <= 0 || info.Limits.MaxChunkBytes <= 0 ||
		info.Limits.MaxChunkBytes > info.Limits.MaxPayloadBytes {
		t.Fatalf("VersionInfo limits = %+v", info.Limits)
	}
	if info.MaxClientConnections != ghostline.DefaultServerMaxClientConnections {
		t.Fatalf("VersionInfo max client connections = %d, want %d", info.MaxClientConnections, ghostline.DefaultServerMaxClientConnections)
	}
}

func TestClientAtomicStateStreamsOpaqueVTStateAndCursor(t *testing.T) {
	_, client := startTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Start(ctx, ghostline.SessionOptions{
		Name:    "remote-atomic-state",
		Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(ctx, []byte("printf 'remote-atomic-state-output\\r\\n'\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitRemoteReplay(t, session, "remote-atomic-state-output")
	state, err := session.AtomicState(ctx)
	if err != nil {
		t.Fatalf("AtomicState: %v", err)
	}
	if state.Format != ghostline.AtomicStateFormat {
		t.Fatalf("AtomicState format = %q, want %q", state.Format, ghostline.AtomicStateFormat)
	}
	if len(state.Payload) == 0 || state.Cursor == (ghostline.Cursor{}) {
		t.Fatalf("AtomicState = payload %d bytes, cursor %q", len(state.Payload), state.Cursor)
	}
	if !bytes.HasPrefix(state.Payload, []byte("GHOSTSNP")) {
		t.Fatalf("AtomicState payload does not start with the Ghostty snapshot magic: %q", state.Payload[:min(len(state.Payload), 16)])
	}
}

func TestClientVersionInfoReportsConfiguredConnectionLimit(t *testing.T) {
	_, client := startTestServerWithOptions(t, ghostline.Options{
		OutputDir:                  t.TempDir(),
		ServerMaxClientConnections: 17,
	})
	info, err := client.VersionInfo(context.Background())
	if err != nil {
		t.Fatalf("VersionInfo: %v", err)
	}
	if info.MaxClientConnections != 17 {
		t.Fatalf("VersionInfo max client connections = %d, want 17", info.MaxClientConnections)
	}
}

func TestClientStreamsLargeReplayAndCheckpoint(t *testing.T) {
	_, client := startTestServerWithOptions(t, ghostline.Options{
		OutputDir:            t.TempDir(),
		VTScrollbackMaxBytes: 32 << 20,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	line := strings.Repeat("x", 256)
	session, err := client.Start(ctx, ghostline.SessionOptions{
		Name:    "large-replay",
		Process: ghostline.Shell(fmt.Sprintf("yes %s | head -c 1600000", line)),
		Size:    ghostline.Size{Columns: 300, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	output, err := session.Output(ctx, ghostline.Cursor{})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	written, copyErr := io.Copy(io.Discard, output)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("drain output = (%v, %v)", copyErr, closeErr)
	}
	if written < 1<<20 {
		t.Fatalf("raw output = %d bytes, want more than one RPC frame", written)
	}
	if err := session.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	replay, err := session.Replay(ctx)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(replay) <= 1<<20 {
		t.Fatalf("Replay = %d bytes, want chunked payload larger than one RPC frame", len(replay))
	}
	checkpoint, err := session.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !bytes.Equal(checkpoint.Replay, replay) {
		t.Fatalf("Checkpoint replay differs from stable Replay: %d != %d bytes", len(checkpoint.Replay), len(replay))
	}
	if checkpoint.Cursor == (ghostline.Cursor{}) {
		t.Fatal("Checkpoint returned a zero cursor after output")
	}
}

func TestDaemonSupportsHundredsOfLiveSessionsAndOutputStreams(t *testing.T) {
	requireScaleTest(t)
	const sessionCount = 256
	beforeGoroutines := runtime.NumGoroutine()
	_, client := startTestServerWithOptions(t, ghostline.Options{
		OutputDir:            t.TempDir(),
		VTScrollbackMaxBytes: 64 << 10,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sessions := make([]*ghostline.Session, 0, sessionCount)
	readers := make([]*ghostline.OutputReader, 0, sessionCount)
	t.Cleanup(func() {
		for _, reader := range readers {
			_ = reader.Close()
		}
	})
	start := time.Now()
	for index := range sessionCount {
		session, err := client.Start(ctx, ghostline.SessionOptions{
			Name: fmt.Sprintf("scale-%03d", index),
			Process: ghostline.ProcessSpec{
				Path: "sleep", Args: []string{"120"},
			},
		})
		if err != nil {
			t.Fatalf("Start %d/%d: %v", index+1, sessionCount, err)
		}
		reader, err := session.Output(ctx, ghostline.Cursor{})
		if err != nil {
			t.Fatalf("Output %d/%d: %v", index+1, sessionCount, err)
		}
		sessions = append(sessions, session)
		readers = append(readers, reader)
	}
	opened := time.Now()

	errs := make(chan error, sessionCount)
	var wait sync.WaitGroup
	for _, session := range sessions {
		wait.Add(1)
		go func(session *ghostline.Session) {
			defer wait.Done()
			status, err := session.Status(ctx)
			if err != nil {
				errs <- err
				return
			}
			if !status.Alive {
				errs <- errors.New("live scale session reported stopped")
			}
		}(session)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	statusDone := time.Now()
	if rawHold := os.Getenv("GHOSTLINE_SCALE_IDLE"); rawHold != "" {
		hold, err := time.ParseDuration(rawHold)
		if err != nil {
			t.Fatalf("parse GHOSTLINE_SCALE_IDLE: %v", err)
		}
		if hold > 0 {
			t.Logf("holding %d live sessions idle for %s", sessionCount, hold)
			time.Sleep(hold)
		}
	}
	t.Logf(
		"%d live sessions and output streams: start/open=%s, concurrent status=%s, goroutines delta=%d",
		sessionCount,
		opened.Sub(start).Round(time.Millisecond),
		statusDone.Sub(opened).Round(time.Millisecond),
		runtime.NumGoroutine()-beforeGoroutines,
	)
}

func TestServerRejectsNegativeMaxClientConnections(t *testing.T) {
	if _, err := ghostline.NewServer(ghostline.Options{ServerMaxClientConnections: -1}); err == nil {
		t.Fatal("NewServer accepted a negative connection limit")
	}
}

func TestServerEnforcesMaxClientConnections(t *testing.T) {
	_, client := startTestServerWithOptions(t, ghostline.Options{
		OutputDir:                  t.TempDir(),
		ServerMaxClientConnections: 2,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Let the readiness probe's accepted socket reach EOF and release its slot.
	time.Sleep(20 * time.Millisecond)
	session, err := client.Start(ctx, ghostline.SessionOptions{
		Name: "connection-limit",
		Process: ghostline.ProcessSpec{
			Path: "sleep", Args: []string{"30"},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	first, err := session.Output(ctx, ghostline.Cursor{})
	if err != nil {
		t.Fatalf("first Output: %v", err)
	}
	second, err := session.Output(ctx, ghostline.Cursor{})
	if err != nil {
		_ = first.Close()
		t.Fatalf("second Output: %v", err)
	}
	if _, err := client.VersionInfo(ctx); err == nil {
		_ = first.Close()
		_ = second.Close()
		t.Fatal("VersionInfo succeeded while every connection slot was occupied")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first output: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close second output: %v", err)
	}
	for {
		if _, err := client.VersionInfo(ctx); err == nil {
			break
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("VersionInfo after releasing connection slot: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDaemonWarrenAttachDetachAndResizeLatency(t *testing.T) {
	requireScaleTest(t)
	const (
		sessionCount = 256
		churnRounds  = 512
		resizePasses = 10
		resizeWidth  = 32
	)
	_, client := startTestServerWithOptions(t, ghostline.Options{
		OutputDir:            t.TempDir(),
		VTScrollbackMaxBytes: 64 << 10,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	sessions := make([]*ghostline.Session, 0, sessionCount)
	for index := range sessionCount {
		process := ghostline.ProcessSpec{Path: "sleep", Args: []string{"120"}}
		if index == 0 {
			process = ghostline.Shell("yes warren-history | head -c 65536; sleep 120")
		}
		session, err := client.Start(ctx, ghostline.SessionOptions{
			Name:    fmt.Sprintf("warren-%03d", index),
			Process: process,
		})
		if err != nil {
			t.Fatalf("Start %d/%d: %v", index+1, sessionCount, err)
		}
		sessions = append(sessions, session)
	}
	active := sessions[0]
	historyDeadline := time.Now().Add(5 * time.Second)
	activeReplayBytes := 0
	for {
		checkpoint, err := active.Checkpoint(ctx)
		if err != nil {
			t.Fatalf("wait for active history: %v", err)
		}
		activeReplayBytes = len(checkpoint.Replay)
		if activeReplayBytes >= 4<<10 {
			break
		}
		if time.Now().After(historyDeadline) {
			t.Fatalf("active replay = %d bytes, want at least 4 KiB", activeReplayBytes)
		}
		time.Sleep(10 * time.Millisecond)
	}
	beforeChurnGoroutines := runtime.NumGoroutine()
	beforeChurnFDs, haveFDCount := openFileDescriptorCount()

	attachLatency := make([]time.Duration, 0, churnRounds)
	checkpointLatency := make([]time.Duration, 0, churnRounds)
	outputOpenLatency := make([]time.Duration, 0, churnRounds)
	detachLatency := make([]time.Duration, 0, churnRounds)
	resizeLatency := make([]time.Duration, 0, churnRounds)
	for round := range churnRounds {
		started := time.Now()
		checkpoint, err := active.Checkpoint(ctx)
		if err != nil {
			t.Fatalf("Checkpoint round %d: %v", round, err)
		}
		checkpointDone := time.Now()
		reader, err := active.Output(ctx, checkpoint.Cursor)
		if err != nil {
			t.Fatalf("Output round %d: %v", round, err)
		}
		outputOpened := time.Now()
		checkpointLatency = append(checkpointLatency, checkpointDone.Sub(started))
		outputOpenLatency = append(outputOpenLatency, outputOpened.Sub(checkpointDone))
		attachLatency = append(attachLatency, outputOpened.Sub(started))

		columns := 80 + round%81
		rows := 24 + round%25
		started = time.Now()
		if err := active.Resize(ctx, ghostline.Size{Columns: columns, Rows: rows}); err != nil {
			_ = reader.Close()
			t.Fatalf("Resize round %d: %v", round, err)
		}
		resizeLatency = append(resizeLatency, time.Since(started))

		started = time.Now()
		if err := reader.Close(); err != nil {
			t.Fatalf("Close round %d: %v", round, err)
		}
		detachLatency = append(detachLatency, time.Since(started))
	}

	burstLatency := make([]time.Duration, 0, sessionCount*resizePasses)
	var burstWall time.Duration
	burstCount := 0
	for pass := range resizePasses {
		for first := 0; first < sessionCount; first += resizeWidth {
			last := first + resizeWidth
			if last > sessionCount {
				last = sessionCount
			}
			results := make(chan time.Duration, last-first)
			errs := make(chan error, last-first)
			var wait sync.WaitGroup
			started := time.Now()
			for index := first; index < last; index++ {
				session := sessions[index]
				wait.Add(1)
				go func(index int, session *ghostline.Session) {
					defer wait.Done()
					operationStarted := time.Now()
					err := session.Resize(ctx, ghostline.Size{
						Columns: 100 + (index+pass)%40,
						Rows:    30 + (index+pass)%20,
					})
					results <- time.Since(operationStarted)
					if err != nil {
						errs <- err
					}
				}(index, session)
			}
			wait.Wait()
			burstWall += time.Since(started)
			burstCount++
			close(results)
			close(errs)
			for err := range errs {
				t.Fatalf("concurrent Resize pass %d batch %d: %v", pass, first/resizeWidth, err)
			}
			for latency := range results {
				burstLatency = append(burstLatency, latency)
			}
		}
	}
	resourceDeadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > beforeChurnGoroutines+16 && time.Now().Before(resourceDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - beforeChurnGoroutines; delta > 16 {
		t.Fatalf("goroutines after attach/detach churn = %+d, want at most +16", delta)
	}
	if haveFDCount {
		for {
			afterFDs, _ := openFileDescriptorCount()
			if afterFDs <= beforeChurnFDs+16 || time.Now().After(resourceDeadline) {
				if delta := afterFDs - beforeChurnFDs; delta > 16 {
					t.Fatalf("file descriptors after attach/detach churn = %+d, want at most +16", delta)
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	t.Logf("attach checkpoint+output with %d-byte replay: %s", activeReplayBytes, formatLatencyPercentiles(attachLatency))
	t.Logf("checkpoint phase: %s", formatLatencyPercentiles(checkpointLatency))
	t.Logf("output-open phase: %s", formatLatencyPercentiles(outputOpenLatency))
	t.Logf("resize active session: %s", formatLatencyPercentiles(resizeLatency))
	t.Logf("detach close: %s", formatLatencyPercentiles(detachLatency))
	t.Logf(
		"resize across %d sessions in batches of %d: %s; mean batch wall=%s",
		sessionCount,
		resizeWidth,
		formatLatencyPercentiles(burstLatency),
		(burstWall / time.Duration(burstCount)).Round(time.Microsecond),
	)
}

func TestDaemonHighTrafficAcrossHundredsOfPTYs(t *testing.T) {
	requireScaleTest(t)
	const (
		sessionCount     = 256
		outputPerSession = 256 << 10
		inputWidth       = 32
	)
	_, client := startTestServerWithOptions(t, ghostline.Options{
		OutputDir:            t.TempDir(),
		VTScrollbackMaxBytes: 64 << 10,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	readers := make([]*ghostline.OutputReader, 0, sessionCount)
	sessions := make([]*ghostline.Session, 0, sessionCount)
	t.Cleanup(func() {
		for _, reader := range readers {
			_ = reader.Close()
		}
	})
	for index := range sessionCount {
		session, err := client.Start(ctx, ghostline.SessionOptions{
			Name: fmt.Sprintf("traffic-%03d", index),
			Process: ghostline.Shell(
				"stty -echo -icanon min 1 time 0; " +
					"printf R; " +
					"dd bs=1 count=1 of=/dev/null 2>/dev/null; " +
					"yes x | tr -d '\\n' | head -c 262144; " +
					"sleep 120",
			),
		})
		if err != nil {
			t.Fatalf("Start %d/%d: %v", index+1, sessionCount, err)
		}
		reader, err := session.Output(ctx, ghostline.Cursor{})
		if err != nil {
			t.Fatalf("Output %d/%d: %v", index+1, sessionCount, err)
		}
		var ready [1]byte
		if _, err := io.ReadFull(reader, ready[:]); err != nil || ready[0] != 'R' {
			t.Fatalf("ready %d/%d = (%q, %v)", index+1, sessionCount, ready, err)
		}
		sessions = append(sessions, session)
		readers = append(readers, reader)
	}

	deliveryLatency := make([]time.Duration, sessionCount)
	deliveryErrors := make(chan error, sessionCount)
	var deliveryWait sync.WaitGroup
	released := time.Now()
	for index, reader := range readers {
		deliveryWait.Add(1)
		go func(index int, reader *ghostline.OutputReader) {
			defer deliveryWait.Done()
			read, err := io.CopyN(io.Discard, reader, outputPerSession)
			deliveryLatency[index] = time.Since(released)
			if err != nil || read != outputPerSession {
				deliveryErrors <- fmt.Errorf("session %d output = (%d, %v)", index, read, err)
			}
		}(index, reader)
	}

	inputLatency := make([]time.Duration, 0, sessionCount)
	for first := 0; first < sessionCount; first += inputWidth {
		last := first + inputWidth
		if last > sessionCount {
			last = sessionCount
		}
		results := make(chan time.Duration, last-first)
		errs := make(chan error, last-first)
		var inputWait sync.WaitGroup
		for index := first; index < last; index++ {
			inputWait.Add(1)
			go func(session *ghostline.Session) {
				defer inputWait.Done()
				started := time.Now()
				err := session.WriteInput(ctx, []byte{'x'})
				results <- time.Since(started)
				if err != nil {
					errs <- err
				}
			}(sessions[index])
		}
		inputWait.Wait()
		close(results)
		close(errs)
		for err := range errs {
			t.Fatalf("release traffic process: %v", err)
		}
		for latency := range results {
			inputLatency = append(inputLatency, latency)
		}
	}

	deliveryWait.Wait()
	close(deliveryErrors)
	for err := range deliveryErrors {
		t.Fatal(err)
	}
	maximumDelivery := time.Duration(0)
	for _, latency := range deliveryLatency {
		if latency > maximumDelivery {
			maximumDelivery = latency
		}
	}
	totalBytes := float64(sessionCount * outputPerSession)
	throughputMiB := totalBytes / maximumDelivery.Seconds() / (1 << 20)
	t.Logf("traffic trigger input: %s", formatLatencyPercentiles(inputLatency))
	t.Logf(
		"256-PTY output delivery: total=64 MiB aggregate=%.1f MiB/s %s",
		throughputMiB,
		formatLatencyPercentiles(deliveryLatency),
	)
}

func openFileDescriptorCount() (int, bool) {
	for _, path := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(path)
		if err == nil {
			return len(entries), true
		}
	}
	return 0, false
}

func formatLatencyPercentiles(samples []time.Duration) string {
	ordered := append([]time.Duration(nil), samples...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	percentile := func(percent int) time.Duration {
		index := (len(ordered)*percent + 99) / 100
		if index > 0 {
			index--
		}
		return ordered[index].Round(time.Microsecond)
	}
	return fmt.Sprintf(
		"n=%d p50=%s p95=%s p99=%s max=%s",
		len(ordered),
		percentile(50),
		percentile(95),
		percentile(99),
		ordered[len(ordered)-1].Round(time.Microsecond),
	)
}

func TestClientWaitReturnsExitError(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "wait", Process: ghostline.Shell("exit 7"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var exitErr *ghostline.ExitError
	if err := session.Wait(context.Background()); !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Wait = %v, want exit code 7", err)
	}
	status, err := session.Status(context.Background())
	if err != nil || status.Exit == nil || status.Exit.Code != 7 {
		t.Fatalf("Status = (%+v, %v), want exit code 7", status, err)
	}
}

func TestClientErrorsPreserveIdentity(t *testing.T) {
	_, client := startTestServer(t)
	ctx := context.Background()
	options := ghostline.SessionOptions{Name: "dup", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()}}
	if _, err := client.Start(ctx, options); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := client.Start(ctx, options); !errors.Is(err, ghostline.ErrSessionExists) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := client.Start(ctx, ghostline.SessionOptions{Name: "../unsafe"}); !errors.Is(err, ghostline.ErrInvalidSessionName) {
		t.Fatalf("invalid name error = %v", err)
	}
}

func TestClientCheckpointAndRecover(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "checkpoint", Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.WriteInput(context.Background(), []byte("printf 'checkpoint-data\\r\\n'\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitRemoteReplay(t, session, "checkpoint-data")
	checkpoint, err := session.Checkpoint(context.Background())
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !bytes.Contains(checkpoint.Replay, []byte("checkpoint-data")) {
		t.Fatalf("replay missing output: %q", checkpoint.Replay)
	}
	if checkpoint.Cursor.String() == "" {
		t.Fatal("checkpoint returned a zero cursor")
	}
	readerCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reader, err := session.Output(readerCtx, ghostline.Cursor{})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	defer reader.Close()
	buffer := make([]byte, 1024)
	n, err := reader.Read(buffer)
	if err != nil {
		t.Fatalf("Read output: %v", err)
	}
	if !bytes.Contains(buffer[:n], []byte("checkpoint-data")) {
		t.Fatalf("output missing checkpoint data: %q", buffer[:n])
	}
}

func TestClientWaitCancellationKeepsSession(t *testing.T) {
	_, client := startTestServer(t)
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "cancel", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := session.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v", err)
	}
	status, statusErr := session.Status(context.Background())
	if statusErr != nil || !status.Alive {
		t.Fatal("canceling Wait terminated the session")
	}
}

func TestServerSocketPermissions(t *testing.T) {
	socket, _ := startTestServer(t)
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}

func TestServerRejectsOversizedRequest(t *testing.T) {
	socket, _ := startTestServer(t)
	connection, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_, _ = connection.Write(bytes.Repeat([]byte("a"), 2<<20))
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 4096)
	count, err := connection.Read(buffer)
	if err != nil {
		t.Fatalf("read frame-limit response: %v", err)
	}
	if !strings.Contains(string(buffer[:count]), "frame_too_large") {
		t.Fatalf("response = %q, want frame_too_large", buffer[:count])
	}
}

func TestServerRejectsMalformedRequest(t *testing.T) {
	socket, _ := startTestServer(t)
	connection, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := connection.Write([]byte("not-json\n")); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 4096)
	count, err := connection.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buffer[:count]), "invalid request") {
		t.Fatalf("response = %q", buffer[:count])
	}
}

func managedClientOptions(t *testing.T, dir string) ghostline.ManagedClientOptions {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("", "ghostline-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "ghostline.sock")
	return ghostline.ManagedClientOptions{
		Socket:       socket,
		Spawn:        []string{executable, "-test.run=TestServerHelperProcess"},
		ReadyTimeout: 5 * time.Second,
		Env: []string{
			"GHOSTLINE_HELPER=1",
			"GHOSTLINE_HELPER_DIR=" + dir,
			"GHOSTLINE_HELPER_SOCKET=" + socket,
		},
	}
}

func TestConnectManagedSpawnsServer(t *testing.T) {
	dir := t.TempDir()
	client, err := ghostline.ConnectManaged(context.Background(), managedClientOptions(t, dir))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "connect", Process: ghostline.ProcessSpec{Path: "sh", Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	status, err := session.Status(context.Background())
	if err != nil || !status.Alive {
		t.Fatal("session not alive after Connect")
	}
}

func TestConnectManagedReusesRunningServer(t *testing.T) {
	socket, _ := startTestServer(t)
	client, err := ghostline.ConnectManaged(context.Background(), ghostline.ManagedClientOptions{Socket: socket})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := ghostline.NewClient(socket).Check(context.Background()); err != nil {
		t.Fatal("Close stopped a server it did not spawn")
	}
}

func TestEnsureRespawnsServer(t *testing.T) {
	dir := t.TempDir()
	client, err := ghostline.ConnectManaged(context.Background(), managedClientOptions(t, dir))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	_ = client.Close()
	if err := client.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "ensure", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	}); err != nil {
		t.Fatalf("Start after Ensure: %v", err)
	}
}

func TestLimitedRecoveryRetriesIdempotentCalls(t *testing.T) {
	dir := t.TempDir()
	client, err := ghostline.ConnectManaged(context.Background(), managedClientOptions(t, dir))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "recover", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = client.Close()
	names, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List after recovery: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("recovered server should be empty, got %v", names)
	}
}

func TestLimitedRecoveryDoesNotRetryInput(t *testing.T) {
	dir := t.TempDir()
	options := managedClientOptions(t, dir)
	client, err := ghostline.ConnectManaged(context.Background(), options)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name: "no-retry", Process: ghostline.ProcessSpec{Path: "sleep", Args: []string{"30"}, Directory: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = client.Close()
	if err := session.WriteInput(context.Background(), []byte("x")); err == nil {
		t.Fatal("Input succeeded after server shutdown")
	}
	if err := ghostline.NewClient(options.Socket).Check(context.Background()); err == nil {
		t.Fatal("non-idempotent call spawned the server")
	}
}

func TestConnectManagedFailsFastWhenSpawnExits(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "ghostline-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "ghostline.sock")
	started := time.Now()
	client, err := ghostline.ConnectManaged(context.Background(), ghostline.ManagedClientOptions{
		Socket:       socket,
		Spawn:        []string{"sh", "-c", "echo boom >&2; exit 1"},
		ReadyTimeout: 30 * time.Second,
	})
	if err == nil {
		_ = client.Close()
		t.Fatal("Connect succeeded with a failing spawn")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should include spawn output, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("spawn failure took %v, want a fast failure", elapsed)
	}
}

func TestConnectManagedRejectsInvalidEnv(t *testing.T) {
	_, err := ghostline.ConnectManaged(context.Background(), ghostline.ManagedClientOptions{
		Socket: filepath.Join(t.TempDir(), "ghostline.sock"),
		Spawn:  []string{"sh", "-c", "exit 0"},
		Env:    []string{"NO_EQUALS"},
	})
	if err == nil {
		t.Fatal("Connect succeeded with an invalid environment entry")
	}
}

func TestConnectManagedConcurrentSpawnsOneServer(t *testing.T) {
	dir := t.TempDir()
	options := managedClientOptions(t, dir)
	const callers = 8
	type result struct {
		client *ghostline.Client
		err    error
	}
	results := make([]result, callers)
	var group sync.WaitGroup
	for index := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			client, err := ghostline.ConnectManaged(context.Background(), options)
			results[index] = result{client: client, err: err}
		}()
	}
	group.Wait()
	for index, result := range results {
		if result.err != nil {
			t.Fatalf("Connect %d: %v", index, result.err)
		}
		if err := result.client.Check(context.Background()); err != nil {
			t.Fatalf("client %d Check: %v", index, err)
		}
	}
	for _, result := range results {
		_ = result.client.Close()
	}
	if err := ghostline.NewClient(options.Socket).Check(context.Background()); err == nil {
		t.Fatal("server still running after every client closed")
	}
}

func TestManagedClientConcurrentEnsurePIDAndClose(t *testing.T) {
	dir := t.TempDir()
	client, err := ghostline.ConnectManaged(context.Background(), managedClientOptions(t, dir))
	if err != nil {
		t.Fatalf("ConnectManaged: %v", err)
	}
	const callers = 16
	var group sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			errs <- client.Ensure(context.Background())
			_ = client.PID()
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Ensure: %v", err)
		}
	}

	closeErrs := make(chan error, callers)
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			closeErrs <- client.Close()
		}()
	}
	group.Wait()
	close(closeErrs)
	for err := range closeErrs {
		if err != nil {
			t.Fatalf("concurrent Close: %v", err)
		}
	}
	if pid := client.PID(); pid != 0 {
		t.Fatalf("PID after Close = %d, want 0", pid)
	}
}
