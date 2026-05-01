// Package httpdest implements the HTTP destination adapter.
package httpdest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/webhookscout/scouttrace/internal/destinations"
)

// Config configures an HTTP adapter.
type Config struct {
	Name          string
	URL           string
	Headers       map[string]string
	AuthHeaderRef string // resolved via Resolver
	TimeoutMS     int
	DialTimeoutMS int
	TLSTimeoutMS  int
	UseGzip       bool
	BodyEnvelope  func(events []byte) ([]byte, error) // optional override
}

// Adapter implements destinations.Adapter for HTTP.
type Adapter struct {
	cfg     Config
	client  *http.Client
	headers map[string]string
	auth    string
}

// New returns a configured HTTP adapter. The resolver is consulted once
// here so credentials live only as long as the adapter does.
func New(cfg Config, res destinations.Resolver) (*Adapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("httpdest: url required")
	}
	if cfg.TimeoutMS == 0 {
		cfg.TimeoutMS = 5000
	}
	a := &Adapter{
		cfg:     cfg,
		client:  &http.Client{Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond},
		headers: cfg.Headers,
	}
	if cfg.AuthHeaderRef != "" {
		if res == nil {
			return nil, errors.New("httpdest: auth_header_ref set but no resolver")
		}
		v, err := res.Resolve(cfg.AuthHeaderRef)
		if err != nil {
			return nil, fmt.Errorf("httpdest: resolve auth: %w", err)
		}
		a.auth = v
	}
	return a, nil
}

// Name returns the adapter name.
func (a *Adapter) Name() string { return a.cfg.Name }

// Type returns "http".
func (a *Adapter) Type() string { return "http" }

// Send transmits the batch and translates the response per §11.2.
func (a *Adapter) Send(ctx context.Context, b destinations.Batch) destinations.Result {
	bodyBytes, err := json.Marshal(struct {
		Schema string `json:"schema"`
		Events any    `json:"events"`
	}{
		Schema: "scouttrace.toolcall.v1",
		Events: b.Events,
	})
	if err != nil {
		return destinations.Result{Err: err}
	}
	if a.cfg.BodyEnvelope != nil {
		if alt, err := a.cfg.BodyEnvelope(bodyBytes); err == nil {
			bodyBytes = alt
		} else {
			return destinations.Result{Err: err}
		}
	}

	var buf bytes.Buffer
	if a.cfg.UseGzip {
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write(bodyBytes); err != nil {
			return destinations.Result{Err: err}
		}
		if err := gz.Close(); err != nil {
			return destinations.Result{Err: err}
		}
	} else {
		buf.Write(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.URL, &buf)
	if err != nil {
		return destinations.Result{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.UseGzip {
		req.Header.Set("Content-Encoding", "gzip")
	}
	req.Header.Set("Idempotency-Key", b.ID)
	req.Header.Set("User-Agent", "ScoutTrace/0.1.0")
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}
	if a.auth != "" {
		req.Header.Set("Authorization", a.auth)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return destinations.Result{Retriable: true, Err: err}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return interpret(resp)
}

// Close releases the HTTP client (no-op for stdlib transport).
func (a *Adapter) Close() error { return nil }

func interpret(resp *http.Response) destinations.Result {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return destinations.Result{OK: true, Status: resp.StatusCode}
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooEarly,
		resp.StatusCode == http.StatusTooManyRequests:
		return destinations.Result{Retriable: true, Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp)}
	case resp.StatusCode >= 500:
		return destinations.Result{Retriable: true, Status: resp.StatusCode}
	default:
		return destinations.Result{
			Status: resp.StatusCode,
			Err:    fmt.Errorf("httpdest: non-retriable status %d", resp.StatusCode),
		}
	}
}

func parseRetryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}
