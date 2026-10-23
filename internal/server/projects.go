package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"codex-control/relay-go/internal/protocol"
)

type projectRequest struct {
	ID       string `json:"id"`
	deviceID string
	response chan protocol.ProjectCatalog
}

type projectBridge struct {
	sync.Mutex
	pending map[string]*projectRequest
}

func (s *Server) projectRequests(w http.ResponseWriter, r *http.Request, deviceID string) {
	s.projects.Lock()
	requests := []projectRequest{}
	for _, request := range s.projects.pending {
		if request.deviceID == deviceID && len(request.response) == 0 {
			requests = append(requests, *request)
			if len(requests) == 4 {
				break
			}
		}
	}
	s.projects.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"requests": requests})
}

func (s *Server) projectResponse(w http.ResponseWriter, r *http.Request, deviceID string) {
	id, ok := pathID(r.URL.Path, "/agent/project-responses/", "")
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Request expired"})
		return
	}
	data, err := s.readBody(w, r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	response, err := protocol.DecodeProjectCatalog(data)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	s.projects.Lock()
	request := s.projects.pending[id]
	if request == nil || request.deviceID != deviceID {
		s.projects.Unlock()
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Request expired"})
		return
	}
	select {
	case request.response <- response:
	default:
	}
	s.projects.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) projectCatalog(w http.ResponseWriter, r *http.Request, ownerID string, account bool) {
	deviceID, err := required(r.URL.Query().Get("deviceId"), "deviceId", 100)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	checkDevice := func() bool {
		allowed, err := s.store.DeviceBelongsToWorkspace(r.Context(), deviceID, ownerID)
		if err != nil {
			s.internalError(w, err)
			return false
		}
		if !allowed {
			s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "Device is not accessible"})
		}
		return allowed
	}
	if !checkDevice() {
		return
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		s.internalError(w, err)
		return
	}
	request := &projectRequest{ID: hex.EncodeToString(random[:]), deviceID: deviceID, response: make(chan protocol.ProjectCatalog, 1)}
	s.projects.Lock()
	if len(s.projects.pending) >= 100 {
		s.projects.Unlock()
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many pending reads"})
		return
	}
	s.projects.pending[request.ID] = request
	s.projects.Unlock()
	defer func() { s.projects.Lock(); delete(s.projects.pending, request.ID); s.projects.Unlock() }()
	timer := time.NewTimer(12 * time.Second)
	defer timer.Stop()
	select {
	case response := <-request.response:
		// Membership, session and device ownership can change while the phone waits.
		if account {
			principal, authenticated, err := s.store.AuthenticateController(r.Context(), bearer(r))
			if err != nil {
				s.internalError(w, err)
				return
			}
			if !authenticated {
				s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
				return
			}
			if _, ok := s.authorizeWorkspace(w, r, principal, false); !ok {
				return
			}
		}
		if !checkDevice() {
			return
		}
		if response.Error != "" {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": response.Error})
			return
		}
		s.writeJSON(w, http.StatusOK, response)
	case <-r.Context().Done():
		return
	case <-timer.C:
		s.writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": "Computer is unavailable; reconnect it to load projects"})
	}
}
