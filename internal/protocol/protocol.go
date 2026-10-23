package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	MaxTasks    = 1000
	MaxRequests = 500
)

var commandKinds = map[string]bool{
	"create_thread": true, "send_message": true, "command_approval": true, "file_approval": true,
	"permissions_approval": true, "user_input_response": true, "mcp_response": true,
}

type AgentState struct {
	Tasks    []Task           `json:"tasks"`
	Requests []PendingRequest `json:"requests"`
}

type Timestamp int64

func (t *Timestamp) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var number float64
	switch typed := value.(type) {
	case json.Number:
		number, _ = strconv.ParseFloat(typed.String(), 64)
	case string:
		number, _ = strconv.ParseFloat(strings.TrimSpace(typed), 64)
	case bool:
		if typed {
			number = 1
		}
	default:
		number = 0
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 || number > math.MaxInt64 {
		*t = 0
		return nil
	}
	*t = Timestamp(math.Floor(number))
	return nil
}

type Task struct {
	ThreadID      string    `json:"threadId"`
	Backend       *string   `json:"backend,omitempty"`
	Title         string    `json:"title"`
	Project       *string   `json:"project"`
	Status        string    `json:"status"`
	Summary       *string   `json:"summary"`
	LatestMessage *string   `json:"latestMessage"`
	UpdatedAt     Timestamp `json:"updatedAt"`
}

type PendingRequest struct {
	ThreadID  string          `json:"threadId"`
	RequestID json.RawMessage `json:"requestId"`
	TurnID    *string         `json:"turnId"`
	Kind      string          `json:"kind"`
	Payload   map[string]any  `json:"payload"`
	UpdatedAt Timestamp       `json:"updatedAt"`
}

type ValidatedRequest struct {
	ThreadID  string
	RequestID string
	TurnID    *string
	Kind      string
	Payload   map[string]any
	UpdatedAt int64
}

type Command struct {
	Kind      string
	ThreadID  string
	Backend   *string
	ProjectID *string
	RequestID *string
	TurnID    *string
	Text      *string
	Decision  *string
	Action    *string
	Answers   map[string][]string
	Content   map[string]any
	Body      []byte
}

func DecodeAgentState(data []byte) (AgentState, []ValidatedRequest, error) {
	var state AgentState
	if err := decodeOne(data, &state); err != nil {
		return state, nil, fmt.Errorf("state is invalid: %w", err)
	}
	if state.Tasks == nil || state.Requests == nil {
		return state, nil, errors.New("tasks and requests must be arrays")
	}
	if len(state.Tasks) > MaxTasks || len(state.Requests) > MaxRequests {
		return state, nil, errors.New("state payload is too large")
	}
	for i := range state.Tasks {
		task := &state.Tasks[i]
		var err error
		if task.ThreadID, err = required(task.ThreadID, "threadId", 100); err != nil {
			return state, nil, err
		}
		if task.Backend, err = optional(task.Backend, "backend", 20); err != nil {
			return state, nil, err
		}
		if task.Backend != nil && !oneOf(*task.Backend, "codex", "chatgpt") {
			return state, nil, errors.New("task backend is unsupported")
		}
		if task.Title, err = required(task.Title, "title", 500); err != nil {
			return state, nil, err
		}
		if task.Project, err = optional(task.Project, "project", 1000); err != nil {
			return state, nil, err
		}
		if task.Status, err = required(task.Status, "status", 40); err != nil {
			return state, nil, err
		}
		if task.Summary, err = optional(task.Summary, "summary", 4000); err != nil {
			return state, nil, err
		}
		if task.LatestMessage, err = optional(task.LatestMessage, "latestMessage", 20000); err != nil {
			return state, nil, err
		}
		if task.UpdatedAt <= 0 {
			task.UpdatedAt = Timestamp(time.Now().UnixMilli())
		}
	}
	requests := make([]ValidatedRequest, 0, len(state.Requests))
	for _, item := range state.Requests {
		threadID, err := required(item.ThreadID, "threadId", 100)
		if err != nil {
			return state, nil, err
		}
		requestID, err := requestIDString(item.RequestID)
		if err != nil {
			return state, nil, err
		}
		turnID, err := optional(item.TurnID, "turnId", 100)
		if err != nil {
			return state, nil, err
		}
		kind, err := required(item.Kind, "kind", 100)
		if err != nil {
			return state, nil, err
		}
		if item.Payload == nil {
			item.Payload = map[string]any{}
		}
		payload, err := json.Marshal(item.Payload)
		if err != nil || len(payload) > 100000 {
			return state, nil, errors.New("request payload is too large")
		}
		if item.UpdatedAt <= 0 {
			item.UpdatedAt = Timestamp(time.Now().UnixMilli())
		}
		requests = append(requests, ValidatedRequest{ThreadID: threadID, RequestID: requestID, TurnID: turnID, Kind: kind, Payload: item.Payload, UpdatedAt: int64(item.UpdatedAt)})
	}
	return state, requests, nil
}

