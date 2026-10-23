package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestRelayHostedClaimPortalOriginIsAllowed(t *testing.T) {
	host, _ := setup(t)
	request, err := http.NewRequest(http.MethodGet, host.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", host.URL)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("same-origin request status %d", response.StatusCode)
	}
	if value := response.Header.Get("Access-Control-Allow-Origin"); value != host.URL {
		t.Fatalf("unexpected same-origin CORS header %q", value)
	}
}

func TestLegacyAPIKillSwitchKeepsAccountBootstrap(t *testing.T) {
	cfg := config.Defaults()
	cfg.Security.ControlToken = controlToken
	cfg.Security.LegacyAPIEnabled = false
	_, call := setupWithConfig(t, cfg)
	control := map[string]string{"Authorization": "Bearer " + controlToken}
	status, _ := call(http.MethodPost, "/control/pairing-codes", map[string]any{"ownerId": "legacy"}, control)
	if status != http.StatusNotFound {
		t.Fatalf("legacy pairing remained enabled with status %d", status)
	}
	status, _ = call(http.MethodPost, "/agent/enroll", map[string]any{"code": "unused", "name": "PC"}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("legacy enrollment remained enabled with status %d", status)
	}
	status, account := call(http.MethodPost, "/control/accounts", map[string]any{
		"email": "bootstrap@example.com", "displayName": "Bootstrap", "password": "bootstrap correct horse battery",
	}, control)
	if status != http.StatusCreated || account["token"] == nil {
		t.Fatalf("account bootstrap was disabled: %d %#v", status, account)
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

func TestDeviceClaimMultiTenantFlow(t *testing.T) {
	host, call := setup(t)
	legacyControl := map[string]string{"Authorization": "Bearer " + controlToken}
	status, registration := call(http.MethodPost, "/v1/auth/register", map[string]any{
		"email": "blocked@example.com", "displayName": "Blocked", "password": "correct horse battery staple",
	}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("closed registration status %d: %#v", status, registration)
	}
	status, account := call(http.MethodPost, "/control/accounts", map[string]any{
		"email": "owner@example.com", "displayName": "Owner", "password": "correct horse battery staple", "personalWorkspaceName": "Owner Space",
	}, legacyControl)
	if status != http.StatusCreated {
		t.Fatalf("create account status %d: %#v", status, account)
	}
	status, loggedIn := call(http.MethodPost, "/v1/auth/login", map[string]any{"email": "OWNER@example.com", "password": "correct horse battery staple"}, nil)
	if status != http.StatusOK {
		t.Fatalf("login status %d: %#v", status, loggedIn)
	}
	controller := map[string]string{"Authorization": "Bearer " + loggedIn["token"].(string)}
	workspaceID := account["workspaces"].([]any)[0].(map[string]any)["id"].(string)

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey := base64.RawURLEncoding.EncodeToString(publicKey)
	nonce := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 24))
	proofMessage := []byte(deviceClaimProofContext + "\n" + encodedKey + "\n" + nonce + "\nOffice PC")
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, proofMessage))
	status, invalidProof := call(http.MethodPost, "/v1/device-claims", map[string]any{
		"deviceName": "Office PC", "devicePublicKey": encodedKey, "nonce": nonce, "proof": base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, ed25519.SignatureSize)),
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid device proof status %d: %#v", status, invalidProof)
	}
	status, created := call(http.MethodPost, "/v1/device-claims", map[string]any{
		"deviceName": "Office PC", "devicePublicKey": encodedKey, "nonce": nonce, "proof": proof,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create claim status %d: %#v", status, created)
	}
	claimID := created["id"].(string)
	claimSecret := created["claimSecret"].(string)
	pollToken := created["pollToken"].(string)
	progressPath := "/v1/device-claims/" + claimID + "/status"
	for _, headers := range []map[string]string{nil, {"X-Claim-Secret": "wrong"}, {"X-Claim-Secret": pollToken}, controller} {
		status, _ := call(http.MethodGet, progressPath, nil, headers)
		if status != http.StatusGone {
			t.Fatalf("claim progress accepted missing/wrong secret: %d", status)
		}
	}
	status, _ = call(http.MethodGet, progressPath+"?secret="+url.QueryEscape(claimSecret), nil, nil)
	if status != http.StatusGone {
		t.Fatalf("claim progress accepted query secret: %d", status)
	}
	progressHeaders := map[string]string{"X-Claim-Secret": claimSecret}
	status, progress := call(http.MethodGet, progressPath, nil, progressHeaders)
	if status != http.StatusOK || progress["status"] != "pending" || progress["expiresAt"] != created["expiresAt"] || len(progress) != 2 {
		t.Fatalf("pending claim progress status %d: %#v", status, progress)
	}
	status, _ = call(http.MethodGet, "/v1/device-claims/"+claimID+"/poll", nil, map[string]string{"Authorization": "Claim " + claimSecret})
	if status != http.StatusUnauthorized {
		t.Fatalf("browser secret could poll device credentials: %d", status)
	}
	if created["deviceKeyFingerprint"] == "" || created["claimUrl"] == "" {
		t.Fatalf("claim response incomplete: %#v", created)
	}
	pageResponse, err := http.Get(host.URL + "/claim/" + claimID)
	if err != nil {
		t.Fatal(err)
	}
	pageBody, _ := io.ReadAll(pageResponse.Body)
	pageResponse.Body.Close()
	if pageResponse.StatusCode != http.StatusOK || !bytes.Contains(pageBody, []byte("认领这台 Codex 设备")) || !bytes.Contains(pageBody, []byte(`id="mobile-claim"`)) || !strings.Contains(pageResponse.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("claim page status=%d csp=%q", pageResponse.StatusCode, pageResponse.Header.Get("Content-Security-Policy"))
	}

	inspectHeaders := map[string]string{"Authorization": controller["Authorization"], "X-Claim-Secret": claimSecret}
	status, claim := call(http.MethodGet, "/v1/device-claims/"+claimID, nil, inspectHeaders)
	if status != http.StatusOK || claim["deviceName"] != "Office PC" {
		t.Fatalf("inspect claim status %d: %#v", status, claim)
	}
	status, leakedQuery := call(http.MethodGet, "/v1/device-claims/"+claimID+"/qr?secret="+url.QueryEscape(claimSecret), nil, nil)
	if status != http.StatusGone {
		t.Fatalf("QR endpoint accepted query-string secret with status %d: %#v", status, leakedQuery)
	}
	qrRequest, err := http.NewRequest(http.MethodGet, host.URL+"/v1/device-claims/"+claimID+"/qr", nil)
	if err != nil {
		t.Fatal(err)
	}
	qrRequest.Header.Set("X-Claim-Secret", claimSecret)
	qrResponse, err := http.DefaultClient.Do(qrRequest)
	if err != nil {
		t.Fatal(err)
	}
	qrBody, _ := io.ReadAll(qrResponse.Body)
	qrResponse.Body.Close()
	if qrResponse.StatusCode != http.StatusOK || qrResponse.Header.Get("Content-Type") != "image/png" || len(qrBody) < 8 || !bytes.Equal(qrBody[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		t.Fatalf("claim QR status=%d type=%q bytes=%d", qrResponse.StatusCode, qrResponse.Header.Get("Content-Type"), len(qrBody))
	}
	status, approved := call(http.MethodPost, "/v1/device-claims/"+claimID+"/approve", map[string]any{"claimSecret": claimSecret, "workspaceId": workspaceID}, controller)
	if status != http.StatusOK {
		t.Fatalf("approve claim status %d: %#v", status, approved)
	}
	deviceID := approved["deviceId"].(string)
	status, progress = call(http.MethodGet, progressPath, nil, progressHeaders)
	if status != http.StatusOK || progress["status"] != "approved" || len(progress) != 2 {
		t.Fatalf("approved progress exposed extra fields or failed: %d %#v", status, progress)
	}
	status, replay := call(http.MethodPost, "/v1/device-claims/"+claimID+"/approve", map[string]any{"claimSecret": claimSecret, "workspaceId": workspaceID}, controller)
	if status != http.StatusConflict {
		t.Fatalf("claim replay status %d: %#v", status, replay)
	}

	status, polled := call(http.MethodGet, "/v1/device-claims/"+claimID+"/poll", nil, map[string]string{"Authorization": "Claim " + pollToken})
	if status != http.StatusOK || polled["status"] != "approved" || polled["deviceToken"] == "" || polled["relayUrl"] != host.URL {
		t.Fatalf("poll claim status %d: %#v", status, polled)
	}
	agent := map[string]string{"Authorization": "Bearer " + polled["deviceToken"].(string)}
	status, _ = call(http.MethodPut, "/agent/state", map[string]any{"tasks": []any{map[string]any{"threadId": "thread-1", "title": "Tenant task", "status": "idle", "updatedAt": 1}}, "requests": []any{}}, agent)
	if status != http.StatusOK {
		t.Fatalf("claimed agent sync status %d", status)
	}
	status, state := call(http.MethodGet, "/v1/state?workspaceId="+url.QueryEscape(workspaceID), nil, controller)
	if status != http.StatusOK || len(state["devices"].([]any)) != 1 || state["tasks"].([]any)[0].(map[string]any)["title"] != "Tenant task" {
		t.Fatalf("tenant state status %d: %#v", status, state)
	}

	status, second := call(http.MethodPost, "/control/accounts", map[string]any{
		"email": "other@example.com", "displayName": "Other", "password": "another correct horse battery", "personalWorkspaceName": "Other Space",
	}, legacyControl)
	if status != http.StatusCreated {
		t.Fatalf("create second account status %d: %#v", status, second)
	}
	otherController := map[string]string{"Authorization": "Bearer " + second["token"].(string)}
	status, forbidden := call(http.MethodGet, "/v1/state?workspaceId="+url.QueryEscape(workspaceID), nil, otherController)
	if status != http.StatusForbidden {
		t.Fatalf("cross-tenant state status %d: %#v", status, forbidden)
	}
	otherWorkspaceID := second["workspaces"].([]any)[0].(map[string]any)["id"].(string)
	status, hidden := call(http.MethodPost, "/v1/commands?workspaceId="+url.QueryEscape(otherWorkspaceID), map[string]any{
		"deviceId": deviceID, "kind": "send_message", "threadId": "thread-1", "text": "should not send", "dedupeKey": "cross-tenant",
	}, otherController)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant command status %d: %#v", status, hidden)
	}

	status, queued := call(http.MethodPost, "/v1/commands?workspaceId="+url.QueryEscape(workspaceID), map[string]any{
		"deviceId": deviceID, "kind": "send_message", "threadId": "thread-1", "text": "continue", "dedupeKey": "tenant-command",
	}, controller)
	if status != http.StatusCreated || queued["commandId"] == nil {
		t.Fatalf("tenant command status %d: %#v", status, queued)
	}
	status, duplicateCommand := call(http.MethodPost, "/v1/commands?workspaceId="+url.QueryEscape(workspaceID), map[string]any{
		"deviceId": deviceID, "kind": "send_message", "threadId": "thread-1", "text": "continue", "dedupeKey": "tenant-command",
	}, controller)
	if status != http.StatusOK || duplicateCommand["commandId"] != queued["commandId"] {
		t.Fatalf("tenant command dedupe status %d: %#v", status, duplicateCommand)
	}
	status, claimedCommands := call(http.MethodGet, "/agent/commands", nil, agent)
	if status != http.StatusOK || len(claimedCommands["commands"].([]any)) != 1 {
		t.Fatalf("claim tenant command status %d: %#v", status, claimedCommands)
	}
	commandID := int64(queued["commandId"].(float64))
	status, _ = call(http.MethodPost, "/agent/commands/"+jsonNumber(commandID)+"/ack", map[string]any{"ok": true, "result": map[string]any{"submitted": true}}, agent)
	if status != http.StatusOK {
		t.Fatalf("ack tenant command status %d", status)
	}
	status, events := call(http.MethodGet, "/v1/audit-events?workspaceId="+url.QueryEscape(workspaceID), nil, controller)
	if status != http.StatusOK || len(events["events"].([]any)) < 3 {
		t.Fatalf("audit events status %d: %#v", status, events)
	}
	actions := map[string]bool{}
	for _, raw := range events["events"].([]any) {
		actions[raw.(map[string]any)["action"].(string)] = true
	}
	createdEventCount := 0
	for _, raw := range events["events"].([]any) {
		if raw.(map[string]any)["action"] == "command.created" {
			createdEventCount++
		}
	}
	if !actions["device.claimed"] || !actions["command.created"] || !actions["command.completed"] || createdEventCount != 1 {
		t.Fatalf("audit lifecycle incomplete: %#v", actions)
	}

	status, team := call(http.MethodPost, "/v1/workspaces", map[string]any{"name": "Support Team"}, controller)
	if status != http.StatusCreated {
		t.Fatalf("create team status %d: %#v", status, team)
	}
	teamID := team["id"].(string)
	status, added := call(http.MethodPost, "/v1/workspaces/"+teamID+"/members", map[string]any{"email": "other@example.com", "role": "admin"}, controller)
	if status != http.StatusOK {
		t.Fatalf("add admin status %d: %#v", status, added)
	}
	status, ownerDemotion := call(http.MethodPost, "/v1/workspaces/"+teamID+"/members", map[string]any{"email": "owner@example.com", "role": "viewer"}, otherController)
	if status != http.StatusForbidden {
		t.Fatalf("admin demoted owner with status %d: %#v", status, ownerDemotion)
	}
	status, viewerAccount := call(http.MethodPost, "/control/accounts", map[string]any{
		"email": "viewer@example.com", "displayName": "Viewer", "password": "viewer correct horse battery", "personalWorkspaceName": "Viewer Space",
	}, legacyControl)
	if status != http.StatusCreated {
		t.Fatalf("create viewer account status %d: %#v", status, viewerAccount)
	}
	viewerController := map[string]string{"Authorization": "Bearer " + viewerAccount["token"].(string)}
	status, added = call(http.MethodPost, "/v1/workspaces/"+teamID+"/members", map[string]any{"email": "viewer@example.com", "role": "viewer"}, controller)
	if status != http.StatusOK {
		t.Fatalf("add viewer status %d: %#v", status, added)
	}
	status, members := call(http.MethodGet, "/v1/workspaces/"+teamID+"/members", nil, viewerController)
	if status != http.StatusOK || len(members["members"].([]any)) != 3 {
		t.Fatalf("list team members status %d: %#v", status, members)
	}
	status, _ = call(http.MethodGet, "/v1/state?workspaceId="+url.QueryEscape(teamID), nil, viewerController)
	if status != http.StatusOK {
		t.Fatalf("viewer could not read team status %d", status)
	}
	status, viewerWrite := call(http.MethodPost, "/v1/commands?workspaceId="+url.QueryEscape(teamID), map[string]any{
		"deviceId": deviceID, "kind": "send_message", "threadId": "thread-1", "text": "must not send", "dedupeKey": "viewer-command",
	}, viewerController)
	if status != http.StatusForbidden {
		t.Fatalf("viewer write status %d: %#v", status, viewerWrite)
	}
	status, revoked := call(http.MethodPost, "/v1/devices/"+deviceID+"/revoke", map[string]any{"workspaceId": workspaceID}, controller)
	if status != http.StatusOK || revoked["ok"] != true {
		t.Fatalf("revoke status %d: %#v", status, revoked)
	}
	status, _ = call(http.MethodPut, "/agent/state", map[string]any{"tasks": []any{}, "requests": []any{}}, agent)
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked device authenticated with status %d", status)
	}
	status, _ = call(http.MethodPost, "/v1/auth/logout", nil, otherController)
	if status != http.StatusOK {
		t.Fatalf("logout status %d", status)
	}
	status, _ = call(http.MethodGet, "/v1/me", nil, otherController)
	if status != http.StatusUnauthorized {
		t.Fatalf("logged out session remained valid with status %d", status)
	}
}

