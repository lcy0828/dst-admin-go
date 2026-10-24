package mods

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

type NetworkError struct {
	Code       string `json:"code"`
	Status     int    `json:"upstreamStatus,omitempty"`
	RetryAfter int    `json:"retryAfterSeconds,omitempty"`
}

func (e *NetworkError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s (HTTP %d)", e.Code, e.Status)
	}
	return e.Code
}

func safeNetworkError(err error) error {
	if err == nil {
		return nil
	}
	var known *NetworkError
	if errors.As(err, &known) {
		return known
	}
	code := "WORKSHOP_CONNECTION_FAILED"
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		code = "WORKSHOP_CANCELED"
	case errors.Is(err, context.DeadlineExceeded):
		code = "WORKSHOP_TIMEOUT"
	case errors.As(err, &dns):
		code = "WORKSHOP_DNS_FAILED"
	case errors.As(err, &cert), errors.As(err, &unknown):
		code = "WORKSHOP_TLS_FAILED"
	case errors.As(err, &netErr) && netErr.Timeout():
		code = "WORKSHOP_TIMEOUT"
	}
	return &NetworkError{Code: code}
}

type networkKindKey struct{}
type directImageKey struct{}
type optionalRequestKey struct{}
type connectionTestKey struct{}
type networkFailure struct {
	until time.Time
	err   error
}

type networkLimits struct {
	slots    [3]chan struct{}
	waiting  chan struct{}
	mu       sync.Mutex
	cooldown map[string]time.Time
	failures map[string]networkFailure
}

func newNetworkLimits() *networkLimits {
	return &networkLimits{slots: [3]chan struct{}{make(chan struct{}, 2), make(chan struct{}, 1), make(chan struct{}, 1)}, waiting: make(chan struct{}, 32), cooldown: make(map[string]time.Time), failures: make(map[string]networkFailure)}
}

type workshopTransport struct {
	base   *http.Transport
	limits *networkLimits
	scope  string
}

func newWorkshopClient(c NetworkConfig, limits *networkLimits) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.MaxConnsPerHost = 4, 2, 4
	transport.IdleConnTimeout, transport.ResponseHeaderTimeout = 30*time.Second, 5*time.Second
	transport.TLSHandshakeTimeout = 4 * time.Second
	dialer := &net.Dialer{Timeout: 4 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if direct, _ := ctx.Value(directImageKey{}).(bool); direct {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, candidate := range addresses {
				if !candidate.IP.IsGlobalUnicast() || candidate.IP.IsPrivate() || candidate.IP.IsLoopback() || candidate.IP.IsLinkLocalUnicast() {
					return nil, &NetworkError{Code: "WORKSHOP_IMAGE_SOURCE_REJECTED"}
				}
			}
			if len(addresses) == 0 {
				return nil, &NetworkError{Code: "WORKSHOP_DNS_FAILED"}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
		}
		return dialer.DialContext(ctx, network, address)
	}
	switch c.ProxyMode {
	case "direct":
		transport.Proxy = nil
	case "custom":
		proxy, _ := url.Parse(c.ProxyURL)
		transport.Proxy = http.ProxyURL(proxy)
	}
	return &http.Client{Transport: &workshopTransport{base: transport, limits: limits, scope: c.scope()}, Timeout: 8 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return &NetworkError{Code: "WORKSHOP_REDIRECT_REJECTED"}
			}
			first := via[0]
			// API keys may be in query parameters or POST bodies. Never redirect
			// an API request to another origin or replay it after a downgrade.
			if req.URL.Scheme != first.URL.Scheme || req.URL.Host != first.URL.Host {
				return &NetworkError{Code: "WORKSHOP_REDIRECT_REJECTED"}
			}
			return nil
		},
	}
}

