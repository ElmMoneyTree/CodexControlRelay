package server

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestConversationIsForwardedOnlyDuringAnAuthenticatedPhoneRequest(t *testing.T) {
	_, call := setup(t)
	control := map[string]string{"Authorization": "Bearer " + controlToken}
	status, account := call(http.MethodPost, "/control/accounts", map[string]any{
		"email": "history@example.com", "displayName": "History", "password": "correct horse battery staple", "personalWorkspaceName": "Personal",
	}, control)
	if status != http.StatusCreated {
		t.Fatalf("account: %d %#v", status, account)
	}
	workspace := account["workspaces"].([]any)[0].(map[string]any)["id"].(string)
	controller := map[string]string{"Authorization": "Bearer " + account["token"].(string)}
	_, pair := call(http.MethodPost, "/control/pairing-codes", map[string]any{"ownerId": workspace}, control)
	_, enrolled := call(http.MethodPost, "/agent/enroll", map[string]any{"code": pair["code"], "name": "PC"}, nil)
	agent := map[string]string{"Authorization": "Bearer " + enrolled["token"].(string)}
	deviceID := enrolled["deviceId"].(string)
	status, result := call(http.MethodPut, "/agent/state", map[string]any{
		"tasks": []any{map[string]any{"threadId": "thread", "title": "Task", "status": "idle", "updatedAt": 1}}, "requests": []any{},
	}, agent)
	if status != http.StatusOK {
		t.Fatalf("state: %d %#v", status, result)
	}
	body := map[string]any{"messages": []any{
		map[string]any{"threadId": "thread", "messageId": "u1", "role": "user", "text": "你好", "createdAt": 1, "position": 0},
		map[string]any{"threadId": "thread", "messageId": "a1", "role": "assistant", "text": "你好！", "createdAt": 2, "position": 1},
	}, "nextCursor": "next", "hasMore": false}
	if status, _ := call(http.MethodPut, "/agent/messages", body, agent); status != http.StatusNotFound {
		t.Fatal("server history upload endpoint must be removed")
	}
	route := "/v1/messages?workspaceId=" + url.QueryEscape(workspace) + "&deviceId=" + url.QueryEscape(deviceID) + "&threadId=thread&cursor=previous"
	if status, _ := call(http.MethodGet, route, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", status)
	}
	if status, _ := call(http.MethodGet, "/v1/messages?workspaceId=other&deviceId="+deviceID+"&threadId=thread", nil, controller); status != http.StatusForbidden {
		t.Fatalf("cross-workspace: %d", status)
	}
	type answer struct {
		status int
		page   map[string]any
	}
	done := make(chan answer, 1)
	go func() { status, page := call(http.MethodGet, route, nil, controller); done <- answer{status, page} }()
	var request map[string]any
	for i := 0; i < 40; i++ {
		_, pending := call(http.MethodGet, "/agent/conversation-requests", nil, agent)
		requests := pending["requests"].([]any)
		if len(requests) > 0 {
			request = requests[0].(map[string]any)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if request == nil || request["cursor"] != "previous" {
		t.Fatalf("phone request not forwarded: %#v", request)
	}
	status, result = call(http.MethodPost, "/agent/conversation-responses/"+request["id"].(string), body, agent)
	if status != http.StatusOK {
		t.Fatalf("response: %d %#v", status, result)
	}
	select {
	case answer := <-done:
		if answer.status != http.StatusOK || len(answer.page["messages"].([]any)) != 2 {
			t.Fatalf("history: %#v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("phone never received forwarded messages")
	}
	_, pending := call(http.MethodGet, "/agent/conversation-requests", nil, agent)
	if len(pending["requests"].([]any)) != 0 {
		t.Fatal("forwarded request was retained")
	}
	if status, _ := call(http.MethodPost, "/agent/conversation-responses/"+request["id"].(string), body, agent); status != http.StatusNotFound {
		t.Fatal("expired response accepted")
	}
}