func TestExpiredDeviceClaimCannotDeliverCredential(t *testing.T) {
	cfg := config.Defaults()
	cfg.Security.ControlToken = controlToken
	cfg.Security.DeviceClaimTTL.Duration = time.Millisecond
	_, call := setupWithConfig(t, cfg)

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey := base64.RawURLEncoding.EncodeToString(publicKey)
	nonce := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 24))
	message := []byte(deviceClaimProofContext + "\n" + encodedKey + "\n" + nonce + "\nShort-lived PC")
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))
	status, created := call(http.MethodPost, "/v1/device-claims", map[string]any{
		"deviceName": "Short-lived PC", "devicePublicKey": encodedKey, "nonce": nonce, "proof": proof,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create expiring claim status %d: %#v", status, created)
	}
	time.Sleep(5 * time.Millisecond)
	status, progress := call(http.MethodGet, "/v1/device-claims/"+created["id"].(string)+"/status", nil, map[string]string{"X-Claim-Secret": created["claimSecret"].(string)})
	if status != http.StatusOK || progress["status"] != "expired" || len(progress) != 2 {
		t.Fatalf("expired progress status %d: %#v", status, progress)
	}
	status, polled := call(http.MethodGet, "/v1/device-claims/"+created["id"].(string)+"/poll", nil, map[string]string{"Authorization": "Claim " + created["pollToken"].(string)})
	if status != http.StatusOK || polled["status"] != "expired" || polled["deviceToken"] != nil {
		t.Fatalf("expired claim poll status %d: %#v", status, polled)
	}
}

