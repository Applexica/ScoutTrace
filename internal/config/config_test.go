package config

import (
	"strings"
	"testing"
)

func TestParseValidFull(t *testing.T) {
	src := `{
		"schema_version": 1,
		"default_destination": "default",
		"destinations": [
			{"name":"default","type":"http","url":"https://example.com/in","auth_header_ref":"env://X_TOKEN"},
			{"name":"local","type":"file","path":"/tmp/events.ndjson"}
		],
		"servers": [{"name_glob":"*","destination":"default"}]
	}`
	c, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.LookupDestination("default") == nil {
		t.Errorf("default destination not resolved")
	}
}

func TestParseRejectsPlaintextAuth(t *testing.T) {
	src := `{
		"destinations":[
			{"name":"bad","type":"http","url":"https://x","headers":{"Authorization":"Bearer secret"}}
		]
	}`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("expected error")
	}
	e, ok := err.(*Error)
	if !ok || e.Code != ErrCodePlaintextAuth {
		t.Fatalf("err = %v, want code %s", err, ErrCodePlaintextAuth)
	}
}

func TestParseRejectsCredentialShapedHeader(t *testing.T) {
	src := `{
		"destinations":[
			{"name":"bad","type":"http","url":"https://x","headers":{"X-Api-Key":"abc"}}
		]
	}`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), ErrCodePlaintextAuth) {
		t.Fatalf("err = %v, want PLAINTEXT_AUTH", err)
	}
}

func TestParseRejectsBadAuthHeaderRef(t *testing.T) {
	src := `{
		"destinations":[
			{"name":"bad","type":"http","url":"https://x","auth_header_ref":"plain-string"}
		]
	}`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("expected error")
	}
	e, ok := err.(*Error)
	if !ok || e.Code != ErrCodeConfigRefInvalid {
		t.Fatalf("err = %v, want CONFIG_REF_INVALID", err)
	}
}

func TestParseRejectsUnknownDestination(t *testing.T) {
	src := `{"default_destination":"missing","destinations":[{"name":"a","type":"stdout"}]}`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("expected error")
	}
	e, ok := err.(*Error)
	if !ok || e.Code != ErrCodeDestNotFound {
		t.Fatalf("err = %v, want DEST_NOT_FOUND", err)
	}
}

func TestParseUnknownTopLevelField(t *testing.T) {
	src := `{"unknown":1}`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("expected error on unknown field")
	}
}

func TestParseDuplicateDestName(t *testing.T) {
	src := `{
		"destinations":[
			{"name":"a","type":"stdout"},
			{"name":"a","type":"stdout"}
		]
	}`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("expected duplicate-name error")
	}
}

func TestSecretValueRejected(t *testing.T) {
	src := `{
		"destinations":[
			{"name":"bad","type":"http","url":"https://x","headers":{"Custom":"AKIAABCDEFGHIJKLMNOP"}}
		]
	}`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), ErrCodePlaintextAuth) {
		t.Fatalf("err = %v, want PLAINTEXT_AUTH", err)
	}
}
