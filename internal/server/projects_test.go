package server

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

type creationAPICall func(string, string, any, map[string]string) (int, map[string]any)

func setupCreationAPI(t *testing.T) (creationAPICall, string, string, map[string]string, map[string]string) {
	t.Helper()
	_, call := setup(t)
	control := map[string]string{"Authorization": "Bearer " + controlToken}
	status, account := call(http.MethodPost, "/control/accounts", map[string]any{"email": "creation@example.com", "displayName": "Creation", "password": "correct horse battery staple"}, control)
	if status != http.StatusCreated {
		t.Fatalf("account: %d %#v", status, account)
	}
	workspace := account["workspaces"].([]any)[0].(map[string]any)["id"].(string)
	controller := map[string]string{"Authorization": "Bearer " + account["token"].(string)}
	_, pair := call(http.MethodPost, "/control/pairing-codes", map[string]any{"ownerId": workspace}, control)
	_, enrolled := call(http.MethodPost, "/agent/enroll", map[string]any{"code": pair["code"], "name": "Computer"}, nil)
	agent := map[string]string{"Authorization": "Bearer " + enrolled["token"].(string)}
	return call, workspace, enrolled["deviceId"].(string), controller, agent
}

func TestProjectCatalogIsForwardedToTheSelectedDeviceOnly(t *testing.T) {
	call, workspace, deviceID, controller, agent := setupCreationAPI(t)
	route := "/v1/projects?workspaceId=" + url.QueryEscape(workspace) + "&deviceId=" + url.QueryEscape(deviceID)
	if status, _ := call(http.MethodGet, route, nil, nil); status != http.StatusUnauthorized {
		t.Fatal("anonymous project read", status)
	}
	if status, _ := call(http.MethodGet, "/v1/projects?workspaceId=other&deviceId="+deviceID, nil, controller); status != http.StatusForbidden {
		t.Fatal("cross workspace project read", status)
	}
	if status, _ := call(http.MethodGet, "/v1/projects?workspaceId="+workspace+"&deviceId=other", nil, controller); status != http.StatusForbidden {
		t.Fatal("cross device project read", status)
	}
	control := map[string]string{"Authorization": "Bearer " + controlToken}
	_, pair := call(http.MethodPost, "/control/pairing-codes", map[string]any{"ownerId": "other"}, control)
	_, other := call(http.MethodPost, "/agent/enroll", map[string]any{"code": pair["code"], "name": "Other computer"}, nil)
	otherAgent := map[string]string{"Authorization": "Bearer " + other["token"].(string)}
	type answer struct {
		status int
		body   map[string]any
	}
	done := make(chan answer, 1)
	go func() { status, body := call(http.MethodGet, route, nil, controller); done <- answer{status, body} }()
	var request map[string]any
	for i := 0; i < 50; i++ {
		_, pending := call(http.MethodGet, "/agent/project-requests", nil, agent)
		requests := pending["requests"].([]any)
		if len(requests) != 0 {
			request = requests[0].(map[string]any)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if request == nil {
		t.Fatal("phone request not forwarded")
	}
	_, pending := call(http.MethodGet, "/agent/project-requests", nil, otherAgent)
	if len(pending["requests"].([]any)) != 0 {
		t.Fatal("project request leaked to other computer")
	}
	responsePath := "/agent/project-responses/" + request["id"].(string)
	body := map[string]any{"projects": []any{map[string]any{"id": "local", "name": "Local project", "kind": "codex"}, map[string]any{"id": "cloud", "name": "Cloud project", "kind": "chatgpt"}}, "backends": []string{"codex", "chatgpt"}}
	if status, _ := call(http.MethodPost, responsePath, body, otherAgent); status != http.StatusNotFound {
		t.Fatal("other computer answered request", status)
	}
	if status, _ := call(http.MethodPost, responsePath, map[string]any{"projects": []any{}, "backends": []string{"invalid"}}, agent); status != http.StatusBadRequest {
		t.Fatal("invalid project catalog accepted", status)
	}
	if status, result := call(http.MethodPost, responsePath, body, agent); status != http.StatusOK {
		t.Fatal(status, result)
	}
	select {
	case result := <-done:
		if result.status != http.StatusOK || len(result.body["projects"].([]any)) != 2 || len(result.body["backends"].([]any)) != 2 {
			t.Fatal("phone did not receive catalog", result)
		}
	case <-time.After(time.Second):
		t.Fatal("phone project request did not finish")
	}
	_, pending = call(http.MethodGet, "/agent/project-requests", nil, agent)
	if len(pending["requests"].([]any)) != 0 {
		t.Fatal("project request retained after delivery")
	}
	if status, _ := call(http.MethodPost, responsePath, body, agent); status != http.StatusNotFound {
		t.Fatal("expired project response accepted", status)
	}
}

func TestCreateThreadCommandCanCreateBothBackendsAndReturnsRoutingIdentity(t *testing.T) {
	call, workspace, deviceID, controller, agent := setupCreationAPI(t)
	route := "/v1/commands?workspaceId=" + workspace
	for _, backend := range []string{"codex", "chatgpt"} {
		body := map[string]any{"deviceId": deviceID, "kind": "create_thread", "backend": backend, "projectId": backend + "-project", "text": "初始消息", "dedupeKey": backend + "-creation"}
		if status, _ := call(http.MethodPost, "/v1/commands?workspaceId=other", body, controller); status != http.StatusForbidden {
			t.Fatal("cross workspace creation accepted", status)
		}
		status, queued := call(http.MethodPost, route, body, controller)
		if status != http.StatusCreated {
			t.Fatal(status, queued)
		}
		if status, retry := call(http.MethodPost, route, body, controller); status != http.StatusOK || retry["commandId"] != queued["commandId"] {
			t.Fatal("create retry was not idempotent", status, retry)
		}
		_, pending := call(http.MethodGet, "/agent/commands", nil, agent)
		commands := pending["commands"].([]any)
		if len(commands) != 1 {
			t.Fatal(commands)
		}
		command := commands[0].(map[string]any)
		if command["backend"] != backend || command["projectId"] != backend+"-project" || command["text"] != "初始消息" || command["threadId"] != nil {
			t.Fatal("creation destination lost", command)
		}
		ack := map[string]any{"ok": true, "result": map[string]any{"threadId": backend + "-thread", "backend": backend, "projectId": backend + "-project", "response": "private desktop contents"}}
		if status, result := call(http.MethodPost, fmt.Sprintf("/agent/commands/%.0f/ack", queued["commandId"]), ack, agent); status != http.StatusOK {
			t.Fatal(status, result)
		}
	}
	_, state := call(http.MethodGet, "/v1/state?workspaceId="+workspace, nil, controller)
	commands := state["commands"].([]any)
	if len(commands) != 2 {
		t.Fatal(commands)
	}
	for _, item := range commands {
		command := item.(map[string]any)
		result := command["result"].(map[string]any)
		if command["state"] != "completed" || command["thread_id"] != result["threadId"] || result["response"] != nil || result["submitted"] != true {
			t.Fatal("unexpected creation status", command)
		}
	}
}

func TestViewerCannotCreateChat(t *testing.T) {
	call, _, _, controller, _ := setupCreationAPI(t)
	control := map[string]string{"Authorization": "Bearer " + controlToken}
	status, team := call(http.MethodPost, "/v1/workspaces", map[string]any{"name": "Creation Team"}, controller)
	if status != http.StatusCreated {
		t.Fatal(status, team)
	}
	workspace := team["id"].(string)
	_, pair := call(http.MethodPost, "/control/pairing-codes", map[string]any{"ownerId": workspace}, control)
	_, enrolled := call(http.MethodPost, "/agent/enroll", map[string]any{"code": pair["code"], "name": "Team Computer"}, nil)
	deviceID := enrolled["deviceId"].(string)
	_, viewer := call(http.MethodPost, "/control/accounts", map[string]any{"email": "viewer-creation@example.com", "displayName": "Viewer", "password": "correct horse battery staple"}, control)
	if status, body := call(http.MethodPost, "/v1/workspaces/"+workspace+"/members", map[string]any{"email": "viewer-creation@example.com", "role": "viewer"}, controller); status != http.StatusOK {
		t.Fatal(status, body)
	}
	viewerHeaders := map[string]string{"Authorization": "Bearer " + viewer["token"].(string)}
	if status, _ := call(http.MethodPost, "/v1/commands?workspaceId="+workspace, map[string]any{"deviceId": deviceID, "kind": "create_thread", "backend": "codex", "text": "Initial prompt"}, viewerHeaders); status != http.StatusForbidden {
		t.Fatal("viewer created a chat", status)
	}
}

func TestCreateThreadCanFinishCommandWhileDesktopStartsTheChat(t *testing.T) {
	call, workspace, deviceID, controller, agent := setupCreationAPI(t)
	body := map[string]any{"deviceId": deviceID, "kind": "create_thread", "backend": "chatgpt", "text": "Initial prompt", "dedupeKey": "pending-creation"}
	status, queued := call(http.MethodPost, "/v1/commands?workspaceId="+workspace, body, controller)
	if status != http.StatusCreated {
		t.Fatal(status, queued)
	}
	call(http.MethodGet, "/agent/commands", nil, agent)
	ack := map[string]any{"ok": true, "result": map[string]any{"backend": "chatgpt", "clientThreadId": "client-new-thread:pending", "response": "private desktop contents"}}
	if status, result := call(http.MethodPost, fmt.Sprintf("/agent/commands/%.0f/ack", queued["commandId"]), ack, agent); status != http.StatusOK {
		t.Fatal(status, result)
	}
	_, state := call(http.MethodGet, "/v1/state?workspaceId="+workspace, nil, controller)
	command := state["commands"].([]any)[0].(map[string]any)
	result := command["result"].(map[string]any)
	if command["state"] != "completed" || command["thread_id"] != "" || result["clientThreadId"] != "client-new-thread:pending" || result["status"] != "starting" || result["response"] != nil || result["threadId"] != nil {
		t.Fatal("pending creation identity was treated as a real chat", command)
	}
	_, pending := call(http.MethodGet, "/agent/commands", nil, agent)
	if len(pending["commands"].([]any)) != 0 {
		t.Fatal("desktop creation was queued again", pending)
	}
}