func TestLegacyDeviceMigratesInPlaceToWorkspace(t *testing.T) {
	host, call := setup(t)
	control := map[string]string{"Authorization": "Bearer " + controlToken}
	_, pair := call(http.MethodPost, "/control/pairing-codes", map[string]any{"ownerId": "legacy-owner"}, control)
	status, enrolled := call(http.MethodPost, "/agent/enroll", map[string]any{"code": pair["code"], "name": "Legacy PC"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("legacy enrollment status %d: %#v", status, enrolled)
	}
	legacyDeviceID := enrolled["deviceId"].(string)
	legacyToken := enrolled["token"].(string)
	legacyAgent := map[string]string{"Authorization": "Bearer " + legacyToken}
	status, _ = call(http.MethodPut, "/agent/state", map[string]any{
		"tasks": []any{map[string]any{"threadId": "legacy-thread", "title": "Preserved task", "status": "idle", "updatedAt": 1}}, "requests": []any{},
	}, legacyAgent)
	if status != http.StatusOK {
		t.Fatalf("legacy sync status %d", status)
	}
	status, stale := call(http.MethodPost, "/control/commands", map[string]any{
		"deviceId": legacyDeviceID, "kind": "send_message", "threadId": "legacy-thread", "text": "stale", "dedupeKey": "before-migration",
	}, control)
	if status != http.StatusCreated {
		t.Fatalf("queue stale command status %d: %#v", status, stale)
	}

	status, account := call(http.MethodPost, "/control/accounts", map[string]any{
		"email": "migrate@example.com", "displayName": "Migrator", "password": "migration correct horse battery", "personalWorkspaceName": "Migrated Space",
	}, control)
	if status != http.StatusCreated {
		t.Fatalf("migration account status %d: %#v", status, account)
	}
	controller := map[string]string{"Authorization": "Bearer " + account["token"].(string)}
	workspaceID := account["workspaces"].([]any)[0].(map[string]any)["id"].(string)

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey := base64.RawURLEncoding.EncodeToString(publicKey)
	nonce := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{4}, 24))
	message := []byte(deviceClaimProofContext + "\n" + encodedKey + "\n" + nonce + "\nLegacy PC")
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))
	status, claim := call(http.MethodPost, "/v1/device-claims", map[string]any{
		"deviceName": "Legacy PC", "devicePublicKey": encodedKey, "nonce": nonce, "proof": proof,
	}, legacyAgent)
	if status != http.StatusCreated || claim["migration"] != true {
		t.Fatalf("migration claim status %d: %#v", status, claim)
	}
	status, approved := call(http.MethodPost, "/v1/device-claims/"+claim["id"].(string)+"/approve", map[string]any{
		"claimSecret": claim["claimSecret"], "workspaceId": workspaceID,
	}, controller)
	if status != http.StatusOK || approved["deviceId"] != legacyDeviceID {
		t.Fatalf("migration approval status %d: %#v", status, approved)
	}
	status, polled := call(http.MethodGet, "/v1/device-claims/"+claim["id"].(string)+"/poll", nil, map[string]string{"Authorization": "Claim " + claim["pollToken"].(string)})
	if status != http.StatusOK || polled["deviceToken"] == "" || polled["relayUrl"] != host.URL {
		t.Fatalf("migration poll status %d: %#v", status, polled)
	}
	status, _ = call(http.MethodGet, "/agent/commands", nil, legacyAgent)
	if status != http.StatusUnauthorized {
		t.Fatalf("legacy token remained valid with status %d", status)
	}
	newAgent := map[string]string{"Authorization": "Bearer " + polled["deviceToken"].(string)}
	status, commands := call(http.MethodGet, "/agent/commands", nil, newAgent)
	if status != http.StatusOK || len(commands["commands"].([]any)) != 0 {
		t.Fatalf("stale legacy command reached migrated agent: %d %#v", status, commands)
	}
	status, state := call(http.MethodGet, "/v1/state?workspaceId="+url.QueryEscape(workspaceID), nil, controller)
	if status != http.StatusOK || len(state["devices"].([]any)) != 1 || len(state["tasks"].([]any)) != 1 {
		t.Fatalf("migrated state status %d: %#v", status, state)
	}
	if state["commands"].([]any)[0].(map[string]any)["state"] != "failed" {
		t.Fatalf("pre-migration command was not failed: %#v", state["commands"])
	}
	status, legacyState := call(http.MethodGet, "/control/state?ownerId=legacy-owner", nil, control)
	if status != http.StatusOK || len(legacyState["devices"].([]any)) != 0 {
		t.Fatalf("legacy owner retained migrated device: %d %#v", status, legacyState)
	}
	status, events := call(http.MethodGet, "/v1/audit-events?workspaceId="+url.QueryEscape(workspaceID), nil, controller)
	if status != http.StatusOK || events["events"].([]any)[0].(map[string]any)["action"] != "device.migrated" {
		t.Fatalf("migration audit status %d: %#v", status, events)
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
