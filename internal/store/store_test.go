package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"codex-control/relay-go/internal/protocol"
	_ "modernc.org/sqlite"
)

func TestOpenMigratesLegacyTurnIDToNullable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "relay.sqlite")
	legacy, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE pending_requests(device_id TEXT NOT NULL,thread_id TEXT NOT NULL,request_id TEXT NOT NULL,turn_id TEXT NOT NULL,kind TEXT NOT NULL,payload_json TEXT NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(device_id,thread_id,request_id));CREATE INDEX idx_requests_device ON pending_requests(device_id,updated_at DESC);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := Open(file, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows, err := database.db.Query("PRAGMA table_info(pending_requests)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "turn_id" {
			found = true
			if notNull != 0 {
				t.Fatal("turn_id remained NOT NULL")
			}
		}
	}
	if !found {
		t.Fatal("turn_id column missing")
	}
	var version int
	if err := database.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("unexpected schema version %d", version)
	}
}

func TestPairingCodeIsSingleUse(t *testing.T) {
	database, err := Open(":memory:", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	pair, err := database.CreatePairingCode(ctx, "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := database.Enroll(ctx, pair.Code, "PC", nil)
	if err != nil {
		t.Fatal(err)
	}
	if enrollment.DeviceID == "" || enrollment.Token == "" {
		t.Fatal("missing enrollment credentials")
	}
	if _, err := database.Enroll(ctx, pair.Code, "PC2", nil); err != ErrPairingCodeInvalid {
		t.Fatalf("expected single-use rejection, got %v", err)
	}
	deviceID, ok, err := database.AuthenticateAgent(ctx, enrollment.Token)
	if err != nil || !ok || deviceID != enrollment.DeviceID {
		t.Fatalf("agent authentication failed: %s %v %v", deviceID, ok, err)
	}
}

func TestJSONColumnsUseSQLiteTextStorage(t *testing.T) {
	database, err := Open(":memory:", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	pair, err := database.CreatePairingCode(ctx, "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := database.Enroll(ctx, pair.Code, "PC", nil)
	if err != nil {
		t.Fatal(err)
	}
	state := protocol.AgentState{Tasks: []protocol.Task{}, Requests: []protocol.PendingRequest{}}
	requests := []protocol.ValidatedRequest{{ThreadID: "t", RequestID: "r", Kind: "mcp", Payload: map[string]any{"mode": "form"}, UpdatedAt: 1}}
	if err := database.Sync(ctx, enrollment.DeviceID, state, requests); err != nil {
		t.Fatal(err)
	}
	command, _, err := protocol.DecodeCommand([]byte(`{"deviceId":"` + enrollment.DeviceID + `","kind":"send_message","threadId":"t","text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	commandID, _, err := database.CreateCommand(ctx, enrollment.DeviceID, command, "text-storage-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ClaimCommands(ctx, enrollment.DeviceID, 20); err != nil {
		t.Fatal(err)
	}
	if err := database.Acknowledge(ctx, enrollment.DeviceID, commandID, true, "done"); err != nil {
		t.Fatal(err)
	}

	var payloadType, bodyType, resultType string
	if err := database.db.QueryRow("SELECT typeof(payload_json) FROM pending_requests LIMIT 1").Scan(&payloadType); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow("SELECT typeof(body_json),typeof(result_json) FROM commands LIMIT 1").Scan(&bodyType, &resultType); err != nil {
		t.Fatal(err)
	}
	if payloadType != "text" || bodyType != "text" || resultType != "text" {
		t.Fatalf("JSON columns must use TEXT storage: payload=%s body=%s result=%s", payloadType, bodyType, resultType)
	}
}
