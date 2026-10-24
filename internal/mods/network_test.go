package mods

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkshopSOCKS5hResolvesTheDestinationOnTheProxy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	destination := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		header := make([]byte, 2)
		if _, err = io.ReadFull(conn, header); err != nil {
			return
		}
		methods := make([]byte, int(header[1]))
		if _, err = io.ReadFull(conn, methods); err != nil {
			return
		}
		conn.Write([]byte{5, 0})
		header = make([]byte, 5)
		if _, err = io.ReadFull(conn, header); err != nil || header[3] != 3 {
			return
		}
		host := make([]byte, int(header[4]))
		if _, err = io.ReadFull(conn, host); err != nil {
			return
		}
		port := make([]byte, 2)
		io.ReadFull(conn, port)
		destination <- string(host)
		conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
		request, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		request.Body.Close()
		fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	}()
	config, _ := (NetworkConfig{ProxyMode: "custom", ProxyURL: "socks5h://" + listener.Addr().String()}).Normalize()
	client := newWorkshopClient(config, newNetworkLimits())
	defer client.CloseIdleConnections()
	response, err := client.Get("http://dst-admin-never-resolves.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case host := <-destination:
		if host != "dst-admin-never-resolves.invalid" {
			t.Fatal(host)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not receive hostname")
	}
}

func TestWorkshopRejectsCrossOriginRedirectWithoutSendingKey(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/?key=private-key", http.StatusFound)
	}))
	defer first.Close()
	config, _ := (NetworkConfig{ProxyMode: "direct"}).Normalize()
	client := newWorkshopClient(config, newNetworkLimits())
	defer client.CloseIdleConnections()
	_, err := client.Get(first.URL + "/?key=private-key")
	if err == nil || leaked.Load() != 0 || strings.Contains(safeNetworkError(err).Error(), "private-key") {
		t.Fatalf("redirect=%v count=%d", safeNetworkError(err), leaked.Load())
	}
}

func TestWorkshopImageCacheAndInvalidContent(t *testing.T) {
	w, err := NewWorkshop(NetworkConfig{}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var data bytes.Buffer
	png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	w.writeImage("test.thumb", data.Bytes())
	value, ok := w.readImage("test.thumb")
	if !ok || value.ContentType != "image/png" || value.ETag == "" {
		t.Fatalf("image=%+v hit=%v", value, ok)
	}
	if _, err := thumbnail([]byte("<html>Access denied</html>")); err == nil {
		t.Fatal("cached HTML as an image")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Thumbnail(ctx, "100", 184); err == nil {
		t.Fatal("ignored canceled caller")
	}
}

func TestWorkshopRequestLimitsRemainSharedAfterConfigurationChanges(t *testing.T) {
	var active, peak atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if count <= old || peak.CompareAndSwap(old, count) {
				break
			}
		}
		started <- struct{}{}
		<-release
		fmt.Fprint(w, `{"response":{"total":0,"publishedfiledetails":[]}}`)
	}))
	defer server.Close()
	w := testWorkshop(t, server, "private-key")
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		if i == 2 {
			commit, discard, err := w.Prepare(w.snapshot().config)
			if err != nil {
				t.Fatal(err)
			}
			commit()
			discard()
		}
		go func(i int) {
			_, err := w.Search(context.Background(), SearchOptions{Query: fmt.Sprint(i, " query"), Page: 1, PageSize: 20})
			results <- err
		}(i)
		if i < 2 {
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("request did not start")
			}
		}
	}
	close(release)
	for i := 0; i < 4; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() > 2 {
		t.Fatalf("API concurrent requests=%d", peak.Load())
	}
}

func TestWorkshopAPIPrefixDoesNotShareCommunityRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/IPublishedFileService/") {
			w.Header().Set("Retry-After", "120")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `<html><body><div>1 entries matching filters</div><a href="https://steamcommunity.com/sharedfiles/filedetails/?id=100"><img alt="Public result"></a></body></html>`)
	}))
	defer server.Close()
	w, err := NewWorkshop(NetworkConfig{APIBase: server.URL + "/workshop/proxy", CommunityBase: server.URL, ProxyMode: "direct", APIKey: "private-key"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	_, err = w.Search(context.Background(), SearchOptions{Query: "test", Page: 1, PageSize: 20})
	var failure *NetworkError
	if !errors.As(err, &failure) || failure.Code != "WORKSHOP_RATE_LIMITED" {
		t.Fatalf("API limit: %v", err)
	}
	result, err := w.Search(context.Background(), SearchOptions{Query: "test", Page: 1, PageSize: 20, Source: "community"})
	if err != nil || len(result.Items) != 1 {
		t.Fatalf("API prefix blocked unrelated community requests: %+v %v", result, err)
	}
}

func TestWorkshopNetworkFailuresCoolDownButExplicitTestCanProbe(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	w := testWorkshop(t, server, "private-key")
	for _, query := range []string{"first", "second"} {
		if _, err := w.Search(context.Background(), SearchOptions{Query: query, Page: 1, PageSize: 20}); err == nil {
			t.Fatal("failure was presented as success")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("ordinary requests ignored cooldown: %d", calls.Load())
	}
	result, err := w.Test(context.Background(), w.snapshot().config, "")
	if err != nil || len(result.Stages) != 3 || result.Stages[0].Status != "unavailable" || calls.Load() != 2 {
		t.Fatalf("explicit probe did not retry: %+v, %v, calls=%d", result, err, calls.Load())
	}
}
