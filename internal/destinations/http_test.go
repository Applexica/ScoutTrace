package destinations_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webhookscout/scouttrace/internal/destinations"
	"github.com/webhookscout/scouttrace/internal/destinations/httpdest"
	"github.com/webhookscout/scouttrace/internal/event"
)

func newBatch() destinations.Batch {
	one, _ := json.Marshal(map[string]any{
		"id":     "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		"schema": event.SchemaVersion,
	})
	return destinations.Batch{
		ID:         "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Events:     []json.RawMessage{one},
		PreparedAt: time.Now(),
	}
}

func TestHTTPStatus2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	a, err := httpdest.New(httpdest.Config{Name: "x", URL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res := a.Send(context.Background(), newBatch())
	if !res.OK {
		t.Fatalf("expected OK, got %+v", res)
	}
}

func TestHTTPStatus500Retriable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	a, _ := httpdest.New(httpdest.Config{Name: "x", URL: srv.URL}, nil)
	res := a.Send(context.Background(), newBatch())
	if res.OK || !res.Retriable {
		t.Fatalf("expected retriable, got %+v", res)
	}
}

func TestHTTP429RetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	a, _ := httpdest.New(httpdest.Config{Name: "x", URL: srv.URL}, nil)
	res := a.Send(context.Background(), newBatch())
	if res.OK || !res.Retriable {
		t.Fatalf("expected retriable, got %+v", res)
	}
	if res.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s", res.RetryAfter)
	}
}

func TestHTTP403NonRetriable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()
	a, _ := httpdest.New(httpdest.Config{Name: "x", URL: srv.URL}, nil)
	res := a.Send(context.Background(), newBatch())
	if res.OK || res.Retriable {
		t.Fatalf("expected non-retriable, got %+v", res)
	}
}

func TestHTTPHeadersAndIdempotency(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("Idempotency-Key") == "" {
			t.Errorf("missing Idempotency-Key")
		}
		if r.Header.Get("X-Custom") != "yes" {
			t.Errorf("missing custom header")
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	a, _ := httpdest.New(httpdest.Config{
		Name:    "x",
		URL:     srv.URL,
		Headers: map[string]string{"X-Custom": "yes"},
	}, nil)
	if r := a.Send(context.Background(), newBatch()); !r.OK {
		t.Fatalf("send failed: %+v", r)
	}
	if hits != 1 {
		t.Errorf("hits = %d, want 1", hits)
	}
}
