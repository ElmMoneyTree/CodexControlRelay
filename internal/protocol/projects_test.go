package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCreateThreadCommandSupportsBothBackendsAndOptionalProject(t *testing.T) {
	for _, backend := range []string{"codex", "chatgpt"} {
		for _, project := range []any{nil, "project-1"} {
			encoded, _ := json.Marshal(map[string]any{"deviceId": "d", "kind": "create_thread", "backend": backend, "projectId": project, "text": " 初始消息 ", "dedupeKey": "retry-key"})
			command, identity, err := DecodeCommand(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if command.ThreadID != "" || command.Backend == nil || *command.Backend != backend || *command.Text != "初始消息" {
				t.Fatalf("invalid command: %#v", command)
			}
			device, dedupe := SplitDeviceAndDedupe(identity)
			if device != "d" || dedupe != "retry-key" {
				t.Fatal(identity)
			}
			var body map[string]any
			json.Unmarshal(command.Body, &body)
			if _, exists := body["threadId"]; exists || body["backend"] != backend || body["projectId"] != project {
				t.Fatalf("invalid wire body: %#v", body)
			}
		}
	}
}

func TestCreateThreadRejectsMissingOrConflictingFields(t *testing.T) {
	for _, input := range []string{
		`{"deviceId":"d","kind":"create_thread","text":"hello"}`,
		`{"deviceId":"d","kind":"create_thread","backend":"other","text":"hello"}`,
		`{"deviceId":"d","kind":"create_thread","backend":"codex","text":" "}`,
		`{"deviceId":"d","kind":"create_thread","backend":"codex","text":"hello","threadId":"existing"}`,
		`{"deviceId":"d","kind":"create_thread","backend":"codex","text":"hello","projectId":" "}`,
		`{"deviceId":"d","kind":"create_thread","backend":"codex","text":"hello","content":{"text":"extra private text"}}`,
	} {
		if _, _, err := DecodeCommand([]byte(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	command, _, err := DecodeCommand([]byte(`{"deviceId":"d","kind":"send_message","threadId":"t","backend":"chatgpt","text":"reply"}`))
	if err != nil || !strings.Contains(string(command.Body), `"backend":"chatgpt"`) {
		t.Fatal("reply backend lost", string(command.Body), err)
	}
}

func TestProjectCatalogValidation(t *testing.T) {
	catalog, err := DecodeProjectCatalog([]byte(`{"projects":[{"id":" p ","name":" 项目 ","kind":"codex"},{"id":"p","name":"Cloud","kind":"chatgpt"}],"backends":["codex","chatgpt"]}`))
	if err != nil || catalog.Projects[0].ID != "p" || catalog.Projects[0].Name != "项目" {
		t.Fatal(catalog, err)
	}
	for _, input := range []string{
		`{"projects":null,"backends":["codex"]}`,
		`{"projects":[],"backends":["unsupported"]}`,
		`{"projects":[],"backends":["codex","codex"]}`,
		`{"projects":[{"id":"p","name":"Project","kind":"chatgpt"}],"backends":["codex"]}`,
		`{"projects":[{"id":"p","name":"One","kind":"codex"},{"id":"p","name":"Two","kind":"codex"}],"backends":["codex"]}`,
		`{"projects":[],"backends":["codex"]} {}`,
	} {
		if _, err := DecodeProjectCatalog([]byte(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	catalog, err = DecodeProjectCatalog([]byte(`{"error":"private desktop failure contents"}`))
	if err != nil || strings.Contains(catalog.Error, "private") {
		t.Fatal("desktop error was not sanitized", catalog, err)
	}
}

func TestAgentStateBackendValidation(t *testing.T) {
	state, _, err := DecodeAgentState([]byte(`{"tasks":[{"threadId":"t","title":"Title","status":"idle","backend":"chatgpt"}],"requests":[]}`))
	if err != nil || state.Tasks[0].Backend == nil || *state.Tasks[0].Backend != "chatgpt" {
		t.Fatal(state, err)
	}
	if _, _, err := DecodeAgentState([]byte(`{"tasks":[{"threadId":"t","title":"Title","status":"idle","backend":"unknown"}],"requests":[]}`)); err == nil {
		t.Fatal("unknown task backend accepted")
	}
}