func DecodeCommand(data []byte) (Command, string, error) {
	var raw struct {
		DeviceID  string              `json:"deviceId"`
		Kind      string              `json:"kind"`
		ThreadID  string              `json:"threadId"`
		Backend   *string             `json:"backend"`
		ProjectID *string             `json:"projectId"`
		RequestID json.RawMessage     `json:"requestId"`
		TurnID    *string             `json:"turnId"`
		Text      *string             `json:"text"`
		Decision  *string             `json:"decision"`
		Action    *string             `json:"action"`
		Answers   map[string][]string `json:"answers"`
		Content   map[string]any      `json:"content"`
		DedupeKey *string             `json:"dedupeKey"`
	}
	if err := decodeOne(data, &raw); err != nil {
		return Command{}, "", errors.New("command is invalid")
	}
	deviceID, err := required(raw.DeviceID, "deviceId", 100)
	if err != nil {
		return Command{}, "", err
	}
	kind, err := required(raw.Kind, "kind", 40)
	if err != nil || !commandKinds[kind] {
		return Command{}, "", errors.New("command kind is unsupported")
	}
	threadID := ""
	if kind != "create_thread" {
		threadID, err = required(raw.ThreadID, "threadId", 100)
		if err != nil {
			return Command{}, "", err
		}
	} else if raw.ThreadID != "" {
		return Command{}, "", errors.New("new chats cannot have a threadId")
	}
	backend, err := optional(raw.Backend, "backend", 20)
	if err != nil || (backend != nil && !oneOf(*backend, "codex", "chatgpt")) {
		return Command{}, "", errors.New("backend is unsupported")
	}
	projectID, err := optional(raw.ProjectID, "projectId", 200)
	if err != nil {
		return Command{}, "", err
	}
	requestID, err := optionalRawString(raw.RequestID, "requestId", 100)
	if err != nil {
		return Command{}, "", err
	}
	turnID, err := optional(raw.TurnID, "turnId", 100)
	if err != nil {
		return Command{}, "", err
	}
	text, err := optional(raw.Text, "text", 20000)
	if err != nil {
		return Command{}, "", err
	}
	decision, err := optional(raw.Decision, "decision", 30)
	if err != nil {
		return Command{}, "", err
	}
	action, err := optional(raw.Action, "action", 30)
	if err != nil {
		return Command{}, "", err
	}

	answers := raw.Answers
	switch kind {
	case "create_thread":
		if backend == nil || text == nil {
			return Command{}, "", errors.New("new chat backend and text are required")
		}
		if requestID != nil || turnID != nil || decision != nil || action != nil || len(answers) != 0 || raw.Content != nil {
			return Command{}, "", errors.New("new chat fields are invalid")
		}
	case "send_message":
		if text == nil {
			return Command{}, "", errors.New("message text is required")
		}
	case "command_approval", "file_approval":
		if requestID == nil || turnID == nil || decision == nil || !oneOf(*decision, "accept", "decline", "cancel") {
			return Command{}, "", errors.New("approval identity or decision is invalid")
		}
	case "permissions_approval":
		if requestID == nil || turnID == nil || decision == nil || !oneOf(*decision, "accept", "decline") {
			return Command{}, "", errors.New("permission approval identity or decision is invalid")
		}
	case "user_input_response":
		if requestID == nil || turnID == nil || len(raw.Answers) == 0 {
			return Command{}, "", errors.New("user input identity or answers are invalid")
		}
		answers, err = normalizeAnswers(raw.Answers)
		if err != nil {
			return Command{}, "", err
		}
	case "mcp_response":
		if requestID == nil || action == nil || !oneOf(*action, "accept", "decline", "cancel") || (*action == "accept" && raw.Content == nil) {
			return Command{}, "", errors.New("MCP response identity, action, or content is invalid")
		}
	}
	if raw.Content != nil {
		encoded, _ := json.Marshal(raw.Content)
		if len(encoded) > 50000 {
			return Command{}, "", errors.New("content is too large")
		}
	}

	normalized := map[string]any{"kind": kind, "threadId": threadID, "requestId": requestID, "turnId": turnID, "text": text, "decision": decision, "answers": answers, "action": action, "content": raw.Content}
	if kind == "create_thread" {
		normalized = map[string]any{"kind": kind, "backend": backend, "text": text}
		if projectID != nil {
			normalized["projectId"] = projectID
		}
	} else if kind == "send_message" && backend != nil {
		normalized["backend"] = backend
	}
	body, _ := json.Marshal(normalized)
	dedupe := ""
	if raw.DedupeKey != nil {
		dedupe, err = required(*raw.DedupeKey, "dedupeKey", 200)
		if err != nil {
			return Command{}, "", err
		}
	}
	return Command{Kind: kind, ThreadID: threadID, Backend: backend, ProjectID: projectID, RequestID: requestID, TurnID: turnID, Text: text, Decision: decision, Action: action, Answers: answers, Content: raw.Content, Body: body}, deviceID + "\x00" + dedupe, nil
}

