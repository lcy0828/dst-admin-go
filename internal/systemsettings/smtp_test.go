package systemsettings

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSMTPStagesWithoutAuthentication(t *testing.T) {
	server, port := startSMTPTestServer(t, smtpTestSuccess)
	service, err := NewService(NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := service.TestSMTP(ctx, SMTPTestInput{Server: server, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	if result.Server != net.JoinHostPort(server, fmt.Sprint(port)) || result.TLS || result.Authenticated {
		t.Fatalf("unexpected result: %#v", result)
	}
	want := []SMTPTestStage{
		{Name: "dns", Status: "success"},
		{Name: "connect", Status: "success"},
		{Name: "tls", Status: "skipped"},
		{Name: "auth", Status: "skipped"},
		{Name: "verify", Status: "success"},
	}
	assertSMTPStages(t, result.Stages, want)
}

func TestSMTPFailureReturnsCompletedStagesAndRequiresExplicitStoredPassword(t *testing.T) {
	server, port := startSMTPTestServer(t, smtpTestSuccess)
	service, err := NewService(NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := service.TestSMTP(ctx, SMTPTestInput{Server: server, Port: port, Username: "admin"})
	if err == nil || !strings.Contains(err.Error(), "SMTP password is required") {
		t.Fatalf("expected missing password error, got %v", err)
	}
	want := []SMTPTestStage{
		{Name: "dns", Status: "success"},
		{Name: "connect", Status: "success"},
		{Name: "tls", Status: "skipped"},
		{Name: "auth", Status: "failed"},
	}
	assertSMTPStages(t, result.Stages, want)
	for _, stage := range result.Stages {
		if strings.Contains(stage.Detail, "test-steam-api-key") {
			t.Fatal("unrelated configured secret leaked into SMTP diagnostics")
		}
	}
}

func TestSMTPTimeoutIsBoundedAndReportsConnectStage(t *testing.T) {
	server, port := startSMTPTestServer(t, smtpTestStall)
	service, err := NewService(NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := service.TestSMTP(ctx, SMTPTestInput{Server: server, Port: port})
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("expected bounded timeout, duration=%s err=%v", time.Since(started), err)
	}
	assertSMTPStages(t, result.Stages, []SMTPTestStage{
		{Name: "dns", Status: "success"},
		{Name: "connect", Status: "success"},
		{Name: "connect", Status: "failed"},
	})
}

func TestSMTPAuthenticationRejectionReportsAuthStage(t *testing.T) {
	_, port := startSMTPTestServer(t, smtpTestRejectAuth)
	service, err := NewService(NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := service.TestSMTP(ctx, SMTPTestInput{
		Server: "localhost", Port: port, Username: "admin", Password: "secret-password",
	})
	if err == nil || !strings.Contains(err.Error(), "535") {
		t.Fatalf("expected authentication rejection, got %v", err)
	}
	assertSMTPStages(t, result.Stages, []SMTPTestStage{
		{Name: "dns", Status: "success"},
		{Name: "connect", Status: "success"},
		{Name: "tls", Status: "skipped"},
		{Name: "auth", Status: "failed"},
	})
	for _, stage := range result.Stages {
		if strings.Contains(stage.Detail, "secret-password") {
			t.Fatal("SMTP password leaked into authentication diagnostics")
		}
	}
}

type smtpTestMode int

const (
	smtpTestSuccess smtpTestMode = iota
	smtpTestStall
	smtpTestRejectAuth
)

func startSMTPTestServer(t *testing.T, mode smtpTestMode) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	address := listener.Addr().(*net.TCPAddr)
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		if mode == smtpTestStall {
			<-time.After(time.Second)
			return
		}
		_, _ = fmt.Fprint(connection, "220 localhost ESMTP ready\r\n")
		scanner := bufio.NewScanner(connection)
		for scanner.Scan() {
			command := strings.ToUpper(scanner.Text())
			switch {
			case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
				if mode == smtpTestRejectAuth {
					_, _ = fmt.Fprint(connection, "250-localhost\r\n250 AUTH PLAIN\r\n")
				} else {
					_, _ = fmt.Fprint(connection, "250 localhost\r\n")
				}
			case strings.HasPrefix(command, "AUTH"):
				_, _ = fmt.Fprint(connection, "535 5.7.8 Authentication credentials invalid\r\n")
			case strings.HasPrefix(command, "NOOP"):
				_, _ = fmt.Fprint(connection, "250 OK\r\n")
			case strings.HasPrefix(command, "QUIT"):
				_, _ = fmt.Fprint(connection, "221 Bye\r\n")
				return
			default:
				_, _ = fmt.Fprint(connection, "250 OK\r\n")
			}
		}
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})
	return address.IP.String(), address.Port
}

func assertSMTPStages(t *testing.T, got, want []SMTPTestStage) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("stages=%#v want=%#v", got, want)
	}
	for index := range want {
		if got[index].Name != want[index].Name || got[index].Status != want[index].Status {
			t.Fatalf("stage %d=%#v want=%#v", index, got[index], want[index])
		}
	}
}
