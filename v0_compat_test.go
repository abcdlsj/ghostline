package ghostline

import (
	"context"
	"encoding/json"
	"testing"
)

func TestV0CompatibilityBridgeVersions(t *testing.T) {
	if ProtocolVersion != "0.8.0" {
		t.Fatalf("ProtocolVersion = %q, want 0.8.0", ProtocolVersion)
	}
	if V1HandoffProtocolVersion != "ghostline-v0-to-v1-1" {
		t.Fatalf("V1HandoffProtocolVersion = %q, want ghostline-v0-to-v1-1", V1HandoffProtocolVersion)
	}
}

func TestAdminListPublishesV1HandoffMetadata(t *testing.T) {
	server, socket := startMigrateServer(t, t.TempDir(), "handoff")
	session, err := server.hub.Start(context.Background(), SessionOptions{Name: "handoff", Command: "sh"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Input(context.Background(), []byte("printf handoff-output\r")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitSessionOutput(t, session, "handoff-output")

	connection, err := dialAdmin(context.Background(), socket+".admin")
	if err != nil {
		t.Fatalf("dial admin: %v", err)
	}
	defer connection.Close()
	client := &adminClient{transport: newAdminTransport(connection), nextID: 1}
	var result adminListResult
	if err := client.call(context.Background(), adminMethodList, nil, &result); err != nil {
		t.Fatalf("admin list: %v", err)
	}
	if result.Version != ProtocolVersion {
		t.Fatalf("list version = %q, want %q", result.Version, ProtocolVersion)
	}
	if result.HandoffVersion != V1HandoffProtocolVersion {
		t.Fatalf("handoff version = %q, want %q", result.HandoffVersion, V1HandoffProtocolVersion)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(result.Sessions))
	}
	meta := result.Sessions[0]
	if meta.SpoolPath == "" {
		t.Fatal("spool path is empty")
	}
	if meta.SpoolFormat != v0SpoolFormat {
		t.Fatalf("spool format = %q, want %q", meta.SpoolFormat, v0SpoolFormat)
	}
	if meta.SpoolSize <= 0 {
		t.Fatalf("spool size = %d, want positive size", meta.SpoolSize)
	}
}

func TestLegacyMigrationClientIgnoresHandoffFields(t *testing.T) {
	raw, err := json.Marshal(adminListResult{
		Version:        ProtocolVersion,
		HandoffVersion: V1HandoffProtocolVersion,
		Sessions: []sessionMeta{{
			Name:        "legacy",
			SpoolPath:   "/tmp/legacy.out",
			SpoolSize:   42,
			SpoolFormat: v0SpoolFormat,
		}},
	})
	if err != nil {
		t.Fatalf("marshal handoff response: %v", err)
	}
	var legacy struct {
		Sessions []struct {
			Name string `json:"name"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if len(legacy.Sessions) != 1 || legacy.Sessions[0].Name != "legacy" {
		t.Fatalf("legacy sessions = %#v, want one legacy session", legacy.Sessions)
	}
}
