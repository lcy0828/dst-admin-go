package kubernetesruntime

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRESTClientObservesOnlyTypedManagedResources(t *testing.T) {
	requests := make([]string, 0)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected request method/auth: %s %q", request.Method, request.Header.Get("Authorization"))
		}
		requests = append(requests, request.URL.String())
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/pods") {
			_, _ = writer.Write([]byte(`{"items":[]}`))
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "token")
	caPath := filepath.Join(directory, "ca.crt")
	if err := os.WriteFile(tokenPath, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(server.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := testProvider()
	client, err := NewRESTClient(provider, RESTConfig{APIServer: server.URL, BearerTokenFile: tokenPath, CAFile: caPath})
	if err != nil {
		t.Fatal(err)
	}
	ref := ShardRef{ProviderID: provider.ID, RoomID: "room-one", WorldID: "master"}
	observation, err := client.Observe(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Ref != ref || observation.ObservedAt.IsZero() || !observation.CPU.Stale || observation.Pod.Exists {
		t.Fatalf("observation = %#v", observation)
	}
	if len(requests) != 6 {
		t.Fatalf("request count = %d, requests=%#v", len(requests), requests)
	}
	if _, err := client.Apply(context.Background(), TypedMutation{}); !errors.Is(err, ErrMutationDisabled) {
		t.Fatalf("Apply error = %v", err)
	}
}

func TestRESTClientRejectsInsecureOrInlineConnectionMaterial(t *testing.T) {
	provider := testProvider()
	for _, config := range []RESTConfig{
		{APIServer: "http://cluster.example", BearerTokenFile: "/token", CAFile: "/ca"},
		{APIServer: "https://user:secret@cluster.example", BearerTokenFile: "/token", CAFile: "/ca"},
		{APIServer: "https://cluster.example/api?token=secret", BearerTokenFile: "/token", CAFile: "/ca"},
		{APIServer: "https://cluster.example", BearerTokenFile: "token", CAFile: "ca"},
	} {
		if _, err := NewRESTClient(provider, config); err == nil {
			t.Fatalf("unsafe REST config accepted: %#v", config)
		}
	}
}

func TestRESTClientRejectsInvalidProviderBeforeReadingConnectionFiles(t *testing.T) {
	provider := testProvider()
	provider.RuntimeImage = "registry.example.com/dst/runtime:latest"
	_, err := NewRESTClient(provider, RESTConfig{
		APIServer:       "https://cluster.example",
		BearerTokenFile: "/does/not/exist/token",
		CAFile:          "/does/not/exist/ca",
	})
	if !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("NewRESTClient error = %v", err)
	}
}

func TestParseGiBUsesConservativeBinaryUnits(t *testing.T) {
	tests := map[string]int64{
		"20Gi":   20,
		"2Ti":    2048,
		"2048Mi": 2,
		"1024Ki": 0,
		"1.5Ti":  0,
		"20G":    0,
		"-1Gi":   0,
	}
	for value, expected := range tests {
		if actual := parseGiB(value); actual != expected {
			t.Errorf("parseGiB(%q) = %d, want %d", value, actual, expected)
		}
	}
}
