package server

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"codex-control/relay-go/internal/protocol"
	"codex-control/relay-go/internal/store"
	qrcode "github.com/skip2/go-qrcode"
)

const deviceClaimProofContext = "codex-control-device-claim-v1"

func (s *Server) v1(w http.ResponseWriter, r *http.Request, clientIP string) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/login":
		if !s.allow(w, s.authLimiter, "auth:"+clientIP) {
			return
		}
		s.login(w, r)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/register":
		if !s.allow(w, s.authLimiter, "register:"+clientIP) {
			return
		}
		if !s.cfg.Security.AllowRegistration {
			s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "Registration is disabled"})
			return
		}
		s.register(w, r)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/v1/device-claims":
		if !s.allow(w, s.claimLimiter, "claim-create:"+clientIP) {
			return
		}
		s.createDeviceClaim(w, r)
		return
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/device-claims/") && strings.HasSuffix(r.URL.Path, "/poll"):
		if !s.allow(w, s.claimLimiter, "claim-poll:"+clientIP) {
			return
		}
		s.pollDeviceClaim(w, r)
		return
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/device-claims/") && strings.HasSuffix(r.URL.Path, "/qr"):
		if !s.allow(w, s.claimLimiter, "claim-qr:"+clientIP) {
			return
		}
		s.deviceClaimQRCode(w, r)
		return
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/device-claims/") && strings.HasSuffix(r.URL.Path, "/status"):
		if !s.allow(w, s.claimLimiter, "claim-status:"+clientIP) {
			return
		}
		s.deviceClaimStatus(w, r)
		return
	}

	if !s.allow(w, s.controlLimiter, "v1:"+clientIP) {
		return
	}
	token := bearer(r)
	if token == "" {
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return
	}
	principal, ok, err := s.store.AuthenticateController(r.Context(), token)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !ok {
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return
	}
	s.v1Authenticated(w, r, token, principal)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := s.decodeBody(w, r, &input); err != nil {
		s.badRequest(w, err)
		return
	}
	result, err := s.store.Login(r.Context(), input.Email, input.Password, s.cfg.Security.AccountSessionTTL.Duration)
	if errors.Is(err, store.ErrInvalidCredentials) {
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Email or password is incorrect"})
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email                 string `json:"email"`
		DisplayName           string `json:"displayName"`
		Password              string `json:"password"`
		PersonalWorkspaceName string `json:"personalWorkspaceName"`
	}
	if err := s.decodeBody(w, r, &input); err != nil {
		s.badRequest(w, err)
		return
	}
	result, err := s.store.CreateAccount(r.Context(), input.Email, input.DisplayName, input.Password, input.PersonalWorkspaceName, s.cfg.Security.AccountSessionTTL.Duration)
	if errors.Is(err, store.ErrAccountExists) {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "Account already exists"})
		return
	}
	if errors.Is(err, store.ErrInvalidCredentials) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Account details are invalid"})
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, result)
}

