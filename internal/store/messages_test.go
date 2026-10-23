package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"codex-control/relay-go/internal/protocol"
)

func TestConversationStorageRemovedAndReadAuthorizationRemainsScoped(t *testing.T) {
	file := filepath.Join(t.TempDir(), "relay.sqlite")
	old, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE conversation_messages(id INTEGER PRIMARY KEY,text TEXT); INSERT INTO conversation_messages VALUES(1,'old transcript')`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := Open(file, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='conversation_messages'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("server history table retained: %d %v", count, err)
	}
	ctx := context.Background()
	pair, _ := db.CreatePairingCode(ctx, "owner", time.Minute)
	device, err := db.Enroll(ctx, pair.Code, "PC", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Sync(ctx, device.DeviceID, protocol.AgentState{Tasks: []protocol.Task{{ThreadID: "thread", Title: "Task", Status: "completed", UpdatedAt: 1}}}, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.CanReadConversation(ctx, "owner", device.DeviceID, "thread"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := db.CanReadConversation(ctx, "other", device.DeviceID, "thread"); ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := db.Sync(ctx, device.DeviceID, protocol.AgentState{}, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.CanReadConversation(ctx, "owner", device.DeviceID, "thread"); ok || err != nil {
		t.Fatal(ok, err)
	}
}

func TestRelayKeepsRoutingStateWithoutReplyBodiesAndErasesDeliveredInputs(t *testing.T) {
	db, err := Open(":memory:", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	pair, _ := db.CreatePairingCode(ctx, "owner", time.Minute)
	device, err := db.Enroll(ctx, pair.Code, "PC", nil)
	if err != nil {
		t.Fatal(err)
	}
	text := "conversation text"
	if err := db.Sync(ctx, device.DeviceID, protocol.AgentState{Tasks: []protocol.Task{{ThreadID: "t", Title: "Title", Status: "idle", Summary: &text, LatestMessage: &text, UpdatedAt: 1}}}, nil); err != nil {
		t.Fatal(err)
	}
	state, err := db.State(ctx, "owner", 0)
	if err != nil || state.Tasks[0].LatestMessage != nil || state.Tasks[0].Summary != nil {
		t.Fatalf("reply body persisted: %#v %v", state.Tasks, err)
	}
	for _, ok := range []bool{true, false} {
		command, _, err := protocol.DecodeCommand([]byte(`{"deviceId":"` + device.DeviceID + `","kind":"send_message","threadId":"t","text":"private user input"}`))
		if err != nil {
			t.Fatal(err)
		}
		id, _, err := db.CreateCommand(ctx, device.DeviceID, command, "")
		if err != nil {
			t.Fatal(err)
		}
		pending, err := db.ClaimCommands(ctx, device.DeviceID, 20)
		if err != nil || len(pending) != 1 || pending[0]["text"] != "private user input" {
			t.Fatal("pending message must still be deliverable", pending, err)
		}
		if err := db.Acknowledge(ctx, device.DeviceID, id, ok, map[string]any{"submitted": ok, "response": "turn payload"}); err != nil {
			t.Fatal(err)
		}
		var savedText *string
		if err := db.db.QueryRow("SELECT json_extract(body_json,'$.text') FROM commands WHERE id=?", id).Scan(&savedText); err != nil || savedText != nil {
			t.Fatal("delivered message retained", savedText, err)
		}
		{
			var response *string
			if err := db.db.QueryRow("SELECT json_extract(result_json,'$.response') FROM commands WHERE id=?", id).Scan(&response); err != nil || response != nil {
				t.Fatal("turn payload retained", response, err)
			}
		}
	}
}
