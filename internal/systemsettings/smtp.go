package systemsettings

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

func (s *Service) TestSMTP(ctx context.Context, input SMTPTestInput) (SMTPTestResult, error) {
	runtime, err := s.Runtime()
	if err != nil {
		return SMTPTestResult{}, err
	}
	server := strings.TrimSpace(input.Server)
	username := strings.TrimSpace(input.Username)
	password := input.Password
	if password == "" && server == runtime.SMTPServer && username == runtime.SMTPUsername {
		password = runtime.SMTPPassword
	}
	if server == "" || input.Port < 1 || input.Port > 65535 {
		return SMTPTestResult{}, ErrInvalidInput
	}
	if strings.ContainsAny(server+username, "\r\n\x00") {
		return SMTPTestResult{}, ErrInvalidInput
	}

	dialer := &net.Dialer{Timeout: 8 * time.Second}
	address := net.JoinHostPort(server, strconv.Itoa(input.Port))
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return SMTPTestResult{}, fmt.Errorf("connect SMTP server: %w", err)
	}
	defer connection.Close()

	result := SMTPTestResult{Server: address}
	var client *smtp.Client
	if input.Port == 465 {
		tlsConnection := tls.Client(connection, &tls.Config{ServerName: server, MinVersion: tls.VersionTLS12})
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			return SMTPTestResult{}, fmt.Errorf("start SMTP TLS: %w", err)
		}
		result.TLS = true
		client, err = smtp.NewClient(tlsConnection, server)
	} else {
		client, err = smtp.NewClient(connection, server)
	}
	if err != nil {
		return SMTPTestResult{}, fmt.Errorf("create SMTP client: %w", err)
	}
	defer client.Close()

	if !result.TLS {
		if supported, _ := client.Extension("STARTTLS"); supported {
			if err := client.StartTLS(&tls.Config{ServerName: server, MinVersion: tls.VersionTLS12}); err != nil {
				return SMTPTestResult{}, fmt.Errorf("upgrade SMTP TLS: %w", err)
			}
			result.TLS = true
		}
	}
	if username != "" {
		if password == "" {
			return SMTPTestResult{}, fmt.Errorf("SMTP password is required")
		}
		if !result.TLS && !isLoopbackServer(server) {
			return SMTPTestResult{}, fmt.Errorf("SMTP server does not support TLS; credentials were not sent")
		}
		if ok, _ := client.Extension("AUTH"); !ok {
			return SMTPTestResult{}, fmt.Errorf("SMTP server does not advertise authentication")
		}
		if err := client.Auth(smtp.PlainAuth("", username, password, server)); err != nil {
			return SMTPTestResult{}, fmt.Errorf("authenticate SMTP client: %w", err)
		}
		result.Authenticated = true
	}
	if err := client.Noop(); err != nil {
		return SMTPTestResult{}, fmt.Errorf("verify SMTP connection: %w", err)
	}
	_ = client.Quit()
	return result, nil
}

func isLoopbackServer(server string) bool {
	if strings.EqualFold(server, "localhost") {
		return true
	}
	ip := net.ParseIP(server)
	return ip != nil && ip.IsLoopback()
}
