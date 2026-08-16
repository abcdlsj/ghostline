package ghostline_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abcdlsj/ghostline"
)

func TestMetadataDisabledByDefault(t *testing.T) {
	hub, err := ghostline.New(ghostline.Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "metadata-off",
		Directory: t.TempDir(),
		Command:   "sh",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	provider, ok := session.(ghostline.MetadataProvider)
	if !ok {
		t.Fatal("session does not implement MetadataProvider")
	}
	metadata, err := provider.Metadata(context.Background())
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if metadata != (ghostline.SessionMetadata{}) {
		t.Fatalf("Metadata = %+v, want empty when probing is disabled", metadata)
	}
}

func TestMetadataEnabledReportsForegroundProcess(t *testing.T) {
	directory := t.TempDir()
	hub, err := ghostline.New(ghostline.Options{
		OutputDir:       t.TempDir(),
		ProbeForeground: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	session, err := hub.Start(context.Background(), ghostline.SessionOptions{
		Name:      "metadata-on",
		Directory: directory,
		Command:   "sh",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForForegroundMetadata(t, session, directory, "sleep")
}

func TestRemoteMetadataEnabledReportsForegroundProcess(t *testing.T) {
	_, client := startTestServerWithOptions(t, ghostline.Options{
		OutputDir:       t.TempDir(),
		ProbeForeground: true,
	})
	directory := t.TempDir()
	session, err := client.Start(context.Background(), ghostline.SessionOptions{
		Name:      "metadata-remote",
		Directory: directory,
		Command:   "sh",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	waitForForegroundMetadata(t, session, directory, "sleep")
}

func waitForForegroundMetadata(t *testing.T, session ghostline.Session, directory, processNeedle string) {
	t.Helper()
	provider, ok := session.(ghostline.MetadataProvider)
	if !ok {
		t.Fatal("session does not implement MetadataProvider")
	}
	ctx := context.Background()
	if err := session.Input(ctx, []byte("cd "+directory+" && exec sleep 30\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var last ghostline.SessionMetadata
	var lastErr error
	wantDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	for time.Now().Before(deadline) {
		metadata, err := provider.Metadata(ctx)
		last, lastErr = metadata, err
		if err == nil && strings.Contains(metadata.Process, processNeedle) {
			if gotDirectory, resolveErr := filepath.EvalSymlinks(metadata.Directory); resolveErr == nil && gotDirectory == wantDirectory {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("metadata did not converge: last=%+v err=%v", last, lastErr)
}