func (t *workshopTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

func (t *workshopTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	kind, _ := req.Context().Value(networkKindKey{}).(int)
	key := t.scope + strconv.Itoa(kind)
	t.limits.mu.Lock()
	until := t.limits.cooldown[key]
	if !until.After(time.Now()) {
		delete(t.limits.cooldown, key)
	}
	failure := t.limits.failures[key]
	if !failure.until.After(time.Now()) {
		delete(t.limits.failures, key)
	}
	t.limits.mu.Unlock()
	if until.After(time.Now()) {
		return nil, &NetworkError{Code: "WORKSHOP_RATE_LIMITED", Status: 429, RetryAfter: max(1, int(time.Until(until).Seconds()))}
	}
	probe, _ := req.Context().Value(connectionTestKey{}).(bool)
	if !probe && failure.until.After(time.Now()) {
		return nil, failure.err
	}
	optional, _ := req.Context().Value(optionalRequestKey{}).(bool)
	if optional && kind == 0 {
		t.limits.mu.Lock()
		// Reserve at least one JSON slot for foreground work.
		if len(t.limits.slots[0]) > 0 {
			t.limits.mu.Unlock()
			return nil, &NetworkError{Code: "WORKSHOP_BUSY"}
		}
		select {
		case t.limits.slots[0] <- struct{}{}:
		default:
			t.limits.mu.Unlock()
			return nil, &NetworkError{Code: "WORKSHOP_BUSY"}
		}
		t.limits.mu.Unlock()
	} else {
		select {
		case t.limits.waiting <- struct{}{}:
		default:
			return nil, &NetworkError{Code: "WORKSHOP_BUSY"}
		}
		select {
		case t.limits.slots[kind] <- struct{}{}:
			<-t.limits.waiting
		case <-req.Context().Done():
			<-t.limits.waiting
			return nil, safeNetworkError(req.Context().Err())
		}
	}
	release := func() { <-t.limits.slots[kind] }
	if kind == 2 {
		var proxy *url.URL
		if t.base.Proxy != nil {
			proxy, _ = t.base.Proxy(req)
		}
		if proxy == nil {
			req = req.WithContext(context.WithValue(req.Context(), directImageKey{}, true))
		}
	}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		release()
		failure := safeNetworkError(err)
		if !optional && !errors.Is(err, context.Canceled) {
			t.recordFailure(key, failure)
		}
		return nil, failure
	}
	if response.StatusCode >= 500 {
		t.recordFailure(key, &NetworkError{Code: "WORKSHOP_UPSTREAM_REJECTED", Status: response.StatusCode})
	}
	response.Body = &limitedResponseBody{ReadCloser: response.Body, release: release}
	if response.StatusCode == 429 {
		seconds := 60
		if n, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil {
			seconds = n
		} else if date, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
			seconds = int(time.Until(date).Seconds())
		}
		seconds = max(1, min(seconds, 86400))
		t.limits.mu.Lock()
		// Bound state even when administrators repeatedly edit the configuration.
		if len(t.limits.cooldown) >= 32 {
			for k, deadline := range t.limits.cooldown {
				if !deadline.After(time.Now()) {
					delete(t.limits.cooldown, k)
				}
			}
			if len(t.limits.cooldown) >= 32 {
				for k := range t.limits.cooldown {
					delete(t.limits.cooldown, k)
					break
				}
			}
		}
		t.limits.cooldown[key] = time.Now().Add(time.Duration(seconds) * time.Second)
		t.limits.mu.Unlock()
		response.Body.Close()
		return nil, &NetworkError{Code: "WORKSHOP_RATE_LIMITED", Status: 429, RetryAfter: seconds}
	}
	return response, nil
}

type limitedResponseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *limitedResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

func (t *workshopTransport) recordFailure(key string, err error) {
	t.limits.mu.Lock()
	defer t.limits.mu.Unlock()
	if len(t.limits.failures) >= 32 {
		for key := range t.limits.failures {
			delete(t.limits.failures, key)
			break
		}
	}
	t.limits.failures[key] = networkFailure{until: time.Now().Add(10 * time.Second), err: err}
}
