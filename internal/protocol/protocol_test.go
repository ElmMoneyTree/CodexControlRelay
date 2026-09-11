package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeAgentStateNormalizesRequestIDsAndTimestamps(t *testing.T) {
	state, requests, err := DecodeAgentState([]byte(`{"tasks":[{"threadId":" t1 ","title":"Title","project":null,"status":"active","summary":null,"latestMessage":null,"updatedAt":"12.9"}],"requests":[{"threadId":"t1","requestId":7,"turnId":null,"kind":"mcp","payload":{"mode":"form"},"updatedAt":"invalid"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if state.Tasks[0].ThreadID != "t1" || requests[0].RequestID != "7" || requests[0].TurnID != nil {
		t.Fatalf("unexpected normalization: %#v %#v", state.Tasks[0], requests[0])
	}
	if state.Tasks[0].UpdatedAt != 12 || requests[0].UpdatedAt <= 0 {
		t.Fatalf("timestamps were not normalized: %#v %#v", state.Tasks[0], requests[0])
	}
}

func TestDecodeCommandKinds(t *testing.T) {
	cases := []string{
		`{"deviceId":"d","kind":"send_message","threadId":"t","text":"hello"}`,
		`{"deviceId":"d","kind":"command_approval","threadId":"t","requestId":"r","turnId":"u","decision":"accept"}`,
		`{"deviceId":"d","kind":"file_approval","threadId":"t","requestId":"r","turnId":"u","decision":"decline"}`,
		`{"deviceId":"d","kind":"permissions_approval","threadId":"t","requestId":"r","turnId":"u","decision":"accept"}`,
		`{"deviceId":"d","kind":"user_input_response","threadId":"t","requestId":"r","turnId":"u","answers":{"q":["a"]}}`,
		`{"deviceId":"d","kind":"mcp_response","threadId":"t","requestId":"r","turnId":null,"action":"decline"}`,
		`{"deviceId":"d","kind":"mcp_response","threadId":"t","requestId":"r","action":"accept","content":{"field":"value"}}`,
	}
	for _, encoded := range cases {
		command, identity, err := DecodeCommand([]byte(encoded))
		if err != nil {
			t.Fatalf("%s: %v", encoded, err)
		}
		device, _ := SplitDeviceAndDedupe(identity)
		if device != "d" {
			t.Fatalf("wrong device: %s", device)
		}
		var body map[string]any
		if err := json.Unmarshal(command.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body["kind"] != command.Kind {
			t.Fatalf("normalized body lost kind: %#v", body)
		}
	}
}

func TestDecodeCommandRejectsExpandedPermissionAndTrailingJSON(t *testing.T) {
	_, _, err := DecodeCommand([]byte(`{"deviceId":"d","kind":"permissions_approval","threadId":"t","requestId":"r","turnId":"u","decision":"accept_session"}`))
	if err == nil || !strings.Contains(err.Error(), "permission approval") {
		t.Fatalf("expected permission rejection, got %v", err)
	}
	_, _, err = DecodeCommand([]byte(`{"deviceId":"d","kind":"send_message","threadId":"t","text":"x"} {}`))
	if err == nil {
		t.Fatal("expected trailing JSON rejection")
	}
}

func TestDecodeStateBounds(t *testing.T) {
	_, _, err := DecodeAgentState([]byte(`{"tasks":null,"requests":[]}`))
	if err == nil {
		t.Fatal("null tasks should fail")
	}
	large := strings.Repeat("x", 100001)
	encoded, _ := json.Marshal(map[string]any{"tasks": []any{}, "requests": []any{map[string]any{"threadId": "t", "requestId": "r", "kind": "mcp", "payload": map[string]any{"x": large}, "updatedAt": 1}}})
	_, _, err = DecodeAgentState(encoded)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected payload bound, got %v", err)
	}
}

func TestUTF16StringLimitsAndAnswerNormalization(t *testing.T) {
	stateInput, _ := json.Marshal(map[string]any{
		"tasks": []any{map[string]any{
			"threadId": "t", "title": strings.Repeat("😀", 250),
			"status": "active", "updatedAt": 1,
		}},
		"requests": []any{},
	})
	if _, _, err := DecodeAgentState(stateInput); err != nil {
		t.Fatalf("500 UTF-16 code units should pass: %v", err)
	}
	stateInput, _ = json.Marshal(map[string]any{
		"tasks": []any{map[string]any{
			"threadId": "t", "title": strings.Repeat("😀", 251),
			"status": "active", "updatedAt": 1,
		}},
		"requests": []any{},
	})
	if _, _, err := DecodeAgentState(stateInput); err == nil {
		t.Fatal("502 UTF-16 code units should fail")
	}

	command, _, err := DecodeCommand([]byte(`{"deviceId":"d","kind":"user_input_response","threadId":"t","requestId":"r","turnId":"u","answers":{" q ":[" answer "]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if values := command.Answers["q"]; len(values) != 1 || values[0] != "answer" {
		t.Fatalf("answers were not normalized: %#v", command.Answers)
	}
}
