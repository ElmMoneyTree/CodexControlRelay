package protocol

import (
	"errors"
	"strings"
	"unicode/utf16"
)

type ConversationMessage struct {
	ThreadID    string                   `json:"threadId"`
	MessageID   string                   `json:"messageId"`
	Role        string                   `json:"role"`
	Text        string                   `json:"text"`
	CreatedAt   int64                    `json:"createdAt"`
	Position    int64                    `json:"position"`
	Truncated   bool                     `json:"truncated"`
	Attachments []ConversationAttachment `json:"attachments,omitempty"`
}

func DecodeMessages(data []byte) ([]ConversationMessage, error) {
	var batch struct {
		Messages []ConversationMessage `json:"messages"`
	}
	if err := decodeOne(data, &batch); err != nil {
		return nil, err
	}
	if batch.Messages == nil || len(batch.Messages) > 100 {
		return nil, errors.New("messages must contain at most 100 items")
	}
	for i := range batch.Messages {
		message := &batch.Messages[i]
		var err error
		if message.ThreadID, err = required(message.ThreadID, "threadId", 100); err != nil {
			return nil, err
		}
		if message.MessageID, err = required(message.MessageID, "messageId", 200); err != nil {
			return nil, err
		}
		if !oneOf(message.Role, "user", "assistant") {
			return nil, errors.New("message role is invalid")
		}
		// Validate without trimming, so code blocks and indentation survive sync.
		if len(utf16.Encode([]rune(message.Text))) > 100000 || (strings.TrimSpace(message.Text) == "" && len(message.Attachments) == 0) {
			return nil, errors.New("message text or attachments are required")
		}
		if err := ValidateAttachments(message.Attachments); err != nil {
			return nil, err
		}
		if message.CreatedAt <= 0 || message.Position < 0 {
			return nil, errors.New("message timestamp or position is invalid")
		}
	}
	return batch.Messages, nil
}
