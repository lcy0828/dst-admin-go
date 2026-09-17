package networkprobe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dont/shared"
)

func TestDetectorUsesFirstValidPublicAddress(t *testing.T) {
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("192.168.2.42\n"))
	}))
	defer private.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("203.0.113.42\n"))
	}))
	defer public.Close()

	address, err := (Detector{Client: public.Client(), Providers: []string{private.URL, public.URL}}).Detect(context.Background(), shared.RuntimeNetworkRegionGlobal)
	if err != nil || address.String() != "203.0.113.42" {
		t.Fatalf("address=%q err=%v", address, err)
	}
}

func TestDetectorRejectsPrivateAndOversizedResponses(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "::1", "not-an-ip"} {
		if _, err := ParsePublicAddress(value); err == nil {
			t.Fatalf("accepted non-public address %q", value)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range maximumResponseBytes + 1 {
			_, _ = w.Write([]byte("x"))
		}
	}))
	defer server.Close()
	if _, err := (Detector{Client: server.Client(), Providers: []string{server.URL}}).Detect(context.Background(), shared.RuntimeNetworkRegionGlobal); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func TestDetectorHonorsContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte("203.0.113.42"))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := (Detector{Client: server.Client(), Providers: []string{server.URL}}).Detect(ctx, shared.RuntimeNetworkRegionGlobal); err == nil {
		t.Fatal("canceled request unexpectedly succeeded")
	}
}

func TestDetectorRoutesProvidersByRegion(t *testing.T) {
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("当前 IP：203.0.113.42 来自于：中国"))
	}))
	defer cn.Close()
	global := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("198.51.100.24"))
	}))
	defer global.Close()
	detector := Detector{Client: cn.Client(), RegionProviders: map[shared.RuntimeNetworkRegion][]string{
		shared.RuntimeNetworkRegionCN:     {cn.URL},
		shared.RuntimeNetworkRegionGlobal: {global.URL},
	}}

	cnAddress, cnErr := detector.Detect(context.Background(), shared.RuntimeNetworkRegionCN)
	globalAddress, globalErr := detector.Detect(context.Background(), shared.RuntimeNetworkRegionGlobal)
	if cnErr != nil || cnAddress.String() != "203.0.113.42" {
		t.Fatalf("cn address=%q err=%v", cnAddress, cnErr)
	}
	if globalErr != nil || globalAddress.String() != "198.51.100.24" {
		t.Fatalf("global address=%q err=%v", globalAddress, globalErr)
	}
}

func TestParseProviderAddressSupportsJSON(t *testing.T) {
	address, err := ParseProviderAddress([]byte(`{"ip":"203.0.113.42","addr":"中国"}`))
	if err != nil || address.String() != "203.0.113.42" {
		t.Fatalf("address=%q err=%v", address, err)
	}
}

func TestDetectorRejectsUnknownRegion(t *testing.T) {
	if _, err := (Detector{}).Detect(context.Background(), shared.RuntimeNetworkRegion("unknown")); err == nil {
		t.Fatal("unknown region unexpectedly succeeded")
	}
}
