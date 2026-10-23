package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf16"
)

func migrateTaskBackend(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(tasks)")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, required, primary int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &required, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		found = found || name == "backend"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !found {
		_, err = db.Exec("ALTER TABLE tasks ADD COLUMN backend TEXT")
	}
	return err
}

func sanitizeCreatedThreadResult(ok bool, data, commandBody []byte) ([]byte, string, error) {
	var response struct {
		ThreadID       string  `json:"threadId"`
		ClientThreadID string  `json:"clientThreadId"`
		Backend        string  `json:"backend"`
		ProjectID      *string `json:"projectId"`
		Error          string  `json:"error"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, "", errors.New("new chat acknowledgement is invalid")
	}
	if !ok {
		message := strings.TrimSpace(response.Error)
		if message == "" || len(utf16.Encode([]rune(message))) > 2000 {
			message = "Chat creation failed"
		}
		encoded, err := json.Marshal(map[string]string{"error": message})
		return encoded, "", err
	}
	var command struct {
		Backend   string  `json:"backend"`
		ProjectID *string `json:"projectId"`
	}
	if err := json.Unmarshal(commandBody, &command); err != nil {
		return nil, "", err
	}
	threadID := strings.TrimSpace(response.ThreadID)
	clientThreadID := strings.TrimSpace(response.ClientThreadID)
	if (threadID == "") == (clientThreadID == "") || len(utf16.Encode([]rune(threadID))) > 100 || len(utf16.Encode([]rune(clientThreadID))) > 200 || response.Backend != command.Backend || (response.Backend != "codex" && response.Backend != "chatgpt") {
		return nil, "", errors.New("new chat identity is invalid")
	}
	result := map[string]any{"submitted": true, "backend": response.Backend}
	if threadID != "" {
		result["threadId"] = threadID
	} else {
		result["clientThreadId"] = clientThreadID
		result["status"] = "starting"
	}
	if response.ProjectID != nil {
		projectID := strings.TrimSpace(*response.ProjectID)
		if projectID == "" || len(utf16.Encode([]rune(projectID))) > 200 {
			return nil, "", errors.New("new chat project identity is invalid")
		}
		result["projectId"] = projectID
	}
	if command.ProjectID != nil {
		if response.ProjectID != nil && *response.ProjectID != *command.ProjectID {
			return nil, "", errors.New("new chat project does not match command")
		}
		result["projectId"] = *command.ProjectID
	} else if response.ProjectID != nil {
		return nil, "", errors.New("new chat project does not match command")
	}
	encoded, err := json.Marshal(result)
	return encoded, threadID, err
}
