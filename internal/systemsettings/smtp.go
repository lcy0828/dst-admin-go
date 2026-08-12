package systemsettings

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

func (s *Service) TestSMTP(ctx context.Context, input SMTPTestInput) (SMTPTestResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	runtime, err := s.Runtime()
	if err != nil {
		return SMTPTestResult{}, err
	}
	server := strings.TrimSpace(input.Server)
	username := strings.TrimSpace(input.Username)
	password := input.Password
	if input.UseStoredPassword && password == "" && server == runtime.SMTPServer && username == runtime.SMTPUsername {
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
	result := SMTPTestResult{Server: address, Stages: make([]SMTPTestStage, 0, 5)}
	addresses, err := net.DefaultResolver.LookupHost(ctx, server)
	if err != nil {
		return failedSMTPStage(result, "dns", err)
	}
	result.Stages = append(result.Stages, SMTPTestStage{Name: "dns", Status: "success", Detail: strings.Join(addresses, ", ")})

	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return failedSMTPStage(result, "connect", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return failedSMTPStage(result, "connect", err)
		}
	}
	result.Stages = append(result.Stages, SMTPTestStage{Name: "connect", Status: "success", Detail: address})

	var client *smtp.Client
	if input.Port == 465 {
		tlsConnection := tls.Client(connection, &tls.Config{ServerName: server, MinVersion: tls.VersionTLS12})
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			return failedSMTPStage(result, "tls", err)
		}
		result.TLS = true
		client, err = smtp.NewClient(tlsConnection, server)
	} else {
		client, err = smtp.NewClient(connection, server)
	}
	if err != nil {
		return failedSMTPStage(result, "connect", err)
	}
	defer client.Close()

	if !result.TLS {
		if supported, _ := client.Extension("STARTTLS"); supported {
			if err := client.StartTLS(&tls.Config{ServerName: server, MinVersion: tls.VersionTLS12}); err != nil {
				return failedSMTPStage(result, "tls", err)
			}
			result.TLS = true
		}
	}
	if result.TLS {
		result.Stages = append(result.Stages, SMTPTestStage{Name: "tls", Status: "success", Detail: "TLS 1.2 or newer"})
	} else {
		result.Stages = append(result.Stages, SMTPTestStage{Name: "tls", Status: "skipped", Detail: "server did not advertise STARTTLS"})
	}
	if username != "" {
		if password == "" {
			return failedSMTPStage(result, "auth", errors.New("SMTP password is required"))
		}
		if !result.TLS && !isLoopbackServer(server) {
			return failedSMTPStage(result, "auth", errors.New("SMTP server does not support TLS; credentials were not sent"))
		}
		if ok, _ := client.Extension("AUTH"); !ok {
			return failedSMTPStage(result, "auth", errors.New("SMTP server does not advertise authentication"))
		}
		if err := client.Auth(smtp.PlainAuth("", username, password, server)); err != nil {
			return failedSMTPStage(result, "auth", err)
		}
		result.Authenticated = true
		result.Stages = append(result.Stages, SMTPTestStage{Name: "auth", Status: "success", Detail: username})
	} else {
		result.Stages = append(result.Stages, SMTPTestStage{Name: "auth", Status: "skipped", Detail: "no username configured"})
	}
	if err := client.Noop(); err != nil {
		return failedSMTPStage(result, "verify", err)
	}
	result.Stages = append(result.Stages, SMTPTestStage{Name: "verify", Status: "success", Detail: "SMTP NOOP accepted"})
	_ = client.Quit()
	return result, nil
}

func failedSMTPStage(result SMTPTestResult, name string, err error) (SMTPTestResult, error) {
	result.Stages = append(result.Stages, SMTPTestStage{Name: name, Status: "failed", Detail: err.Error()})
	return result, fmt.Errorf("%s SMTP stage: %w", name, err)
}

func isLoopbackServer(server string) bool {
	if strings.EqualFold(server, "localhost") {
		return true
	}
	ip := net.ParseIP(server)
	return ip != nil && ip.IsLoopback()
}
