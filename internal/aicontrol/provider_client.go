package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type providerError struct {
	code  string
	cause error
}

func (e *providerError) Error() string { return e.code }
func (e *providerError) Unwrap() error { return e.cause }

func providerStatusError(response *http.Response) error {
	code := fmt.Sprintf("AI_PROVIDER_HTTP_%d", response.StatusCode)
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	if json.Unmarshal(raw, &payload) == nil {
		upstream := strings.ToLower(payload.Error.Code + " " + payload.Error.Type)
		message := strings.ToLower(payload.Error.Message)
		for _, marker := range []string{"pre_consume_token_quota_failed", "insufficient_quota", "quota_exceeded", "billing_hard_limit_reached"} {
			if strings.Contains(upstream, marker) {
				code = "AI_PROVIDER_QUOTA_EXHAUSTED"
				break
			}
		}
		if strings.Contains(message, "token quota is not enough") || strings.Contains(message, "insufficient quota") {
			code = "AI_PROVIDER_QUOTA_EXHAUSTED"
		}
	}
	// Do not expose or log arbitrary provider response text; it may echo secrets.
	log.Printf("[AI][ERROR] provider_rejected status=%d error=%s", response.StatusCode, code)
	return &providerError{code: code}
}

func providerTransportError(ctx context.Context, err error) error {
	code := "AI_PROVIDER_CONNECTION_FAILED"
	var network net.Error
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		code = "AI_REQUEST_CANCELED"
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
		code = "AI_PROVIDER_TIMEOUT"
	}
	return &providerError{code: code, cause: err}
}

// Only retry the model HTTP request. Platform tools run after a valid model
// response and are never replayed by this transport retry.
func requestProvider(ctx context.Context, request *http.Request) (*http.Response, error) {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for attempt := 0; ; attempt++ {
		current := request.Clone(ctx)
		if attempt > 0 {
			body, err := request.GetBody()
			if err != nil {
				return nil, providerTransportError(ctx, err)
			}
			current.Body = body
		}
		started := time.Now()
		response, err := client.Do(current)
		delay, retry := 250*time.Millisecond, false
		if err != nil {
			cause := err
			var requestError *url.Error
			if errors.As(err, &requestError) {
				cause = requestError.Err // Exclude the configured endpoint and all request headers.
			}
			err = providerTransportError(ctx, err)
			detail := []rune(sanitizeText(cause.Error()))
			if len(detail) > 256 {
				detail = detail[:256]
			}
			log.Printf("[AI][ERROR] provider_request attempt=%d bytes=%d duration_ms=%d error=%s cause=%q", attempt+1, request.ContentLength, time.Since(started).Milliseconds(), err, string(detail))
			retry = ctx.Err() == nil
		} else {
			if response.StatusCode != http.StatusOK || time.Since(started) >= 10*time.Second {
				log.Printf("[AI] provider_request attempt=%d bytes=%d duration_ms=%d status=%d", attempt+1, request.ContentLength, time.Since(started).Milliseconds(), response.StatusCode)
			}
			switch response.StatusCode {
			case 429, 500, 502, 503, 504:
				retry = true
			}
			if value := response.Header.Get("Retry-After"); value != "" {
				seconds, parseErr := strconv.Atoi(value)
				if parseErr != nil || seconds < 0 || seconds > 2 {
					retry = false
				} else {
					delay = time.Duration(seconds) * time.Second
				}
			}
		}
		if attempt > 0 || !retry {
			return response, err
		}
		if response != nil {
			response.Body.Close()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, providerTransportError(ctx, fmt.Errorf("model request retry interrupted: %w", ctx.Err()))
		case <-timer.C:
		}
	}
}
