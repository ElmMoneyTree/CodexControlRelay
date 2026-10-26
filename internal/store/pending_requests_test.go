package store

import (
	"context"
	"testing"
	"time"

	"codex-control/relay-go/internal/protocol"
)

func TestSyncClearsResolvedRequestsOnlyForUpdatedDevice(t *testing.T) {
	database, err := Open(":memory:", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	devices := make([]Enrollment, 0, 2)
	for _, name := range []string{"First computer", "Second computer"} {
		pair, err := database.CreatePairingCode(ctx, "owner", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		device, err := database.Enroll(ctx, pair.Code, name, nil)
		if err != nil {
			t.Fatal(err)
		}
		devices = append(devices, device)
		state := protocol.AgentState{Tasks: []protocol.Task{{ThreadID: "thread", Title: name, Status: "waiting_approval", UpdatedAt: 1}}}
		requests := []protocol.ValidatedRequest{{ThreadID: "thread", RequestID: "approval", Kind: "command", Payload: map[string]any{"command": "go test ./..."}, UpdatedAt: 1}}
		if err := database.Sync(ctx, device.DeviceID, state, requests); err != nil {
			t.Fatal(err)
		}
	}
	state, err := database.State(ctx, "owner", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Requests) != 2 {
		t.Fatalf("both computers should have a pending request: %#v", state.Requests)
	}

	resolved := protocol.AgentState{Tasks: []protocol.Task{{ThreadID: "thread", Title: "First computer", Status: "completed", UpdatedAt: 2}}, Requests: []protocol.PendingRequest{}}
	if err := database.Sync(ctx, devices[0].DeviceID, resolved, []protocol.ValidatedRequest{}); err != nil {
		t.Fatal(err)
	}
	state, err = database.State(ctx, "owner", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Requests) != 1 || state.Requests[0].DeviceID != devices[1].DeviceID || state.Requests[0].RequestID != "approval" {
		t.Fatalf("resolved request must disappear while the other computer's request remains: %#v", state.Requests)
	}
	if len(state.Tasks) != 2 {
		t.Fatalf("both computers' tasks should remain: %#v", state.Tasks)
	}
	for _, task := range state.Tasks {
		want := "waiting_approval"
		if task.DeviceID == devices[0].DeviceID {
			want = "completed"
		}
		if task.Status != want {
			t.Fatalf("task status for %s: got %q, want %q", task.DeviceID, task.Status, want)
		}
	}
}
