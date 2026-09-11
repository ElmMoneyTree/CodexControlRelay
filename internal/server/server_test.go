package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-control/relay-go/internal/config"
	"codex-control/relay-go/internal/store"
)

const controlToken = "0123456789abcdef0123456789abcdef"

func setup(t *testing.T) (*httptest.Server, func(string, string, any, map[string]string) (int, map[string]any)) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Security.ControlToken = controlToken
	return setupWithConfig(t, cfg)
}

func setupWithConfig(t *testing.T, cfg config.Config) (*httptest.Server, func(string, string, any, map[string]string) (int, map[string]any)) {
	t.Helper()
	database, err := store.Open(":memory:", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	relay, err := New(cfg, database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(relay.Handler())
	t.Cleanup(host.Close)
	call := func(method, path string, body any, headers map[string]string) (int, map[string]any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(encoded)
		}
		request, err := http.NewRequest(method, host.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result := map[string]any{}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil && response.StatusCode != http.StatusNoContent {
			t.Fatal(err)
		}
		return response.StatusCode, result
	}
	return host, call
}

func TestFullCompatibilityFlow(t *testing.T) {
	_, call := setup(t)
	control := map[string]string{"Authorization": "Bearer " + controlToken}
	status, pair := call(http.MethodPost, "/control/pairing-codes", map[string]any{"ownerId": "me"}, control)
	if status != http.StatusCreated {
		t.Fatalf("pair status %d: %#v", status, pair)
	}
	status, enrolled := call(http.MethodPost, "/agent/enroll", map[string]any{"code": pair["code"], "name": "PC A", "accountLabel": "Account A"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("enroll status %d: %#v", status, enrolled)
	}
	status, _ = call(http.MethodPost, "/agent/enroll", map[string]any{"code": pair["code"], "name": "PC B"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("pairing code reused with status %d", status)
	}
	agent := map[string]string{"Authorization": "Bearer " + enrolled["token"].(string)}
	state := map[string]any{"tasks": []any{map[string]any{"threadId": "thread-1", "title": "Fix test", "project": "repo", "status": "active", "updatedAt": 1}}, "requests": []any{map[string]any{"threadId": "thread-1", "requestId": "7", "turnId": "turn-1", "kind": "command", "payload": map[string]any{"command": "npm test", "availableDecisions": []string{"accept", "cancel"}}, "updatedAt": 2}}}
	status, _ = call(http.MethodPut, "/agent/state", state, agent)
	if status != http.StatusOK {
		t.Fatalf("sync status %d", status)
	}
	status, controlState := call(http.MethodGet, "/control/state?ownerId=me", nil, control)
	if status != http.StatusOK {
		t.Fatal(status)
	}
	if len(controlState["devices"].([]any)) != 1 || controlState["tasks"].([]any)[0].(map[string]any)["title"] != "Fix test" {
		t.Fatalf("bad state: %#v", controlState)
	}
	command := map[string]any{"deviceId": enrolled["deviceId"], "kind": "command_approval", "threadId": "thread-1", "requestId": "7", "turnId": "turn-1", "decision": "accept", "dedupeKey": "approve-7"}
	status, queued := call(http.MethodPost, "/control/commands", command, control)
	if status != http.StatusCreated {
		t.Fatalf("queue status %d", status)
	}
	status, duplicate := call(http.MethodPost, "/control/commands", command, control)
	if status != http.StatusOK || duplicate["commandId"] != queued["commandId"] {
		t.Fatalf("dedupe failed: %d %#v %#v", status, queued, duplicate)
	}
	status, commands := call(http.MethodGet, "/agent/commands", nil, agent)
	if status != http.StatusOK || len(commands["commands"].([]any)) != 1 {
		t.Fatalf("claim failed: %d %#v", status, commands)
	}
	status, commands = call(http.MethodGet, "/agent/commands", nil, agent)
	if status != http.StatusOK || len(commands["commands"].([]any)) != 1 {
		t.Fatalf("claimed command was not redelivered: %d %#v", status, commands)
	}
	id := int64(queued["commandId"].(float64))
	status, _ = call(http.MethodPost, "/agent/commands/"+jsonNumber(id)+"/ack", map[string]any{"ok": true, "result": map[string]any{"submitted": true}}, agent)
	if status != http.StatusOK {
		t.Fatalf("ack status %d", status)
	}
	_, completed := call(http.MethodGet, "/control/state?ownerId=me", nil, control)
	first := completed["commands"].([]any)[0].(map[string]any)
	if first["state"] != "completed" || first["result"].(map[string]any)["submitted"] != true {
		t.Fatalf("completion missing: %#v", first)
	}
}

func TestAuthenticationPayloadAndCORSGuards(t *testing.T) {
	host, call := setup(t)
	status, _ := call(http.MethodGet, "/control/state", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("control auth status %d", status)
	}
	status, _ = call(http.MethodGet, "/agent/commands", nil, map[string]string{"Authorization": "Bearer wrong"})
	if status != http.StatusUnauthorized {
		t.Fatalf("agent auth status %d", status)
	}
	request, err := http.NewRequest(http.MethodPost, host.URL+"/agent/enroll", strings.NewReader(strings.Repeat("x", 1_000_001)))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("large payload status %d", response.StatusCode)
	}
	status, _ = call(http.MethodGet, "/health", nil, map[string]string{"Origin": "https://untrusted.example"})
	if status != http.StatusForbidden {
		t.Fatalf("CORS status %d", status)
	}
}

func TestMCPRequestWithoutTurnID(t *testing.T) {
	_, call := setup(t)
	control := map[string]string{"Authorization": "Bearer " + controlToken}
	_, pair := call(http.MethodPost, "/control/pairing-codes", map[string]any{"ownerId": "me"}, control)
	_, enrolled := call(http.MethodPost, "/agent/enroll", map[string]any{"code": pair["code"], "name": "PC"}, nil)
	agent := map[string]string{"Authorization": "Bearer " + enrolled["token"].(string)}
	status, _ := call(http.MethodPut, "/agent/state", map[string]any{"tasks": []any{}, "requests": []any{map[string]any{"threadId": "thread-1", "requestId": "m1", "turnId": nil, "kind": "mcp", "payload": map[string]any{"mode": "form"}, "updatedAt": 2}}}, agent)
	if status != http.StatusOK {
		t.Fatal(status)
	}
	_, state := call(http.MethodGet, "/control/state?ownerId=me", nil, control)
	if state["requests"].([]any)[0].(map[string]any)["turn_id"] != nil {
		t.Fatalf("turn_id was not nullable: %#v", state)
	}
	status, _ = call(http.MethodPost, "/control/commands", map[string]any{"deviceId": enrolled["deviceId"], "kind": "mcp_response", "threadId": "thread-1", "requestId": "m1", "action": "decline", "dedupeKey": "mcp-1"}, control)
	if status != http.StatusCreated {
		t.Fatalf("MCP command status %d", status)
	}
}

func TestAllowedCORSPreflightAndTLSHeaders(t *testing.T) {
	cfg := config.Defaults()
	cfg.Security.ControlToken = controlToken
	cfg.Security.AllowedOrigins = []string{"https://controller.example"}
	database, err := store.Open(":memory:", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	relay, err := New(cfg, database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewTLSServer(relay.Handler())
	defer host.Close()
	request, err := http.NewRequest(http.MethodOptions, host.URL+"/control/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", "https://controller.example")
	response, err := host.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status %d", response.StatusCode)
	}
	if value := response.Header.Get("Access-Control-Allow-Origin"); value != "https://controller.example" {
		t.Fatalf("unexpected CORS origin %q", value)
	}
	if value := response.Header.Get("Strict-Transport-Security"); value != "max-age=31536000" {
		t.Fatalf("unexpected HSTS header %q", value)
	}
}

func TestEnrollmentRateLimitAndTrustedProxyChain(t *testing.T) {
	cfg := config.Defaults()
	cfg.Security.ControlToken = controlToken
	cfg.Security.RateLimits.Enrollment.Requests = 1
	cfg.Server.TrustedProxies = []string{"127.0.0.1/32", "10.0.0.0/8"}
	host, _ := setupWithConfig(t, cfg)

	request := func(xff string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, host.URL+"/agent/enroll", strings.NewReader(`{"code":"bad","name":"PC"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", xff)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response
	}
	first := request("203.0.113.99, 198.51.100.10, 10.0.0.4")
	if first.StatusCode != http.StatusBadRequest {
		t.Fatalf("first request status %d", first.StatusCode)
	}
	second := request("192.0.2.77, 198.51.100.10, 10.0.0.4")
	if second.StatusCode != http.StatusTooManyRequests || second.Header.Get("Retry-After") == "" {
		t.Fatalf("spoofed XFF bypassed rate limit: status=%d retry=%q", second.StatusCode, second.Header.Get("Retry-After"))
	}
}

func jsonNumber(value int64) string { return strings.TrimSpace(string(mustJSON(value))) }
func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
