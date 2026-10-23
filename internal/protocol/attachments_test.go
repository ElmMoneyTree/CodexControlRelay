package protocol

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestAttachmentMetadataAndPureImageMessages(t *testing.T) {
	attachment := ConversationAttachment{ID: strings.Repeat("a", 64), Name: "image.png", MIMEType: "image/png", ByteCount: 3, SHA256: strings.Repeat("b", 64)}
	message := ConversationMessage{ThreadID: "thread", MessageID: "image", Role: "user", CreatedAt: 1, Attachments: []ConversationAttachment{attachment}}
	body, _ := json.Marshal(map[string]any{"messages": []ConversationMessage{message}})
	if messages, err := DecodeMessages(body); err != nil || len(messages) != 1 || len(messages[0].Attachments) != 1 {
		t.Fatalf("pure image: %v", err)
	}
	for _, change := range []func(*ConversationAttachment){
		func(a *ConversationAttachment) { a.ID = "../../private" },
		func(a *ConversationAttachment) { a.MIMEType = "text/html" },
		func(a *ConversationAttachment) { a.ByteCount = MaxAttachmentBytes + 1 },
		func(a *ConversationAttachment) { a.SHA256 = "missing" },
		func(a *ConversationAttachment) { a.UnavailableReason = "missing file" },
	} {
		bad := attachment
		change(&bad)
		if err := ValidateAttachments([]ConversationAttachment{bad}); err == nil {
			t.Fatalf("accepted %#v", bad)
		}
	}
	attachment.ByteCount = 0
	attachment.SHA256 = ""
	attachment.UnavailableReason = "电脑端原图已删除"
	if err := ValidateAttachments([]ConversationAttachment{attachment}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAttachments([]ConversationAttachment{attachment, attachment}); err == nil {
		t.Fatal("duplicate IDs accepted")
	}
}

func TestAttachmentChunkRequiresExactRangeEncodingAndHash(t *testing.T) {
	data := []byte{1, 2, 3}
	hash := sha256.Sum256(data)
	id := strings.Repeat("a", 64)
	response := map[string]any{"attachmentId": id, "offset": 0, "totalBytes": 3, "mimeType": "image/png", "sha256": hex.EncodeToString(hash[:]), "data": base64.StdEncoding.EncodeToString(data), "hasMore": false}
	body, _ := json.Marshal(response)
	if _, err := DecodeAttachmentChunk(body, id, 0, MaxAttachmentChunkBytes); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]any){
		func(v map[string]any) { delete(v, "offset") },
		func(v map[string]any) { delete(v, "hasMore") },
		func(v map[string]any) { v["attachmentId"] = strings.Repeat("b", 64) },
		func(v map[string]any) { v["offset"] = 1 },
		func(v map[string]any) { v["hasMore"] = true },
		func(v map[string]any) { v["data"] = "AQI=" },
		func(v map[string]any) { v["data"] = "AQID\n" },
		func(v map[string]any) { v["sha256"] = strings.Repeat("b", 64) },
	} {
		bad := map[string]any{}
		for k, v := range response {
			bad[k] = v
		}
		change(bad)
		body, _ := json.Marshal(bad)
		if _, err := DecodeAttachmentChunk(body, id, 0, MaxAttachmentChunkBytes); err == nil {
			t.Fatalf("accepted %#v", bad)
		}
	}
	if _, err := DecodeAttachmentChunk(body, id, 0, MaxAttachmentChunkBytes+1); err == nil {
		t.Fatal("oversized request accepted")
	}
}
