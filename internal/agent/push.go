package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Outcome says what to do with the records a push carried.
type Outcome int

const (
	// Delivered: acknowledge them.
	OutcomeOK Outcome = iota
	// Transient (network, 5xx, 429): keep them and retry with backoff.
	OutcomeRetry
	// The backend will never accept them (400): drop them, or they would
	// block everything queued behind them.
	OutcomeDrop
	// Too large (413): retry with fewer records.
	OutcomeTooLarge
	// Key unknown (401) or server deleted (410): stop pushing.
	OutcomeRevoked
	// Invalid (422): something in the batch is refused. Retry with fewer
	// records until the culprit is alone, so it does not cost the rest.
	OutcomeInvalid
)

func (o Outcome) String() string {
	return [...]string{"ok", "retrying", "rejected", "too large", "revoked", "invalid"}[o]
}

// Classify maps an HTTP status to an Outcome (0 = no response at all).
func Classify(status int) Outcome {
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		return OutcomeOK
	case status == http.StatusUnauthorized || status == http.StatusGone:
		return OutcomeRevoked
	case status == http.StatusRequestEntityTooLarge:
		return OutcomeTooLarge
	case status == http.StatusUnprocessableEntity:
		return OutcomeInvalid
	case status == 0, status == http.StatusRequestTimeout, status == http.StatusTooEarly,
		status == http.StatusTooManyRequests, status >= 500:
		return OutcomeRetry
	}
	return OutcomeDrop
}

// Pusher posts batches to the ingestion API.
type Pusher struct {
	URL       string
	Key       string
	UserAgent string
	HTTP      *http.Client
}

func NewPusher(url, key, version string) *Pusher {
	return &Pusher{
		URL:       strings.TrimRight(url, "/") + "/v1/servers/metrics/",
		Key:       key,
		UserAgent: "infinianalytics-agent/" + version,
		HTTP:      &http.Client{Timeout: 20 * time.Second},
	}
}

// PushError carries the backend's answer for logs and `status`.
type PushError struct {
	Status int
	Detail string
	Err    error
}

func (e *PushError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Detail != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// Push sends one gzip-compressed batch.
func (p *Pusher) Push(ctx context.Context, b Batch) (PushResponse, Outcome, error) {
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	if err := json.NewEncoder(zw).Encode(b); err != nil {
		return PushResponse{}, OutcomeDrop, err
	}
	if err := zw.Close(); err != nil {
		return PushResponse{}, OutcomeDrop, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, &body)
	if err != nil {
		return PushResponse{}, OutcomeRetry, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Agent-Key", p.Key)
	req.Header.Set("User-Agent", p.UserAgent)

	resp, err := p.HTTP.Do(req)
	if err != nil {
		return PushResponse{}, OutcomeRetry, &PushError{Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	outcome := Classify(resp.StatusCode)
	if outcome != OutcomeOK {
		var answer struct {
			Detail string `json:"detail"`
			Code   string `json:"code"`
		}
		_ = json.Unmarshal(raw, &answer)
		detail := answer.Detail
		if answer.Code != "" {
			detail = answer.Code + ": " + detail
		}
		return PushResponse{}, outcome, &PushError{Status: resp.StatusCode, Detail: detail}
	}
	var out PushResponse
	_ = json.Unmarshal(raw, &out)
	return out, OutcomeOK, nil
}
