package protocol

import "testing"

func TestMessageValidation(t *testing.T) {
	valid := `{"messages":[{"threadId":"t","messageId":"m","role":"user","text":"  code\n","createdAt":1,"position":0}]}`
	messages, err := DecodeMessages([]byte(valid))
	if err != nil || messages[0].Text != "  code\n" {
		t.Fatalf("text changed: %#v %v", messages, err)
	}
	for _, invalid := range []string{
		`{}`, `{"messages":null}`, `{"messages":[{"threadId":"t","messageId":"m","role":"system","text":"x","createdAt":1}]}`,
		`{"messages":[{"threadId":"t","messageId":"m","role":"user","text":"x","createdAt":0}]}`,
		`{"messages":[{"threadId":"t","messageId":"m","role":"user","text":"x","createdAt":1,"position":-1}]}`,
	} {
		if _, err := DecodeMessages([]byte(invalid)); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}