func (s *Server) createDeviceClaim(w http.ResponseWriter, r *http.Request) {
	var input struct {
		DeviceName      string  `json:"deviceName"`
		AccountLabel    *string `json:"accountLabel"`
		DevicePublicKey string  `json:"devicePublicKey"`
		Nonce           string  `json:"nonce"`
		Proof           string  `json:"proof"`
	}
	if err := s.decodeBody(w, r, &input); err != nil {
		s.badRequest(w, err)
		return
	}
	deviceName, err := required(input.DeviceName, "deviceName", 200)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	if input.AccountLabel != nil {
		value := strings.TrimSpace(*input.AccountLabel)
		if len(utf16.Encode([]rune(value))) > 200 {
			s.badRequest(w, errors.New("accountLabel is invalid"))
			return
		}
		if value == "" {
			input.AccountLabel = nil
		} else {
			input.AccountLabel = &value
		}
	}
	publicKey, err := decodeBase64URL(input.DevicePublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		s.badRequest(w, errors.New("devicePublicKey is invalid"))
		return
	}
	nonce, err := decodeBase64URL(input.Nonce)
	if err != nil || len(nonce) < 16 || len(nonce) > 64 {
		s.badRequest(w, errors.New("nonce is invalid"))
		return
	}
	proof, err := decodeBase64URL(input.Proof)
	normalizedPublicKey := base64.RawURLEncoding.EncodeToString(publicKey)
	normalizedNonce := base64.RawURLEncoding.EncodeToString(nonce)
	proofMessage := []byte(deviceClaimProofContext + "\n" + normalizedPublicKey + "\n" + normalizedNonce + "\n" + deviceName)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), proofMessage, proof) {
		s.badRequest(w, errors.New("device proof is invalid"))
		return
	}
	fingerprintHash := sha256.Sum256(publicKey)
	fingerprint := groupedFingerprint(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(fingerprintHash[:10]))
	var legacyDeviceID *string
	if deviceToken := bearer(r); deviceToken != "" {
		deviceID, authenticated, err := s.store.AuthenticateAgent(r.Context(), deviceToken)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if !authenticated {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Existing device credential is invalid"})
			return
		}
		eligible, err := s.store.CanMigrateLegacyDevice(r.Context(), deviceID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if !eligible {
			s.writeJSON(w, http.StatusConflict, map[string]any{"error": "Device is already account-managed"})
			return
		}
		legacyDeviceID = &deviceID
	}
	claim, err := s.store.CreateDeviceClaim(r.Context(), normalizedPublicKey, fingerprint, deviceName, input.AccountLabel, legacyDeviceID, s.cfg.Security.DeviceClaimTTL.Duration)
	if err != nil {
		s.internalError(w, err)
		return
	}
	origin := s.publicOrigin(r)
	claimURL := origin + "/claim/" + url.PathEscape(claim.ID) + "#secret=" + url.QueryEscape(claim.ClaimSecret)
	claimURI := customClaimURI(origin, claim.ID, claim.ClaimSecret)
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"id": claim.ID, "claimSecret": claim.ClaimSecret, "pollToken": claim.PollToken,
		"deviceKeyFingerprint": claim.DeviceKeyFingerprint, "expiresAt": claim.ExpiresAt,
		"claimUrl": claimURL, "claimUri": claimURI, "migration": legacyDeviceID != nil,
	})
}

