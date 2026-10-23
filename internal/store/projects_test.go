package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"codex-control/relay-go/internal/protocol"
)

func TestCreateThreadQueueResultAndPromptPrivacy(t *testing.T) {
	db, err := Open(":memory:", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	pair, _ := db.CreatePairingCode(ctx, "owner", time.Minute)
	device, err := db.Enroll(ctx, pair.Code, "Computer", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"codex", "chatgpt"} {
		for _, ok := range []bool{true, false} {
			data, _ := json.Marshal(map[string]any{"deviceId": device.DeviceID, "kind": "create_thread", "backend": backend, "projectId": "project", "text": "private initial prompt"})
			command, _, err := protocol.DecodeCommand(data)
			if err != nil {
				t.Fatal(err)
			}
			id, _, err := db.CreateCommand(ctx, device.DeviceID, command, "")
			if err != nil {
				t.Fatal(err)
			}
			pending, err := db.ClaimCommands(ctx, device.DeviceID, 20)
			if err != nil || len(pending) != 1 || pending[0]["text"] != "private initial prompt" || pending[0]["backend"] != backend {
				t.Fatal(pending, err)
			}
			if ok {
				if err := db.Acknowledge(ctx, device.DeviceID, id, true, map[string]any{"threadId": "created", "backend": "mismatch"}); err == nil {
					t.Fatal("invalid creation identity accepted")
				}
			}
			if err := db.Acknowledge(ctx, device.DeviceID, id, ok, map[string]any{"threadId": "created", "backend": backend, "projectId": "project", "response": "private turn contents", "text": "private initial prompt"}); err != nil {
				t.Fatal(err)
			}
			var prompt *string
			var resultJSON, threadID string
			if err := db.db.QueryRow("SELECT json_extract(body_json,'$.text'),result_json,thread_id FROM commands WHERE id=?", id).Scan(&prompt, &resultJSON, &threadID); err != nil || prompt != nil {
				t.Fatal("prompt retained", prompt, err)
			}
			var result map[string]any
			json.Unmarshal([]byte(resultJSON), &result)
			if result["response"] != nil || result["text"] != nil {
				t.Fatal("private result retained", result)
			}
			if ok && (threadID != "created" || result["threadId"] != "created" || result["backend"] != backend || result["projectId"] != "project") {
				t.Fatal("routing result lost", result, threadID)
			}
		}
	}
	backend := "chatgpt"
	if err := db.Sync(ctx, device.DeviceID, protocol.AgentState{Tasks: []protocol.Task{{ThreadID: "created", Backend: &backend, Title: "Title", Status: "idle", UpdatedAt: 1}}}, nil); err != nil {
		t.Fatal(err)
	}
	state, err := db.State(ctx, "owner", 0)
	if err != nil || len(state.Tasks) != 1 || state.Tasks[0].Backend == nil || *state.Tasks[0].Backend != backend {
		t.Fatal("task backend lost", state, err)
	}
}

func TestCreateThreadPromptErasedForExternalFailureTransitions(t *testing.T) {
	db, err := Open(":memory:", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	pair, _ := db.CreatePairingCode(ctx, "owner", time.Minute)
	device, _ := db.Enroll(ctx, pair.Code, "Computer", nil)
	data, _ := json.Marshal(map[string]any{"deviceId": device.DeviceID, "kind": "create_thread", "backend": "codex", "text": "private prompt"})
	command, _, _ := protocol.DecodeCommand(data)
	id, _, err := db.CreateCommand(ctx, device.DeviceID, command, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE commands SET state='failed' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	var prompt *string
	if err := db.db.QueryRow("SELECT json_extract(body_json,'$.text') FROM commands WHERE id=?", id).Scan(&prompt); err != nil || prompt != nil {
		t.Fatal("terminal command kept prompt", prompt, err)
	}
}

func TestTaskBackendMigratesExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE tasks(device_id TEXT NOT NULL,thread_id TEXT NOT NULL,title TEXT NOT NULL,project TEXT,status TEXT NOT NULL,summary TEXT,latest_message TEXT,updated_at INTEGER NOT NULL,PRIMARY KEY(device_id,thread_id))`); err != nil {
		t.Fatal(err)
	}
	legacy.Close()
	db, err := Open(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO tasks(device_id,thread_id,title,status,backend,updated_at) VALUES('d','t','Title','idle','chatgpt',1)`); err != nil {
		t.Fatal("backend column was not migrated", err)
	}
	db.Close()
	db, err = Open(path, time.Second)
	if err != nil {
		t.Fatal("migration was not idempotent", err)
	}
	db.Close()
}

func TestCreateThreadPendingIdentityIsSanitized(t *testing.T) {
	command := []byte(`{"backend":"chatgpt","text":"private prompt"}`)
	data, threadID, err := sanitizeCreatedThreadResult(true, []byte(`{"backend":"chatgpt","clientThreadId":"client-new-thread:pending","response":"private payload"}`), command)
	if err != nil || threadID != "" {
		t.Fatal(string(data), threadID, err)
	}
	var result map[string]any
	json.Unmarshal(data, &result)
	if result["clientThreadId"] != "client-new-thread:pending" || result["status"] != "starting" || result["submitted"] != true || result["threadId"] != nil || result["response"] != nil {
		t.Fatal("invalid pending routing result", result)
	}
	for _, value := range []string{
		`{"backend":"chatgpt"}`,
		`{"backend":"chatgpt","threadId":"real","clientThreadId":"client-new-thread:pending"}`,
		`{"backend":"codex","clientThreadId":"client-new-thread:pending"}`,
	} {
		if _, _, err := sanitizeCreatedThreadResult(true, []byte(value), command); err == nil {
			t.Fatal("invalid pending creation identity accepted", value)
		}
	}
}
