package steamlogin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/shared"
)

func TestKeywordCredentialsDoNotChangeLoginState(t *testing.T) {
	m, exe := fixture(t, "keywords")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, password := range []string{"authenticator", "OK"} {
		_, err := m.Manage(exe, shared.SteamLoginRequest{Action: "configure", Mode: "account", Username: "InvalidPassword", Password: password})
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err = m.Run(ctx, exe, []string{"+login", "anonymous", "+quit"}, &output); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), password) {
			t.Fatal("password leaked")
		}
	}
}

func TestZeroExitStorageFailureSurvivesSuccessfulBatchItem(t *testing.T) {
	m, exe := fixture(t, "disk")
	configure(t, m, exe)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output bytes.Buffer
	err := m.Run(ctx, exe, []string{"+login", "anonymous", "+workshop_download_item", "322330", "111", "+workshop_download_item", "322330", "222", "+quit"}, &output)
	if err == nil || !strings.Contains(err.Error(), "Disk write failure") || !strings.Contains(output.String(), "Success. Downloaded item 222") {
		t.Fatal("failed batch was reported as success", err, output.String())
	}
}

func TestStructuredOutputParsingAcrossChunks(t *testing.T) {
	for _, tc := range []struct{ name, output, want string }{
		{"email", "Please enter the Steam Guard code from the email sent to you:", "guard_email"},
		{"short email prompt", "Steam Guard code:", "guard_email"},
		{"short mobile prompt", "Two-factor code:", "guard_mobile"},
		{"mobile", "Please enter the current auth code from your Steam Guard Mobile Authenticator app:", "guard_mobile"},
		{"approval", "Please confirm the login in your Steam mobile app:", "guard_approval"},
		{"modern mobile approval", "Use the Steam Mobile App to confirm your sign in...", "guard_approval"},
		{"approval followed by ready", "Please confirm the login in your Steam mobile app:Waiting for user info...OK\n", "ready"},
		{"modern approval followed by ready", "Use the Steam Mobile App to confirm your sign in...Waiting for user info...OK\n", "ready"},
		{"modern approval followed by timeout", "Use the Steam Mobile App to confirm your sign in...ERROR! Failed to login: Timeout\n", "STEAM_NETWORK"},
		{"waiting confirmation followed by colon error", "Waiting for confirmation...ERROR: InvalidPassword\n", "INVALID_PASSWORD"},
		{"standalone repl failed line", "Logging in user 'user' to Steam Public...\nRetrying...\nFAILED (Account Disabled)\n", "LOGIN_FAILED"},
		{"waiting confirmation followed by ready", "Waiting for confirmation...Waiting for user info...OK\n", "ready"},
		{"password prompt without newline", "Password:", "PASSWORD_REQUIRED"},
		{"inline password prompt", "Logging in user 'user' to Steam Public...Password:", "PASSWORD_REQUIRED"},
		{"invalid guard code", "Logging in user 'user' to Steam Public...FAILED (Two-factor code mismatch)\n", "INVALID_GUARD_CODE"},
		{"rate limited", "Logging in user 'user' to Steam Public...FAILED (Rate Limit Exceeded)\n", "RATE_LIMITED"},
		{"invalid password", "ERROR! Failed to login: InvalidPassword\n", "INVALID_PASSWORD"},
		{"inline login failure", "Logging in user 'user' to Steam Public...FAILED (InvalidPassword)\n", "INVALID_PASSWORD"},
		{"subscription", "ERROR! Failed to install app (NoSubscription)\n", "ACCESS_DENIED"},
		{"network", "ERROR! Failed to login: NoConnection\n", "STEAM_NETWORK"},
		{"keyword username", "Logging in user 'InvalidPassword' to Steam Public...OK\nWaiting for user info...OK\n", "ready"},
		{"command echo", "Steam>login InvalidPassword authenticator\n", "connecting"},
		{"ordinary text", "authenticator InvalidPassword logged in ok\n", "connecting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &loginSession{state: "connecting", output: io.Discard}
			for _, b := range []byte(tc.output) {
				_, _ = s.Write([]byte{b})
			}
			if s.state != tc.want {
				t.Fatalf("got %s, want %s", s.state, tc.want)
			}
		})
	}
	var output bytes.Buffer
	s := &loginSession{state: "ready", output: &output, secrets: []string{"private-password"}}
	for _, chunk := range []string{"Update canc", "eled: private-", "password (Disk write failure)\n", strings.Repeat("progress ", 9000), "\nSuccess. Downloaded item 222\n"} {
		_, _ = s.Write([]byte(chunk))
	}
	if s.result() == nil || !strings.Contains(s.result().Error(), "Disk write failure") || strings.Contains(s.result().Error(), "private-") || strings.Contains(output.String(), "private-") {
		t.Fatal("failure lost or leaked secret", s.result())
	}
	for _, password := range []string{"OK", "Password:", "Use the Steam Mobile App to confirm your sign in...", "Please enter the Steam Guard code from the email sent to you:", "logged in ok", "ERROR! Failed to login: InvalidPassword"} {
		s := &loginSession{state: "connecting", output: io.Discard, secrets: []string{password}}
		for _, b := range []byte(password + "\n") {
			_, _ = s.Write([]byte{b})
		}
		if s.state != "connecting" || s.result() != nil {
			t.Fatal("bare credential echo changed protocol state", password, s.state)
		}
		_, _ = s.Write([]byte("Waiting for user info...OK\n"))
		if s.state != "ready" {
			t.Fatal("password hid actual readiness", password, s.state)
		}
	}
	s = &loginSession{state: "ready", output: io.Discard}
	_, _ = s.Write([]byte("Steam>Update canceled: Staging folder not writable (Disk write failure)\n"))
	if s.result() == nil || !strings.Contains(s.result().Error(), "Disk write failure") {
		t.Fatal("interactive prompt hid storage failure", s.result())
	}
}

