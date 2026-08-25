package ghostline_test

import (
	"bufio"
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abcdlsj/ghostline"
)

type inputScalePTY struct {
	session *ghostline.Session
	reader  *ghostline.OutputReader
	lines   *bufio.Reader
}

func TestDaemonWarrenCodexTUIInputScale(t *testing.T) {
	requireScaleTest(t)

	const (
		sessionCount  = 256
		inputRounds   = 32
		inputInFlight = 32
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, client := startTestServerWithOptions(t, ghostline.Options{
		OutputDir:            t.TempDir(),
		VTScrollbackMaxBytes: 64 << 10,
	})

	ptys := make([]inputScalePTY, 0, sessionCount)
	t.Cleanup(func() {
		for _, pty := range ptys {
			_ = pty.reader.Close()
		}
	})

	for index := 0; index < sessionCount; index++ {
		session, err := client.Start(ctx, ghostline.SessionOptions{
			Name: fmt.Sprintf("warren-codex-%03d", index),
			Process: ghostline.Shell(
				"stty -echo; printf 'READY\\n'; " +
					"while IFS= read -r frame; do printf 'ACK:%s\\n' \"$frame\"; done",
			),
		})
		if err != nil {
			t.Fatalf("start PTY %d/%d: %v", index+1, sessionCount, err)
		}
		reader, err := session.Output(ctx, ghostline.Cursor{})
		if err != nil {
			t.Fatalf("open output %d/%d: %v", index+1, sessionCount, err)
		}
		lines := bufio.NewReaderSize(reader, 128)
		ready, err := readInputScaleLine(lines)
		if err != nil {
			t.Fatalf("read ready PTY %d/%d: %v", index+1, sessionCount, err)
		}
		if ready != "READY" {
			t.Fatalf("ready PTY %d/%d = %q, want READY", index+1, sessionCount, ready)
		}
		ptys = append(ptys, inputScalePTY{session: session, reader: reader, lines: lines})
	}

	totalFrames := sessionCount * inputRounds
	queueLatencies := make(chan time.Duration, totalFrames)
	writeLatencies := make(chan time.Duration, totalFrames)
	ackLatencies := make(chan time.Duration, totalFrames)
	inputSlots := make(chan struct{}, inputInFlight)
	var failureMu sync.Mutex
	var failure error
	recordFailure := func(err error) {
		if err == nil {
			return
		}
		failureMu.Lock()
		if failure == nil {
			failure = err
			cancel()
		}
		failureMu.Unlock()
	}

	beforeGoroutines := runtime.NumGoroutine()
	beforeFDs, haveFDCount := openFileDescriptorCount()
	phaseStarted := time.Now()
	var workers sync.WaitGroup
	workers.Add(sessionCount)
	for index, pty := range ptys {
		go func(index int, pty inputScalePTY) {
			defer workers.Done()
			for round := 0; round < inputRounds; round++ {
				token := fmt.Sprintf("W%03d-R%03d", index, round)
				frame := token + "\n"
				queued := time.Now()
				select {
				case inputSlots <- struct{}{}:
				case <-ctx.Done():
					recordFailure(fmt.Errorf("acquire input slot for frame %s: %w", token, ctx.Err()))
					return
				}
				writeStarted := time.Now()
				queueLatencies <- writeStarted.Sub(queued)
				if err := pty.session.WriteInput(ctx, []byte(frame)); err != nil {
					<-inputSlots
					recordFailure(fmt.Errorf("write frame %s: %w", token, err))
					return
				}
				writeLatencies <- time.Since(writeStarted)
				<-inputSlots
				line, err := readInputScaleLine(pty.lines)
				if err != nil {
					recordFailure(fmt.Errorf("read acknowledgement %s: %w", token, err))
					return
				}
				expected := "ACK:" + token
				if line != expected {
					recordFailure(fmt.Errorf("acknowledgement for frame %s = %q, want %q (loss or reordering)", token, line, expected))
					return
				}
				ackLatencies <- time.Since(queued)
			}
		}(index, pty)
	}
	workers.Wait()
	phaseElapsed := time.Since(phaseStarted)
	close(queueLatencies)
	close(writeLatencies)
	close(ackLatencies)

	failureMu.Lock()
	firstFailure := failure
	failureMu.Unlock()
	if firstFailure != nil {
		t.Fatal(firstFailure)
	}

	queueSamples := make([]time.Duration, 0, totalFrames)
	for latency := range queueLatencies {
		queueSamples = append(queueSamples, latency)
	}
	writeSamples := make([]time.Duration, 0, totalFrames)
	for latency := range writeLatencies {
		writeSamples = append(writeSamples, latency)
	}
	ackSamples := make([]time.Duration, 0, totalFrames)
	for latency := range ackLatencies {
		ackSamples = append(ackSamples, latency)
	}
	if len(queueSamples) != totalFrames || len(writeSamples) != totalFrames || len(ackSamples) != totalFrames {
		t.Fatalf(
			"completed frames = queue:%d write:%d ack:%d, want %d each; possible silent loss",
			len(queueSamples), len(writeSamples), len(ackSamples), totalFrames,
		)
	}

	resourceDeadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > beforeGoroutines+16 && time.Now().Before(resourceDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - beforeGoroutines; delta > 16 {
		t.Fatalf("goroutines after sustained input = %+d, want at most +16", delta)
	}
	fdDelta := 0
	if haveFDCount {
		for {
			afterFDs, _ := openFileDescriptorCount()
			fdDelta = afterFDs - beforeFDs
			if fdDelta <= 16 || time.Now().After(resourceDeadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if fdDelta > 16 {
			t.Fatalf("file descriptors after sustained input = %+d, want at most +16", fdDelta)
		}
	}

	totalInputBytes := int64(0)
	for index := 0; index < sessionCount; index++ {
		for round := 0; round < inputRounds; round++ {
			totalInputBytes += int64(len(fmt.Sprintf("W%03d-R%03d\n", index, round)))
		}
	}
	throughput := float64(totalInputBytes) / phaseElapsed.Seconds()
	t.Logf(
		"Warren Codex TUI input scale: ptys=%d rounds=%d frames=%d bytes=%d elapsed=%s aggregate=%.0f frames/s %.1f KiB/s goroutines=%+d fds=%+d",
		sessionCount,
		inputRounds,
		totalFrames,
		totalInputBytes,
		phaseElapsed.Round(time.Millisecond),
		float64(totalFrames)/phaseElapsed.Seconds(),
		throughput/1024,
		runtime.NumGoroutine()-beforeGoroutines,
		fdDelta,
	)
	t.Logf("input queue latency: %s", formatLatencyPercentiles(queueSamples))
	t.Logf("WriteInput RPC latency: %s", formatLatencyPercentiles(writeSamples))
	t.Logf("input-to-output acknowledgement latency: %s", formatLatencyPercentiles(ackSamples))
}

func readInputScaleLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}
