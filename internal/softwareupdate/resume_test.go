package softwareupdate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestInterruptedArchiveResumesAfterReopening(t *testing.T) {
	root := t.TempDir()
	payload := strings.Repeat("package-content", 100)
	size := int64(len(payload))
	asset := Asset{Name: "dst-admin-update-linux-amd64.tar.gz", Size: size, URL: "https://github.com/" + Repository + "/releases/download/v1.3.4/dst-admin-update-linux-amd64.tar.gz"}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	client := NewGitHubClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 200, ContentLength: size, Header: http.Header{}, Body: &cancelBody{data: strings.NewReader(payload[:len(payload)/2]), cancel: cancel}}, nil
		}
		offset := int64(len(payload) / 2)
		if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-", offset) {
			t.Fatalf("not resumed: %v", r.Header)
		}
		return &http.Response{StatusCode: 206, ContentLength: size - offset, Header: http.Header{"Content-Range": []string{fmt.Sprintf("bytes %d-%d/%d", offset, size-1, size)}}, Body: io.NopCloser(strings.NewReader(payload[offset:]))}, nil
	})})
	if f, err := VerifiedArchive(ctx, root, client, asset, "direct", hashBytes([]byte(payload)), nil); err == nil {
		f.Close()
		t.Fatal("interruption accepted")
	}
	var resumed bool
	f, err := VerifiedArchive(context.Background(), root, client, asset, "direct", hashBytes([]byte(payload)), func(p DownloadProgress) { resumed = resumed || p.Resumed })
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != payload || !resumed || calls != 2 {
		t.Fatal("invalid resumed archive", calls, resumed)
	}
}

type cancelBody struct {
	data   *strings.Reader
	cancel context.CancelFunc
}

func (b *cancelBody) Read(p []byte) (int, error) {
	n, err := b.data.Read(p)
	if err == io.EOF {
		b.cancel()
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}
func (b *cancelBody) Close() error { return nil }

func TestRangeIgnoredRestartsAndMismatchedRangeDoesNotAppend(t *testing.T) {
	for _, status := range []int{200, 206} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "partial")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			_, _ = f.WriteString("wrong")
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, ContentLength: 10, Header: http.Header{"Content-Range": []string{"bytes 0-9/10"}}, Body: io.NopCloser(strings.NewReader("0123456789"))}, nil
			})
			err = DownloadHTTPFile(ctx, Asset{Size: 10}, &http.Client{Transport: transport}, "http://relay.test/archive", nil, f, nil)
			_, _ = f.Seek(0, io.SeekStart)
			data, _ := io.ReadAll(f)
			if status == 200 && (err != nil || string(data) != "0123456789") {
				t.Fatal(string(data), err)
			}
			if status == 206 && (err == nil || string(data) != "wrong") {
				t.Fatal("bad range changed bytes", string(data), err)
			}
		})
	}
}

func TestUpdateSettingsPersistMaskAndSeparateCacheSources(t *testing.T) {
	s, client := testService(t, []byte("fixture"))
	settings := DefaultSettings()
	settings.Source = "mirror"
	settings.ProxyMode = "custom"
	settings.ProxyURL = "http://name:password@127.0.0.1:7890"
	saved, err := s.SaveSettings(settings)
	if err != nil || saved.ProxyURL != "" || !saved.ProxyConfigured {
		t.Fatal(saved, err)
	}
	if _, err := s.Check(context.Background(), false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Check(context.Background(), false, "direct"); err != nil {
		t.Fatal(err)
	}
	if client.calls.Load() != 2 {
		t.Fatal("different source reused cache", client.calls.Load())
	}
	saved.TimeoutMinutes = 120
	if _, err := s.SaveSettings(saved); err != nil {
		t.Fatal(err)
	}
	restored := New(s.config)
	actual, err := restored.settings()
	if err != nil || actual.ProxyURL != settings.ProxyURL || actual.TimeoutMinutes != 120 {
		t.Fatal(actual.Public(), err)
	}
}

func TestDisabledBackgroundChecksSleep(t *testing.T) {
	s, client := testService(t, []byte("fixture"))
	settings := DefaultSettings()
	settings.BackgroundCheck = false
	if _, err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunChecks(ctx, nil); close(done) }()
	s.WakeChecks()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	if client.calls.Load() != 0 {
		t.Fatal("disabled checking queried release")
	}
}