func TestCorruptCredentialsRequireExplicitReset(t *testing.T) {
	for _, damage := range []string{"configuration", "missing key", "invalid key"} {
		t.Run(damage, func(t *testing.T) {
			m, exe := fixture(t, "")
			configure(t, m, exe)
			p, _ := m.profile(exe)
			switch damage {
			case "configuration":
				if err := os.WriteFile(filepath.Join(m.root, p.id+".json"), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing key":
				if err := os.Remove(filepath.Join(m.root, p.id+".key")); err != nil {
					t.Fatal(err)
				}
			case "invalid key":
				if err := os.WriteFile(filepath.Join(m.root, p.id+".key"), []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			reopened := New(m.root)
			t.Cleanup(reopened.Close)
			status, err := reopened.Status(exe)
			if err != nil || !status.RecoveryRequired || status.ErrorCode != "CREDENTIALS_UNAVAILABLE" || status.PasswordSet || status.Mode != "unavailable" {
				t.Fatal(status, err)
			}
			for _, input := range []shared.SteamLoginRequest{{Action: "configure", Mode: "anonymous"}, {Action: "configure", Mode: "account", Username: "user", Password: "password"}, {Action: "login"}} {
				if _, err = reopened.Manage(exe, input); !errors.Is(err, ErrCredentialsUnavailable) {
					t.Fatal("silently changed unreadable credentials", err)
				}
			}
			if err = reopened.Run(context.Background(), exe, []string{"+login", "anonymous", "+quit"}, io.Discard); !errors.Is(err, ErrCredentialsUnavailable) {
				t.Fatal("download fell back to anonymous", err)
			}
			if _, _, err = reopened.AnonymousUsage(exe); !errors.Is(err, ErrCredentialsUnavailable) {
				t.Fatal("mod download fell back", err)
			}
			if _, err = reopened.Account(exe); !errors.Is(err, ErrCredentialsUnavailable) {
				t.Fatal("version lookup fell back", err)
			}
			status, err = reopened.Manage(exe, shared.SteamLoginRequest{Action: "reset"})
			if err != nil || status.RecoveryRequired || status.Mode != "anonymous" {
				t.Fatal(status, err)
			}
			if err = reopened.Run(context.Background(), exe, []string{"+login", "anonymous", "+quit"}, io.Discard); err != nil {
				t.Fatal(err)
			}
			configure(t, reopened, exe)
			status, err = New(m.root).Status(exe)
			if err != nil || !status.PasswordSet || status.RecoveryRequired {
				t.Fatal("new credentials did not persist", status, err)
			}
		})
	}
}

func TestResetLegacyProfilePreservesOtherAccounts(t *testing.T) {
	m, exe := fixture(t, "")
	configure(t, m, exe)
	p, _ := m.profile(exe)
	// Convert the selected profile to the legacy shared-key format.
	if err := os.Rename(filepath.Join(m.root, p.id+".key"), filepath.Join(m.root, "key")); err != nil {
		t.Fatal(err)
	}
	otherID := "other-installation"
	// The other legacy profile shares the original key but has its own AAD.
	aead, err := m.cipher(otherID, false)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	plain := []byte(`{"mode":"account","username":"other_user","password":"other-password"}`)
	if err := os.WriteFile(filepath.Join(m.root, otherID+".json"), aead.Seal(nonce, nonce, plain, []byte(otherID)), 0600); err != nil {
		t.Fatal(err)
	}
	keyBefore, _ := os.ReadFile(filepath.Join(m.root, "key"))
	otherBefore, _ := os.ReadFile(filepath.Join(m.root, otherID+".json"))
	reopened := New(m.root)
	status, err := reopened.Status(exe)
	if err != nil || status.Username != "steam_user" {
		t.Fatal("legacy credentials incompatible", status, err)
	}
	// A broken shared key must not prevent recovery of just this profile.
	if err = os.WriteFile(filepath.Join(m.root, "key"), []byte("broken-legacy-key"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened = New(m.root)
	status, err = reopened.Status(exe)
	if err != nil || !status.RecoveryRequired {
		t.Fatal(status, err)
	}
	if _, err = reopened.Manage(exe, shared.SteamLoginRequest{Action: "reset"}); err != nil {
		t.Fatal(err)
	}
	configure(t, reopened, exe)
	keyAfter, _ := os.ReadFile(filepath.Join(m.root, "key"))
	if !bytes.Equal(keyAfter, []byte("broken-legacy-key")) {
		t.Fatal("reset changed global key")
	}
	otherAfter, _ := os.ReadFile(filepath.Join(m.root, otherID+".json"))
	if !bytes.Equal(otherBefore, otherAfter) {
		t.Fatal("other credentials modified")
	}
	// Restoring a shared key continues to recover any other legacy profiles.
	if err = os.WriteFile(filepath.Join(m.root, "key"), keyBefore, 0600); err != nil {
		t.Fatal(err)
	}
	if value, err := New(m.root).load(otherID); err != nil || value.Username != "other_user" {
		t.Fatal("other account lost", value, err)
	}
	if value, err := New(m.root).load(p.id); err != nil || value.Username != "steam_user" {
		t.Fatal("new independent profile incompatible", value, err)
	}
}

func TestResetRefusesActiveLoginAndDownload(t *testing.T) {
	m, exe := fixture(t, "hang")
	_, release, err := m.AnonymousUsage(exe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Manage(exe, shared.SteamLoginRequest{Action: "reset"}); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	release()
	configure(t, m, exe)
	if _, err = m.Manage(exe, shared.SteamLoginRequest{Action: "login"}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Manage(exe, shared.SteamLoginRequest{Action: "reset"}); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}
