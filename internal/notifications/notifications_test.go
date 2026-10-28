package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/jobs"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type testMail struct{ calls atomic.Int32 }

func (m *testMail) SendMail(context.Context, []string, string, string) error {
	m.calls.Add(1)
	return nil
}

func fixture(t *testing.T) (*Service, *Store, *testMail) {
	t.Helper()
	db, err := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "notifications.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	db.LogMode(false)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "test_", filepath.Join(t.TempDir(), "secrets"))
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	mail := &testMail{}
	s, err := New(store, mail)
	if err != nil {
		t.Fatal(err)
	}
	return s, store, mail
}

func webhookInput(url string) ChannelInput {
	return ChannelInput{Name: "Webhook", Type: "webhook", Enabled: true, Events: append([]string{}, EventKinds...), URL: url}
}

func TestCredentialsEncryptedPreservedAndRecoverable(t *testing.T) {
	s, store, _ := fixture(t)
	in := webhookInput("https://example.com/hook?token=private-token")
	in.Headers = map[string]string{"Authorization": "Bearer private-auth"}
	channel, err := s.Save("", in)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := store.record(channel.ID)
	public, _ := json.Marshal(channel)
	for _, forbidden := range []string{"private-token", "private-auth", "example.com"} {
		if strings.Contains(row.Secret+row.Data+string(public), forbidden) {
			t.Fatal("plaintext credentials persisted or returned")
		}
	}
	info, err := os.Stat(filepath.Join(store.root, "webhook.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key permissions: %v %v", info, err)
	}
	in.URL, in.Headers, in.Revision = "", nil, channel.Revision
	updated, err := s.Save(channel.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := store.Secret(channel.ID)
	if err != nil || secret.Headers["Authorization"] != "Bearer private-auth" || !strings.Contains(secret.URL, "private-token") {
		t.Fatal("blank update lost credentials", err)
	}
	if _, err := s.Save(channel.ID, in); !errors.Is(err, ErrConflict) {
		t.Fatal("stale revision accepted", err)
	}
	in.Headers, in.Revision = map[string]string{}, updated.Revision
	updated, err = s.Save(channel.ID, in)
	if err != nil || updated.HeadersConfigured {
		t.Fatal("explicit clear failed", err)
	}
	if err := os.Remove(filepath.Join(store.root, "webhook.key")); err != nil {
		t.Fatal(err)
	}
	in.Revision = updated.Revision
	if _, err := s.Save(channel.ID, in); !errors.Is(err, ErrSecrets) {
		t.Fatal("missing key silently accepted", err)
	}
	in.URL, in.Headers = "https://example.com/reentered", map[string]string{}
	if _, err := s.Save(channel.ID, in); err != nil {
		t.Fatal("cannot reenter credentials after key loss", err)
	}
}

func TestTemplatesKeepMessageAsJSONData(t *testing.T) {
	event := Event{Title: "引号\"", Message: "第一行\n\"},\"injected\":true", ID: "event-1"}
	body, err := render(Channel{Format: "json", Template: `{"text":"{{title}}\n{{message}}","nested":["{{id}}",7,true]}`}, event)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if len(value) != 2 || value["text"] != event.Title+"\n"+event.Message {
		t.Fatal("JSON shape or message changed", string(body))
	}
	for _, mutate := range []func(*ChannelInput){
		func(in *ChannelInput) { in.Template = `{"text":"{{password}}"}` },
		func(in *ChannelInput) { in.Headers = map[string]string{"Authorization": "a", "authorization": "b"} },
		func(in *ChannelInput) { in.Headers = map[string]string{"X-Token": "a\r\nb"} },
		func(in *ChannelInput) { in.Headers = map[string]string{"Host": "example.com"} },
		func(in *ChannelInput) { in.URL = "file:///tmp/test" },
		func(in *ChannelInput) { in.Template = "{broken" },
	} {
		in := webhookInput("https://example.com/hook")
		mutate(&in)
		if err := validate(&in, credentials{URL: in.URL, Headers: in.Headers}); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid input accepted", in, err)
		}
	}
}