func (s *Server) deviceClaimQRCode(w http.ResponseWriter, r *http.Request) {
	claimID, ok := pathID(r.URL.Path, "/v1/device-claims/", "/qr")
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
		return
	}
	secret := r.Header.Get("X-Claim-Secret")
	if _, err := s.store.InspectDeviceClaim(r.Context(), claimID, secret); errors.Is(err, store.ErrClaimInvalid) {
		s.writeJSON(w, http.StatusGone, map[string]any{"error": "Device claim is invalid or expired"})
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	origin := s.publicOrigin(r)
	png, err := qrcode.Encode(customClaimURI(origin, claimID, secret), qrcode.Medium, 384)
	if err != nil {
		s.internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

func (s *Server) deviceClaimStatus(w http.ResponseWriter, r *http.Request) {
	claimID, ok := pathID(r.URL.Path, "/v1/device-claims/", "/status")
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
		return
	}
	progress, err := s.store.DeviceClaimProgress(r.Context(), claimID, r.Header.Get("X-Claim-Secret"))
	if errors.Is(err, store.ErrClaimInvalid) {
		s.writeJSON(w, http.StatusGone, map[string]any{"error": "Device claim is invalid"})
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, progress)
}

func (s *Server) pollDeviceClaim(w http.ResponseWriter, r *http.Request) {
	claimID, ok := pathID(r.URL.Path, "/v1/device-claims/", "/poll")
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
		return
	}
	pollToken := claimBearer(r)
	if pollToken == "" {
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return
	}
	claim, err := s.store.PollDeviceClaim(r.Context(), claimID, pollToken, s.claimWrapKey)
	if errors.Is(err, store.ErrClaimInvalid) {
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Device claim is invalid"})
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	claim.RelayURL = s.publicOrigin(r)
	s.writeJSON(w, http.StatusOK, claim)
}

func (s *Server) v1Authenticated(w http.ResponseWriter, r *http.Request, token string, principal store.Principal) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/logout":
		if err := s.store.Logout(r.Context(), token); err != nil {
			s.internalError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
		user, err := s.store.User(r.Context(), principal.UserID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		workspaces, err := s.store.ListWorkspaces(r.Context(), principal.UserID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"user": user, "workspaces": workspaces})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/workspaces":
		workspaces, err := s.store.ListWorkspaces(r.Context(), principal.UserID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"workspaces": workspaces})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/workspaces":
		var input struct {
			Name string `json:"name"`
		}
		if err := s.decodeBody(w, r, &input); err != nil {
			s.badRequest(w, err)
			return
		}
		workspace, err := s.store.CreateWorkspace(r.Context(), principal.UserID, input.Name)
		if err != nil {
			s.badRequest(w, errors.New("Workspace name is invalid"))
			return
		}
		s.writeJSON(w, http.StatusCreated, workspace)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/workspaces/") && strings.HasSuffix(r.URL.Path, "/members"):
		s.addWorkspaceMember(w, r, principal)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/workspaces/") && strings.HasSuffix(r.URL.Path, "/members"):
		s.listWorkspaceMembers(w, r, principal)
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/attachments"):
		workspaceID, ok := s.authorizeWorkspace(w, r, principal, false)
		if !ok {
			return
		}
		if r.URL.Path == "/v1/attachments" {
			s.conversationAttachment(w, r, workspaceID)
		} else {
			s.conversationMessages(w, r, workspaceID)
		}
	case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
		workspaceID, ok := s.authorizeWorkspace(w, r, principal, false)
		if !ok {
			return
		}
		s.projectCatalog(w, r, workspaceID, true)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/state":
		workspaceID, ok := s.authorizeWorkspace(w, r, principal, false)
		if !ok {
			return
		}
		state, err := s.store.State(r.Context(), workspaceID, nowMinus(s.cfg.Security.OfflineAfter.Duration))
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, state)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/commands":
		s.createV1Command(w, r, principal)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/device-claims/"):
		s.inspectDeviceClaim(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/device-claims/") && strings.HasSuffix(r.URL.Path, "/approve"):
		s.approveDeviceClaim(w, r, principal)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/devices/") && strings.HasSuffix(r.URL.Path, "/revoke"):
		s.revokeDevice(w, r, principal)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/audit-events":
		workspaceID, ok := s.authorizeWorkspace(w, r, principal, false)
		if !ok {
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		events, err := s.store.AuditEvents(r.Context(), principal.UserID, workspaceID, limit)
		if err != nil {
			s.handleV1StoreError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"events": events})
	default:
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
	}
}

func (s *Server) inspectDeviceClaim(w http.ResponseWriter, r *http.Request) {
	claimID, ok := pathID(r.URL.Path, "/v1/device-claims/", "")
	if !ok || strings.Contains(claimID, "/") {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
		return
	}
	claim, err := s.store.InspectDeviceClaim(r.Context(), claimID, r.Header.Get("X-Claim-Secret"))
	if errors.Is(err, store.ErrClaimInvalid) {
		s.writeJSON(w, http.StatusGone, map[string]any{"error": "Device claim is invalid or expired"})
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, claim)
}

func (s *Server) approveDeviceClaim(w http.ResponseWriter, r *http.Request, principal store.Principal) {
	claimID, ok := pathID(r.URL.Path, "/v1/device-claims/", "/approve")
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
		return
	}
	var input struct {
		ClaimSecret string `json:"claimSecret"`
		WorkspaceID string `json:"workspaceId"`
	}
	if err := s.decodeBody(w, r, &input); err != nil {
		s.badRequest(w, err)
		return
	}
	claim, err := s.store.ApproveDeviceClaim(r.Context(), principal.UserID, input.WorkspaceID, claimID, input.ClaimSecret, s.claimWrapKey)
	if err != nil {
		s.handleV1StoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, claim)
}

