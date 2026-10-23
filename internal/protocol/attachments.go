package protocol

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	MaxAttachmentBytes      = 25 * 1024 * 1024
	MaxAttachmentChunkBytes = 196608
)

type ConversationAttachment struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	MIMEType          string `json:"mimeType"`
	ByteCount         int64  `json:"byteCount"`
	SHA256            string `json:"sha256"`
	UnavailableReason string `json:"unavailableReason,omitempty"`
}

type AttachmentChunk struct {
	AttachmentID string `json:"attachmentId"`
	Offset       int64  `json:"offset"`
	TotalBytes   int64  `json:"totalBytes"`
	MIMEType     string `json:"mimeType"`
	SHA256       string `json:"sha256"`
	Data         string `json:"data"`
	HasMore      bool   `json:"hasMore"`
}

func IsAttachmentID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isImageMIME(value string) bool {
	return oneOf(value, "image/png", "image/jpeg", "image/gif", "image/webp", "image/heic", "image/heif", "image/avif")
}

func ValidateAttachments(attachments []ConversationAttachment) error {
	if len(attachments) > 16 {
		return errors.New("too many message attachments")
	}
	seen := make(map[string]bool, len(attachments))
	for _, item := range attachments {
		if !IsAttachmentID(item.ID) || seen[item.ID] {
			return errors.New("attachment id is invalid")
		}
		seen[item.ID] = true
		if _, err := required(item.Name, "attachment name", 200); err != nil {
			return err
		}
		if !isImageMIME(item.MIMEType) {
			return errors.New("attachment MIME type is unsupported")
		}
		if item.UnavailableReason != "" {
			if _, err := required(item.UnavailableReason, "unavailableReason", 500); err != nil {
				return err
			}
			if item.ByteCount != 0 || item.SHA256 != "" {
				return errors.New("unavailable attachment contains file metadata")
			}
		} else if item.ByteCount < 1 || item.ByteCount > MaxAttachmentBytes || !IsAttachmentID(item.SHA256) {
			return errors.New("attachment size or SHA-256 is invalid")
		}
	}
	return nil
}

func DecodeAttachmentChunk(data []byte, attachmentID string, offset int64, length int64) (AttachmentChunk, error) {
	var chunk AttachmentChunk
	// Required fields use pointers here: missing offset or hasMore must not silently
	// acquire a valid zero value, especially for a single-chunk file.
	var raw struct {
		AttachmentID string `json:"attachmentId"`
		Offset       *int64 `json:"offset"`
		TotalBytes   int64  `json:"totalBytes"`
		MIMEType     string `json:"mimeType"`
		SHA256       string `json:"sha256"`
		Data         string `json:"data"`
		HasMore      *bool  `json:"hasMore"`
	}
	if err := decodeOne(data, &raw); err != nil {
		return chunk, err
	}
	if !IsAttachmentID(attachmentID) || offset < 0 || offset >= MaxAttachmentBytes || length < 1 || length > MaxAttachmentChunkBytes {
		return chunk, errors.New("attachment request is invalid")
	}
	if raw.AttachmentID != attachmentID || raw.Offset == nil || *raw.Offset != offset || raw.TotalBytes < 1 || raw.TotalBytes > MaxAttachmentBytes || offset >= raw.TotalBytes {
		return chunk, errors.New("attachment response does not match the request")
	}
	if !isImageMIME(raw.MIMEType) || !IsAttachmentID(raw.SHA256) || raw.HasMore == nil {
		return chunk, errors.New("attachment response metadata is invalid")
	}
	expected := min(length, raw.TotalBytes-offset)
	// Reject oversized, non-canonical, and CR/LF-containing base64 before retaining
	// any bytes. There is never a Relay file or database entry for these contents.
	if len(raw.Data) != base64.StdEncoding.EncodedLen(int(expected)) || strings.ContainsAny(raw.Data, "\r\n") {
		return chunk, errors.New("attachment chunk length is invalid")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(raw.Data)
	if err != nil || int64(len(decoded)) != expected || *raw.HasMore != (offset+expected < raw.TotalBytes) {
		return chunk, errors.New("attachment chunk data or hasMore is invalid")
	}
	if offset == 0 && expected == raw.TotalBytes {
		digest := sha256.Sum256(decoded)
		if hex.EncodeToString(digest[:]) != raw.SHA256 {
			return chunk, errors.New("attachment SHA-256 does not match its data")
		}
	}
	return AttachmentChunk{AttachmentID: raw.AttachmentID, Offset: offset, TotalBytes: raw.TotalBytes, MIMEType: raw.MIMEType, SHA256: raw.SHA256, Data: raw.Data, HasMore: *raw.HasMore}, nil
}
