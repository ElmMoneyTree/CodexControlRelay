package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"codex-control/relay-go/internal/config"
	"codex-control/relay-go/internal/protocol"
	"codex-control/relay-go/internal/store"
)

type Server struct {
	cfg               config.Config
	store             *store.Store
	logger            *slog.Logger
	controlHash       [32]byte
	trustedProxy      []*net.IPNet
	allowedOrigin     map[string]bool
	enrollmentLimiter *rateLimiter
	controlLimiter    *rateLimiter
	agentLimiter      *rateLimiter
}

func New(cfg config.Config, database *store.Store, logger *slog.Logger) (*Server, error) {
	result := &Server{
		cfg: cfg, store: database, logger: logger,
		controlHash:       sha256.Sum256([]byte(cfg.Security.ControlToken)),
		allowedOrigin:     map[string]bool{},
		enrollmentLimiter: newRateLimiter(cfg.Security.RateLimits.Enrollment),
		controlLimiter:    newRateLimiter(cfg.Security.RateLimits.Control),
		agentLimiter:      newRateLimiter(cfg.Security.RateLimits.Agent),
	}
	for _, value := range cfg.Server.TrustedProxies {
		if ip := net.ParseIP(value); ip != nil {
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			result.trustedProxy = append(result.trustedProxy, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		} else {
			_, network, err := net.ParseCIDR(value)
			if err != nil {
				return nil, err
			}
			result.trustedProxy = append(result.trustedProxy, network)
		}
	}
	for _, origin := range cfg.Security.AllowedOrigins {
		result.allowedOrigin[strings.TrimSuffix(origin, "/")] = true
	}
	return result, nil
}

func (s *Server) Handler() http.Handler {
	return s.logging(s.securityHeaders(http.HandlerFunc(s.route)))
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if !s.cors(w, r) {
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	clientIP := s.clientIP(r)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodGet && r.URL.Path == "/ready":
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.store.Ping(ctx); err != nil {
			s.logger.Error("readiness check failed", "error", err)
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case strings.HasPrefix(r.URL.Path, "/control/"):
		if !s.allow(w, s.controlLimiter, "control:"+clientIP) {
			return
		}
		if !s.authorizeControl(r) {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
			return
		}
		s.control(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/agent/enroll":
		if !s.allow(w, s.enrollmentLimiter, "enroll:"+clientIP) {
			return
		}
		s.enroll(w, r)
	case strings.HasPrefix(r.URL.Path, "/agent/"):
		if !s.allow(w, s.agentLimiter, "agent:"+clientIP) {
			return
		}
		token := bearer(r)
		if token == "" {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
			return
		}
		deviceID, ok, err := s.store.AuthenticateAgent(r.Context(), token)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if !ok {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
			return
		}
		s.agent(w, r, deviceID)
	default:
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
	}
}

func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/control/pairing-codes":
		var input struct {
			OwnerID string `json:"ownerId"`
		}
		if err := s.decodeBody(w, r, &input); err != nil {
			s.badRequest(w, err)
			return
		}
		if input.OwnerID == "" {
			input.OwnerID = "owner"
		}
		ownerID, err := required(input.OwnerID, "ownerId", 100)
		if err != nil {
			s.badRequest(w, err)
			return
		}
		pair, err := s.store.CreatePairingCode(r.Context(), ownerID, s.cfg.Security.PairingCodeTTL.Duration)
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeJSON(w, http.StatusCreated, pair)
	case r.Method == http.MethodGet && r.URL.Path == "/control/state":
		ownerID := r.URL.Query().Get("ownerId")
		if ownerID == "" {
			ownerID = "owner"
		}
		ownerID, err := required(ownerID, "ownerId", 100)
		if err != nil {
			s.badRequest(w, err)
			return
		}
		state, err := s.store.State(r.Context(), ownerID, time.Now().Add(-s.cfg.Security.OfflineAfter.Duration).UnixMilli())
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, state)
	case r.Method == http.MethodPost && r.URL.Path == "/control/commands":
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
		id, created, err := s.store.CreateCommand(r.Context(), deviceID, command, dedupe)
		if errors.Is(err, store.ErrDeviceNotFound) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Device not found"})
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		s.writeJSON(w, status, map[string]any{"commandId": id})
	default:
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
	}
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code         string  `json:"code"`
		Name         string  `json:"name"`
		AccountLabel *string `json:"accountLabel"`
	}
	if err := s.decodeBody(w, r, &input); err != nil {
		s.badRequest(w, err)
		return
	}
	code, err := required(input.Code, "code", 100)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	name, err := required(input.Name, "name", 200)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	if input.AccountLabel != nil {
		value := strings.TrimSpace(*input.AccountLabel)
		if value == "" {
			input.AccountLabel = nil
		} else if len(utf16.Encode([]rune(value))) > 200 {
			s.badRequest(w, errors.New("accountLabel is invalid"))
			return
		} else {
			input.AccountLabel = &value
		}
	}
	enrollment, err := s.store.Enroll(r.Context(), code, name, input.AccountLabel)
	if errors.Is(err, store.ErrPairingCodeInvalid) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Pairing code is invalid or expired"})
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, enrollment)
}

