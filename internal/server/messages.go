package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-control/relay-go/internal/protocol"
)

type conversationRequest struct {
	ID       string `json:"id"`
	Kind     string `json:"kind,omitempty"`
	ThreadID string `json:"threadId"`
	Cursor   string `json:"cursor,omitempty"`
	*conversationAttachmentRequest
	deviceID string
	response chan conversationResponse
}
type conversationAttachmentRequest struct {
	MessageID    string `json:"messageId"`
	AttachmentID string `json:"attachmentId"`
	Offset       int64  `json:"offset"`
	Length       int64  `json:"length"`
}
type conversationResponse struct {
	Messages   []protocol.ConversationMessage `json:"messages"`
	NextCursor string                         `json:"nextCursor"`
	HasMore    bool                           `json:"hasMore"`
	Error      string                         `json:"error,omitempty"`
	attachment *protocol.AttachmentChunk
}
type conversationBridge struct {
	sync.Mutex
	pending map[string]*conversationRequest
}

func (s *Server) conversationRequests(w http.ResponseWriter, r *http.Request, deviceID string) {
	s.conversations.Lock()
	requests := []conversationRequest{}
	for _, request := range s.conversations.pending {
		if request.deviceID == deviceID && len(request.response) == 0 {
			requests = append(requests, *request)
			if len(requests) == 4 {
				break
			}
		}
	}
	s.conversations.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"requests": requests})
}

func (s *Server) conversationResponse(w http.ResponseWriter, r *http.Request, deviceID string) {
	id := strings.TrimPrefix(r.URL.Path, "/agent/conversation-responses/")
	s.conversations.Lock()
	request := s.conversations.pending[id]
	s.conversations.Unlock()
	if request == nil || request.deviceID != deviceID {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Request expired"})
		return
	}
	data, err := s.readBody(w, r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	var response conversationResponse
	if err := json.Unmarshal(data, &response); err != nil {
		s.badRequest(w, err)
		return
	}
	if response.Error == "" {
		if request.Kind == "attachment" {
			chunk, err := protocol.DecodeAttachmentChunk(data, request.AttachmentID, request.Offset, request.Length)
			if err != nil {
				s.badRequest(w, err)
				return
			}
			response = conversationResponse{attachment: &chunk}
		} else {
			messages, err := protocol.DecodeMessages(data)
			if err != nil {
				s.badRequest(w, err)
				return
			}
			response.Messages = messages
			if response.NextCursor == "" || len(response.NextCursor) > 1024 {
				s.badRequest(w, errors.New("cursor is invalid"))
				return
			}
		}
	} else {
		response = conversationResponse{Error: "Conversation is unavailable on the computer"}
	}
	s.conversations.Lock()
	if s.conversations.pending[id] != request {
		s.conversations.Unlock()
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Request expired"})
		return
	}
	for _, message := range response.Messages {
		if message.ThreadID != request.ThreadID {
			s.conversations.Unlock()
			s.badRequest(w, errors.New("threadId does not match request"))
			return
		}
	}
	select {
	case request.response <- response:
	default:
	}
	s.conversations.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) conversationMessages(w http.ResponseWriter, r *http.Request, ownerID string) {
	s.conversationRead(w, r, ownerID, nil)
}

func (s *Server) conversationAttachment(w http.ResponseWriter, r *http.Request, ownerID string) {
	query := r.URL.Query()
	messageID, err := required(query.Get("messageId"), "messageId", 200)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	attachmentID := query.Get("attachmentId")
	if !protocol.IsAttachmentID(attachmentID) {
		s.badRequest(w, errors.New("attachmentId is invalid"))
		return
	}
	offset, err := attachmentQueryInteger(query.Get("offset"), 0)
	if err != nil || offset >= protocol.MaxAttachmentBytes {
		s.badRequest(w, errors.New("attachment offset is invalid"))
		return
	}
	length, err := attachmentQueryInteger(query.Get("length"), protocol.MaxAttachmentChunkBytes)
	if err != nil || length < 1 || length > protocol.MaxAttachmentChunkBytes {
		s.badRequest(w, errors.New("attachment length is invalid"))
		return
	}
	s.conversationRead(w, r, ownerID, &conversationAttachmentRequest{MessageID: messageID, AttachmentID: attachmentID, Offset: offset, Length: length})
}

func attachmentQueryInteger(value string, fallback int64) (int64, error) {
	if value == "" {
		return fallback, nil
	}
	// Accept only decimal unsigned integer spelling; reject signs and whitespace.
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid integer")
		}
	}
	return strconv.ParseInt(value, 10, 64)
}

func (s *Server) conversationRead(w http.ResponseWriter, r *http.Request, ownerID string, attachment *conversationAttachmentRequest) {
	deviceID, err := required(r.URL.Query().Get("deviceId"), "deviceId", 100)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	threadID, err := required(r.URL.Query().Get("threadId"), "threadId", 100)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 1024 {
		s.badRequest(w, errors.New("cursor is invalid"))
		return
	}
	allowed, err := s.store.CanReadConversation(r.Context(), ownerID, deviceID, threadID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !allowed {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "Conversation is not accessible"})
		return
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		s.internalError(w, err)
		return
	}
	request := &conversationRequest{ID: hex.EncodeToString(random[:]), ThreadID: threadID, Cursor: cursor, deviceID: deviceID, response: make(chan conversationResponse, 1)}
	if attachment != nil {
		request.Kind = "attachment"
		request.Cursor = ""
		request.conversationAttachmentRequest = attachment
	}
	s.conversations.Lock()
	if len(s.conversations.pending) >= 100 {
		s.conversations.Unlock()
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many pending reads"})
		return
	}
	s.conversations.pending[request.ID] = request
	s.conversations.Unlock()
	defer func() {
		s.conversations.Lock()
		delete(s.conversations.pending, request.ID)
		// Release a response that raced with cancellation or timeout immediately.
		select {
		case <-request.response:
		default:
		}
		s.conversations.Unlock()
	}()
	timer := time.NewTimer(12 * time.Second)
	defer timer.Stop()
	select {
	case response := <-request.response:
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			principal, ok, err := s.store.AuthenticateController(r.Context(), bearer(r))
			if err != nil {
				s.internalError(w, err)
				return
			}
			if !ok {
				s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
				return
			}
			if _, ok := s.authorizeWorkspace(w, r, principal, false); !ok {
				return
			}
		}
		// Recheck after the wait: a task may have moved or the device been revoked.
		allowed, err := s.store.CanReadConversation(r.Context(), ownerID, deviceID, threadID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if !allowed {
			s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "Conversation is not accessible"})
			return
		}
		if response.Error != "" {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": response.Error})
			return
		}
		if response.attachment != nil {
			s.writeJSON(w, http.StatusOK, response.attachment)
		} else {
			s.writeJSON(w, http.StatusOK, response)
		}
	case <-r.Context().Done():
		return
	case <-timer.C:
		s.writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": "Computer is unavailable; saved history remains on the phone"})
	}
}