func TestBackgroundChecksRunEveryThirtyMinutesWithoutBrowserRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, client := testService(t, []byte("fixture"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			s.RunChecks(ctx, nil)
			close(done)
		}()
		synctest.Wait()
		if client.calls.Load() != 1 {
			t.Fatal("initial background release check did not run")
		}
		time.Sleep(29 * time.Minute)
		synctest.Wait()
		if client.calls.Load() != 1 {
			t.Fatal("background release check ran before its interval")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if client.calls.Load() != 2 {
			t.Fatal("background release check did not repeat after thirty minutes")
		}
		cancel()
		<-done
	})
}

func TestCheckIntervalDefaultsAndSavedHoursRemainCompatible(t *testing.T) {
	s, _ := testService(t, []byte("fixture"))
	defaults, err := s.Settings()
	if err != nil || !defaults.BackgroundCheck || defaults.CheckIntervalHours != 0.5 {
		t.Fatal("new installations must default to thirty-minute checks", defaults, err)
	}
	// A pre-upgrade settings file uses a JSON integer for the interval.
	legacy := []byte(`{"source":"direct","proxyMode":"system","timeoutMinutes":60,"backgroundCheck":false,"checkIntervalHours":6}`)
	if err := os.WriteFile(filepath.Join(s.config.Root, "settings.json"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := New(s.config).Settings()
	if err != nil || actual.CheckIntervalHours != 6 || actual.BackgroundCheck {
		t.Fatal("saved interval and disabled state changed on upgrade", actual, err)
	}
	for _, hours := range []float64{0.5, 1.25, 6, 168} {
		settings := DefaultSettings()
		settings.CheckIntervalHours = hours
		if _, err := s.SaveSettings(settings); err != nil {
			t.Fatal(hours, err)
		}
		actual, err := New(s.config).Settings()
		if err != nil || actual.CheckIntervalHours != hours {
			t.Fatal("saved interval did not survive reopening", hours, actual, err)
		}
	}
	for _, hours := range []float64{0, 0.49, 168.01} {
		settings := DefaultSettings()
		settings.CheckIntervalHours = hours
		if _, err := s.SaveSettings(settings); err == nil {
			t.Fatal("out-of-range interval accepted", hours)
		}
	}
}

func TestCachedReleaseCanNotifyNewSubscriptionsWithoutNetwork(t *testing.T) {
	s, client := testService(t, []byte("fixture"))
	delivered := 0
	s.config.OnRelease = func(Release) { delivered++ }
	if _, err := s.Check(context.Background(), false, "direct"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Check(context.Background(), false, "direct"); err != nil {
		t.Fatal(err)
	}
	if delivered != 2 || client.calls.Load() != 1 {
		t.Fatalf("release signals=%d network=%d", delivered, client.calls.Load())
	}
}

func TestChannelWakeDoesNotPostponeScheduledChecks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, client := testService(t, []byte("fixture"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { s.RunChecks(ctx, nil); close(done) }()
		synctest.Wait()
		time.Sleep(19 * time.Minute)
		s.WakeChecks()
		synctest.Wait()
		if client.calls.Load() != 1 {
			t.Fatal("channel edit bypassed shared cache")
		}
		time.Sleep(11 * time.Minute)
		synctest.Wait()
		if client.calls.Load() != 2 {
			t.Fatal("channel edit postponed the thirty-minute check")
		}
		cancel()
		<-done
	})
}