func TestWebhookTransportAndProviderResults(t *testing.T) {
	s, _, _ := fixture(t)
	var body []byte
	var mu sync.Mutex
	status, response := 200, `{"code":0}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != "PUT" || r.Header.Get("Authorization") != "Bearer test" || r.Header.Get("X-DST-Event-ID") != "event-1" {
			t.Error("incorrect request metadata")
		}
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	defer server.Close()
	in := webhookInput(server.URL)
	in.Headers, in.Method, in.ResponseMode = map[string]string{"Authorization": "Bearer test"}, "PUT", "feishu"
	channel, err := s.Save("", in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event := Event{ID: "event-1", Message: "引用\"换行\n"}
	if result := s.deliver(ctx, channel, event); result.ErrorCode != "" || result.HTTPStatus != 200 {
		t.Fatal(result)
	}
	var received Event
	var payload map[string]any
	mu.Lock()
	if err := json.Unmarshal(body, &payload); err != nil || payload["message"] != event.Message {
		t.Fatal("bad body", received, string(body))
	}
	mu.Unlock()
	for _, tc := range []struct {
		status     int
		body, code string
		retry      bool
	}{
		{200, `{"code":42,"message":"private upstream text"}`, "WEBHOOK_REJECTED", false},
		{200, `{"unexpected":0}`, "WEBHOOK_REJECTED", false},
		{200, `invalid`, "WEBHOOK_RESPONSE_INVALID", false},
		{429, ``, "WEBHOOK_HTTP_FAILED", true},
		{503, ``, "WEBHOOK_HTTP_FAILED", true},
		{403, ``, "WEBHOOK_HTTP_FAILED", false},
		{302, ``, "WEBHOOK_HTTP_FAILED", false},
	} {
		mu.Lock()
		status, response = tc.status, tc.body
		mu.Unlock()
		result := s.deliver(ctx, channel, event)
		if result.ErrorCode != tc.code || result.Retryable != tc.retry {
			t.Fatal(tc, result)
		}
	}
}

func TestWorkerPersistsAndDoesNotSendDisabledChannels(t *testing.T) {
	s, store, mail := fixture(t)
	channel, err := s.Save("", ChannelInput{Name: "Email", Type: "email", Enabled: true, Events: []string{"custom"}, Recipients: []string{"test@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.Send("标题", "正文")
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	// A new service reads the persisted outbox, rather than an in-memory queue.
	restarted, err := New(store, mail)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { restarted.Run(ctx); close(done) }()
	awaitStatus(t, restarted, "sent")
	cancel()
	<-done
	if mail.calls.Load() != 1 {
		t.Fatal("mail not delivered exactly once")
	}
	in := ChannelInput{Revision: channel.Revision, Name: channel.Name, Type: "email", Enabled: false, Events: channel.Events, Recipients: channel.Recipients}
	if _, err := restarted.Save(channel.ID, in); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(Event{ID: "disabled", Kind: "custom"}, []Channel{channel}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { restarted.Run(ctx); close(done) }()
	awaitStatus(t, restarted, "canceled")
	cancel()
	<-done
	if mail.calls.Load() != 1 {
		t.Fatal("disabled channel received notification")
	}
}

func awaitStatus(t *testing.T, s *Service, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		items, _, err := s.History(25, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.Status == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("delivery never reached", want)
}

func TestEventsFilterDedupeAndDoNotExposeTaskErrors(t *testing.T) {
	s, _, _ := fixture(t)
	in := webhookInput("https://example.com/hook")
	in.FailuresOnly = true
	if _, err := s.Save("", in); err != nil {
		t.Fatal(err)
	}
	job := jobs.Job{ID: "job-1", Kind: "room.start", Status: jobs.StatusSucceeded}
	s.OnJob(job)
	job.Status, job.Error = jobs.StatusFailed, &jobs.Error{Code: "START_FAILED", Message: "secret-password"}
	s.OnJob(job)
	s.OnJob(job)
	s.ObserveAgent("a", "Node A", "online")
	s.ObserveAgent("a", "Node A", "offline")
	s.ObserveAgent("a", "Node A", "offline")
	s.ObserveAgent("a", "Node A", "online")
	items, count, err := s.History(25, 0)
	if err != nil || count != 2 {
		t.Fatal("filter or dedupe failed", count, err)
	}
	for _, item := range items {
		if strings.Contains(item.Event.Message, "secret-password") {
			t.Fatal("task secret exposed")
		}
	}
	if _, err := s.Test(items[0].ChannelID); err != nil {
		t.Fatal("test must bypass failures filter", err)
	}
	if items, _ := s.Send("title", "body"); len(items) != 0 {
		t.Fatal("custom bypassed failure filter")
	}
}

func TestRetryBoundAndInterruptedSendRecovery(t *testing.T) {
	s, store, _ := fixture(t)
	channel, err := s.Save("", webhookInput("https://example.com/hook"))
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.Enqueue(Event{ID: "event-1", Kind: "custom"}, []Channel{channel})
	if err != nil {
		t.Fatal(err)
	}
	item := items[0]
	for attempt := 1; attempt <= 3; attempt++ {
		if began, err := store.Begin(item.ID); err != nil || !began {
			t.Fatal("begin", err)
		}
		item.Attempts = attempt
		if err := store.Finish(item, SendResult{ErrorCode: "WEBHOOK_HTTP_FAILED", HTTPStatus: 503, Retryable: true}, false); err != nil {
			t.Fatal(err)
		}
		history, _, _ := store.History(25, 0)
		want := "queued"
		if attempt == 3 {
			want = "failed"
		}
		if history[0].Status != want || history[0].Attempts != attempt {
			t.Fatal("retry bound", history[0])
		}
	}
	if err := store.Retry(item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin(item.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(); err != nil {
		t.Fatal(err)
	}
	history, _, _ := store.History(25, 0)
	if history[0].Status != "failed" || history[0].ErrorCode != "DELIVERY_INTERRUPTED" {
		t.Fatal(history)
	}
	if _, err := store.Next(); !errors.Is(err, ErrNotFound) {
		t.Fatal("uncertain delivery replayed automatically", err)
	}
}

func TestWebhookRejectsLinkLocalAtDialTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, address := range []string{"169.254.169.254:80", "0.0.0.0:80", "224.0.0.1:80", "[fe80::1]:80", "127.0.0.1:0"} {
		if conn, err := webhookDial(ctx, "tcp", address); !errors.Is(err, ErrInvalid) {
			if conn != nil {
				conn.Close()
			}
			t.Fatal("unsafe address accepted", address, err)
		}
	}
}

func TestWebhookTimeoutRetriesButShutdownCancellationDoesNot(t *testing.T) {
	s, _, _ := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	channel, err := s.Save("", webhookInput(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if result := s.deliver(ctx, channel, Event{ID: "timeout"}); result.ErrorCode != "WEBHOOK_CONNECTION_FAILED" || !result.Retryable {
		t.Fatal("timeout not retryable", result)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if result := s.deliver(ctx, channel, Event{ID: "shutdown"}); result.Retryable {
		t.Fatal("shutdown cancellation retried", result)
	}
}

func TestQueueAndHistoryStayBoundedIncludingManualRetry(t *testing.T) {
	_, store, _ := fixture(t)
	now := time.Now()
	if err := store.db.Transaction(func(tx *gorm.DB) error {
		for i := 0; i < 3005; i++ {
			status := "sent"
			if i < 1000 {
				status = "queued"
			}
			if i == 1000 {
				status = "failed"
			}
			row := deliveryRecord{ID: fmt.Sprint(i), ChannelID: "channel", EventData: `{}`, Status: status, CreatedAt: now.Add(time.Duration(i) * time.Millisecond), NextAttemptAt: now}
			if err := tx.Table(store.deliveriesTable).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Retry("1000"); !errors.Is(err, ErrLimit) {
		t.Fatal("retry bypassed queue bound", err)
	}
	if _, err := store.Enqueue(Event{ID: "event"}, []Channel{{ID: "channel"}}); !errors.Is(err, ErrLimit) {
		t.Fatal("enqueue bypassed queue bound", err)
	}
	if err := store.Prune(); err != nil {
		t.Fatal(err)
	}
	var completed, pending int
	store.db.Table(store.deliveriesTable).Where("status = ?", "queued").Count(&pending)
	store.db.Table(store.deliveriesTable).Where("status != ?", "queued").Count(&completed)
	if pending != 1000 || completed != 2000 {
		t.Fatal("prune removed pending or kept too much history", pending, completed)
	}
}

func TestReleaseAlertsSurviveHistoryCleanupAndFailuresOnly(t *testing.T) {
	s, store, _ := fixture(t)
	input := webhookInput("https://example.com/hook")
	input.FailuresOnly = true
	if _, err := s.Save("", input); err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "software:v1.3.4", Kind: "software.available", Version: "v1.3.4", Title: "New version", Message: "v1.3.4", Status: "available", Severity: "info"}
	s.Emit(event)
	_, count, err := s.History(25, 0)
	if err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := store.db.Table(store.deliveriesTable).Where("1 = 1").Delete(&deliveryRecord{}).Error; err != nil {
		t.Fatal(err)
	}
	recreated, err := New(store, &testMail{})
	if err != nil {
		t.Fatal(err)
	}
	recreated.Emit(event)
	_, count, err = recreated.History(25, 0)
	if err != nil || count != 0 {
		t.Fatal("repeated after history cleanup", count, err)
	}
	event.ID = "software:v1.3.5"
	event.Version = "v1.3.5"
	recreated.Emit(event)
	event.Kind = "agent.software.available"
	event.TargetID = "node-a"
	event.ID = "agent-software:node-a:v1.3.5"
	recreated.Emit(event)
	event.TargetID = "node-b"
	event.ID = "agent-software:node-b:v1.3.5"
	recreated.Emit(event)
	_, count, err = recreated.History(25, 0)
	if err != nil || count != 3 {
		t.Fatal("new releases/nodes suppressed", count, err)
	}
}

func TestBackupNotificationUsesSafePersistedArtifactAndReleaseStates(t *testing.T) {
	s, _, _ := fixture(t)
	in := webhookInput("https://example.com/hook")
	in.Events = []string{"backup.result"}
	if _, err := s.Save("", in); err != nil {
		t.Fatal(err)
	}
	s.OnJob(jobs.Job{ID: "backup-failed", Kind: "backup-set.create", Status: jobs.StatusFailed, Error: &jobs.Error{Code: "SAVE_IN_PROGRESS"}, Targets: []jobs.Target{{Error: &jobs.Error{Code: "SAVE_IN_PROGRESS", Message: "secret-password", Details: map[string]interface{}{"artifactState": "not_generated", "barrierReleaseState": "unconfirmed", "barrierAttempts": 3, "verifiedWorlds": 0, "totalWorlds": 2, "untrusted": "secret-password"}}}}})
	items, count, err := s.History(10, 0)
	if err != nil || count != 1 {
		t.Fatalf("items=%+v count=%d err=%v", items, count, err)
	}
	message := items[0].Event.Message
	for _, value := range []string{"not_generated", "unconfirmed", "3", "0/2"} {
		if !strings.Contains(message, value) {
			t.Fatalf("missing %q: %s", value, message)
		}
	}
	if strings.Contains(message, "secret-password") {
		t.Fatal("raw error/details leaked in notification")
	}
}