func SplitDeviceAndDedupe(value string) (string, string) {
	parts := strings.SplitN(value, "\x00", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func required(value, name string, max int) (string, error) {
	value = strings.TrimSpace(value)
	// JavaScript String.length counts UTF-16 code units. Matching it keeps the
	// Go Relay's validation behavior identical to the existing clients/Relay.
	if value == "" || len(utf16.Encode([]rune(value))) > max {
		return "", fmt.Errorf("%s is invalid", name)
	}
	return value, nil
}

func optional(value *string, name string, max int) (*string, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := required(*value, name, max)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func optionalRawString(value json.RawMessage, name string, max int) (*string, error) {
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return nil, nil
	}
	var stringValue string
	if err := json.Unmarshal(value, &stringValue); err == nil {
		return optional(&stringValue, name, max)
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return nil, fmt.Errorf("%s is invalid", name)
	}
	converted := number.String()
	return optional(&converted, name, max)
}

func requestIDString(value json.RawMessage) (string, error) {
	parsed, err := optionalRawString(value, "requestId", 100)
	if err != nil || parsed == nil {
		return "", errors.New("requestId is invalid")
	}
	return *parsed, nil
}

func normalizeAnswers(answers map[string][]string) (map[string][]string, error) {
	if len(answers) > 20 {
		return nil, errors.New("answers is invalid")
	}
	result := make(map[string][]string, len(answers))
	for key, values := range answers {
		id, err := required(key, "questionId", 100)
		if err != nil || len(values) == 0 || len(values) > 20 {
			return nil, errors.New("answer list is invalid")
		}
		normalized := make([]string, len(values))
		for index, value := range values {
			parsed, err := required(value, "answer", 4000)
			if err != nil {
				return nil, err
			}
			normalized[index] = parsed
		}
		result[id] = normalized
	}
	return result, nil
}

func oneOf(value string, values ...string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func decodeOne(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values")
	}
	return nil
}