func (s *Server) agent(w http.ResponseWriter, r *http.Request, deviceID string) {
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/agent/state":
		data, err := s.readBody(w, r)
		if err != nil {
			s.badRequest(w, err)
			return
		}
		state, requests, err := protocol.DecodeAgentState(data)
		if err != nil {
			s.badRequest(w, err)
			return
		}
		if err := s.store.Sync(r.Context(), deviceID, state, requests); err != nil {
			s.internalError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodGet && r.URL.Path == "/agent/commands":
		commands, err := s.store.ClaimCommands(r.Context(), deviceID, 20)
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"commands": commands})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/agent/commands/") && strings.HasSuffix(r.URL.Path, "/ack"):
		middle := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/agent/commands/"), "/ack")
		if middle == "" || strings.Contains(middle, "/") {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
			return
		}
		commandID, err := strconv.ParseInt(middle, 10, 64)
		if err != nil || commandID <= 0 {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
			return
		}
		var input struct {
			OK     bool `json:"ok"`
			Result any  `json:"result"`
		}
		if err := s.decodeBody(w, r, &input); err != nil {
			s.badRequest(w, err)
			return
		}
		err = s.store.Acknowledge(r.Context(), deviceID, commandID, input.OK, input.Result)
		if errors.Is(err, store.ErrCommandNotClaimable) {
			s.writeJSON(w, http.StatusConflict, map[string]any{"error": "Command is not claimable"})
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
	}
}

func (s *Server) authorizeControl(r *http.Request) bool {
	candidate := sha256.Sum256([]byte(bearer(r)))
	return subtle.ConstantTimeCompare(candidate[:], s.controlHash[:]) == 1
}
func bearer(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(value, "Bearer ")
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			return nil, &httpError{status: http.StatusRequestEntityTooLarge, message: "Payload too large"}
		}
		return nil, errors.New("unable to read request body")
	}
	if len(data) == 0 {
		return []byte("{}"), nil
	}
	return data, nil
}
func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, target any) error {
	data, err := s.readBody(w, r)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(target); err != nil {
		return errors.New("JSON is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("JSON is invalid")
	}
	return nil
}

type httpError struct {
	status  int
	message string
}

func (e *httpError) Error() string { return e.message }
func (s *Server) badRequest(w http.ResponseWriter, err error) {
	var typed *httpError
	if errors.As(err, &typed) {
		s.writeJSON(w, typed.status, map[string]any{"error": typed.message})
		return
	}
	s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}
func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.logger.Error("request failed", "error", err)
	s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal error"})
}
func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		s.logger.Error("write response", "error", err)
	}
}

func (s *Server) allow(w http.ResponseWriter, limiter *rateLimiter, key string) bool {
	allowed, retry := limiter.allow(key, time.Now())
	if allowed {
		return true
	}
	seconds := int(retry.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Rate limit exceeded"})
	return false
}

func (s *Server) clientIP(r *http.Request) string {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	ip := net.ParseIP(remote)
	if ip == nil {
		return remote
	}
	if !s.isTrustedProxy(ip) {
		return ip.String()
	}

	// Walk the forwarding chain from the nearest proxy toward the client and
	// stop at the first untrusted hop. This prevents a client-supplied leftmost
	// X-Forwarded-For value from bypassing per-IP enrollment rate limits.
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for index := len(forwarded) - 1; index >= 0; index-- {
		candidate := net.ParseIP(strings.TrimSpace(forwarded[index]))
		if candidate != nil && !s.isTrustedProxy(candidate) {
			return candidate.String()
		}
	}
	for _, part := range forwarded {
		if candidate := net.ParseIP(strings.TrimSpace(part)); candidate != nil {
			return candidate.String()
		}
	}
	return ip.String()
}

func (s *Server) isTrustedProxy(ip net.IP) bool {
	for _, network := range s.trustedProxy {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := strings.TrimSuffix(r.Header.Get("Origin"), "/")
	if origin == "" {
		return true
	}
	if !s.allowedOrigin[origin] {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "Origin is not allowed"})
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
	w.Header().Set("Access-Control-Max-Age", "600")
	return true
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *responseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	size, err := w.ResponseWriter.Write(data)
	w.bytes += size
	return size, err
}
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		wrapped := &responseWriter{ResponseWriter: w}
		next.ServeHTTP(wrapped, r)
		status := wrapped.status
		if status == 0 {
			status = http.StatusOK
		}
		s.logger.Info("http request", "method", r.Method, "path", r.URL.Path, "status", status, "bytes", wrapped.bytes, "duration_ms", time.Since(started).Milliseconds(), "remote_ip", s.clientIP(r))
	})
}

func required(value, name string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(utf16.Encode([]rune(value))) > max {
		return "", fmt.Errorf("%s is invalid", name)
	}
	return value, nil
}