func (s *Server) createV1Command(w http.ResponseWriter, r *http.Request, principal store.Principal) {
	workspaceID := strings.TrimSpace(r.URL.Query().Get("workspaceId"))
	if workspaceID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "workspaceId is required"})
		return
	}
	data, err := s.readBody(w, r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	command, identity, err := protocol.DecodeCommand(data)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	deviceID, dedupe := protocol.SplitDeviceAndDedupe(identity)
	id, created, err := s.store.CreateWorkspaceCommand(r.Context(), principal.UserID, workspaceID, deviceID, command, dedupe)
	if err != nil {
		s.handleV1StoreError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, map[string]any{"commandId": id})
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request, principal store.Principal) {
	deviceID, ok := pathID(r.URL.Path, "/v1/devices/", "/revoke")
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
		return
	}
	var input struct {
		WorkspaceID string `json:"workspaceId"`
	}
	if err := s.decodeBody(w, r, &input); err != nil {
		s.badRequest(w, err)
		return
	}
	if err := s.store.RevokeDevice(r.Context(), principal.UserID, input.WorkspaceID, deviceID); err != nil {
		s.handleV1StoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) addWorkspaceMember(w http.ResponseWriter, r *http.Request, principal store.Principal) {
	workspaceID, ok := pathID(r.URL.Path, "/v1/workspaces/", "/members")
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
		return
	}
	var input struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := s.decodeBody(w, r, &input); err != nil {
		s.badRequest(w, err)
		return
	}
	if err := s.store.AddWorkspaceMember(r.Context(), principal.UserID, workspaceID, input.Email, input.Role); err != nil {
		s.handleV1StoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) listWorkspaceMembers(w http.ResponseWriter, r *http.Request, principal store.Principal) {
	workspaceID, ok := pathID(r.URL.Path, "/v1/workspaces/", "/members")
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
		return
	}
	members, err := s.store.ListWorkspaceMembers(r.Context(), principal.UserID, workspaceID)
	if err != nil {
		s.handleV1StoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

func (s *Server) authorizeWorkspace(w http.ResponseWriter, r *http.Request, principal store.Principal, write bool) (string, bool) {
	workspaceID := strings.TrimSpace(r.URL.Query().Get("workspaceId"))
	if workspaceID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "workspaceId is required"})
		return "", false
	}
	ok, err := s.store.CanUseWorkspace(r.Context(), principal.UserID, workspaceID, write)
	if err != nil {
		s.internalError(w, err)
		return "", false
	}
	if !ok {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "Forbidden"})
		return "", false
	}
	return workspaceID, true
}

func (s *Server) handleV1StoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "Forbidden"})
	case errors.Is(err, store.ErrClaimInvalid):
		s.writeJSON(w, http.StatusGone, map[string]any{"error": "Device claim is invalid or expired"})
	case errors.Is(err, store.ErrClaimAlreadyUsed):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "Device claim was already used"})
	case errors.Is(err, store.ErrDedupeConflict):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "Dedupe key belongs to another command"})
	case errors.Is(err, store.ErrDeviceNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Device not found"})
	case errors.Is(err, store.ErrInvalidCredentials):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Account not found"})
	default:
		s.internalError(w, err)
	}
}

func decodeBase64URL(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
}

func groupedFingerprint(value string) string {
	value = strings.ToUpper(value)
	parts := make([]string, 0, (len(value)+3)/4)
	for len(value) > 4 {
		parts = append(parts, value[:4])
		value = value[4:]
	}
	if value != "" {
		parts = append(parts, value)
	}
	return strings.Join(parts, "-")
}

func (s *Server) publicOrigin(r *http.Request) string {
	if configured := strings.TrimSuffix(s.cfg.Server.PublicURL, "/"); configured != "" {
		return configured
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func customClaimURI(origin, claimID, secret string) string {
	return "codexcontrol://claim?relay=" + url.QueryEscape(origin) + "&id=" + url.QueryEscape(claimID) + "#secret=" + url.QueryEscape(secret)
}

func claimBearer(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Claim ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(value, "Claim "))
}

func pathID(path, prefix, suffix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if value == "" || strings.Contains(value, "/") {
		return "", false
	}
	return value, true
}

func nowMinus(duration time.Duration) int64 {
	return time.Now().Add(-duration).UnixMilli()
}
