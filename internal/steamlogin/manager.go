// Package steamlogin owns node-local SteamCMD credentials and bounded login
// sessions. Anonymous operation retains its existing command path.
package steamlogin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"dont/shared"
)

var ErrBusy = errors.New("SteamCMD 正在下载或验证登录，请等待任务完成")
var ErrInvalid = errors.New("Steam 登录参数无效")
var ErrCredentialsUnavailable = errors.New("Steam 登录凭据无法读取，请在该节点的 Steam 登录设置中重置后重新配置")
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
var guardPattern = regexp.MustCompile(`^[A-Za-z0-9]{5}$`)

type Manager struct {
	root     string
	mu       sync.Mutex
	storeMu  sync.Mutex
	profiles map[string]*profile
}
type Runner interface {
	Run(context.Context, string, []string, io.Writer) error
}
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, exe string, args []string, out io.Writer) error {
	c := exec.CommandContext(ctx, exe, args...)
	c.Stdout, c.Stderr = out, out
	return c.Run()
}

type wrappedRunner struct {
	manager *Manager
	base    Runner
}

func Wrap(manager *Manager, base Runner) Runner {
	if manager == nil {
		return base
	}
	return wrappedRunner{manager, base}
}
func (r wrappedRunner) Run(ctx context.Context, exe string, args []string, out io.Writer) error {
	return r.manager.run(ctx, exe, args, out, r.base)
}

type profile struct {
	mu          sync.Mutex
	id          string
	credentials credentials
	status      shared.SteamLoginStatus
	busy        bool
	uses        int
	session     *loginSession
	loadErr     error
}

func New(root string) *Manager { return &Manager{root: root, profiles: map[string]*profile{}} }

func (m *Manager) profile(executable string) (*profile, error) {
	if executable == "" {
		return nil, errors.New("运行节点未配置可用的 SteamCMD")
	}
	path := ResolveExecutable(executable)
	if path == "" {
		return nil, errors.New("运行节点未配置可用的 SteamCMD")
	}
	if real, e := filepath.EvalSymlinks(path); e == nil {
		path = real
	}
	sum := sha256.Sum256([]byte(path))
	id := hex.EncodeToString(sum[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.profiles[id]; p != nil {
		return p, nil
	}
	value, err := m.load(id)
	p := &profile{id: id, credentials: value, loadErr: err}
	p.resetStatus()
	m.profiles[id] = p
	return p, nil
}
func (p *profile) resetStatus() {
	if p.loadErr != nil {
		p.status = shared.SteamLoginStatus{Mode: "unavailable", State: "failed", RecoveryRequired: true, ErrorCode: "CREDENTIALS_UNAVAILABLE", UpdatedAt: time.Now().UTC()}
		return
	}
	state := "anonymous"
	if p.credentials.Mode == "account" {
		state = "configured"
	}
	p.status = shared.SteamLoginStatus{Mode: p.credentials.Mode, Username: p.credentials.Username, PasswordSet: p.credentials.Password != "", State: state, UpdatedAt: time.Now().UTC()}
}
func (m *Manager) Status(executable string) (shared.SteamLoginStatus, error) {
	p, err := m.profile(executable)
	if err != nil {
		return shared.SteamLoginStatus{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status, nil
}
func Validate(input shared.SteamLoginRequest) error {
	switch input.Action {
	case "status", "cancel", "reset":
		if input.Mode != "" || input.Username != "" || input.Password != "" || input.UseCachedCredentials || input.Code != "" {
			return ErrInvalid
		}
		if input.Action == "cancel" && input.SessionID == "" {
			return ErrInvalid
		}
		if input.Action != "cancel" && input.SessionID != "" {
			return ErrInvalid
		}
	case "login":
		if input.Mode != "" || input.Username != "" || input.Password != "" || input.UseCachedCredentials || input.SessionID != "" {
			return ErrInvalid
		}
		if input.Code != "" && !guardPattern.MatchString(input.Code) {
			return ErrInvalid
		}
	case "configure":
		if input.SessionID != "" || input.Code != "" {
			return ErrInvalid
		}
		if input.Mode == "anonymous" {
			if input.Username != "" || input.Password != "" || input.UseCachedCredentials {
				return ErrInvalid
			}
			return nil
		}
		if input.Mode != "account" || !usernamePattern.MatchString(input.Username) || strings.EqualFold(input.Username, "anonymous") || len(input.Password) > 256 || strings.ContainsAny(input.Password, "\x00\r\n") {
			return ErrInvalid
		}
		if input.UseCachedCredentials && input.Password != "" {
			return ErrInvalid
		}
	case "code":
		if !guardPattern.MatchString(input.Code) || input.SessionID == "" || input.Mode != "" || input.Username != "" || input.Password != "" || input.UseCachedCredentials {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if len(input.SessionID) > 128 {
		return ErrInvalid
	}
	return nil
}
func (m *Manager) Manage(executable string, input shared.SteamLoginRequest) (shared.SteamLoginStatus, error) {
	if err := Validate(input); err != nil {
		return shared.SteamLoginStatus{}, err
	}
	p, err := m.profile(executable)
	if err != nil {
		return shared.SteamLoginStatus{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loadErr != nil && input.Action != "status" && input.Action != "reset" {
		return p.status, ErrCredentialsUnavailable
	}
	switch input.Action {
	case "status":
	case "reset":
		if p.busy || p.uses > 0 {
			return p.status, ErrBusy
		}
		m.storeMu.Lock()
		err = m.reset(p.id)
		m.storeMu.Unlock()
		if err != nil {
			return p.status, err
		}
		p.credentials, p.loadErr = credentials{Mode: "anonymous"}, nil
		p.resetStatus()
	case "configure":
		if p.busy || p.uses > 0 {
			return p.status, ErrBusy
		}
		value := credentials{Mode: input.Mode}
		if input.Mode == "account" {
			value.Username = input.Username
			if !input.UseCachedCredentials {
				value.Password = input.Password
				if value.Password == "" && value.Username == p.credentials.Username {
					value.Password = p.credentials.Password
				}
				if value.Password == "" {
					return p.status, errors.New("请输入该 Steam 账号的密码")
				}
			}
		}
		m.storeMu.Lock()
		err = m.save(p.id, value)
		m.storeMu.Unlock()
		if err != nil {
			return p.status, err
		}
		p.credentials = value
		p.resetStatus()
	case "login":
		if p.busy || p.uses > 0 {
			return p.status, ErrBusy
		}
		if p.credentials.Mode != "account" {
			return p.status, ErrInvalid
		}
		if input.Code != "" && p.credentials.Password == "" {
			return p.status, errors.New("仅用户名模式无法提交一次性验证码，请先保存密码或直接在运行节点通过 steamcmd 完成登录")
		}
		p.busy = true
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		session, err := startSession(ctx, ResolveExecutable(executable), p.credentials, input.Code, io.Discard)
		if err != nil {
			cancel()
			p.busy = false
			return p.status, err
		}
		session.cancel = cancel
		p.session = session
		expires := time.Now().UTC().Add(4 * time.Minute)
		p.status.State, p.status.SessionID, p.status.ErrorCode = "connecting", session.id, ""
		p.status.ExpiresAt = &expires
		p.status.UpdatedAt = time.Now().UTC()
		go m.verifyLogin(ctx, p, session)
	case "code":
		if p.session == nil || p.status.SessionID != input.SessionID || (p.status.State != "guard_email" && p.status.State != "guard_mobile") {
			return p.status, ErrInvalid
		}
		if err = p.session.sendCode(input.Code); err != nil {
			return p.status, err
		}
		p.status.State = "connecting"
		p.status.UpdatedAt = time.Now().UTC()
	case "cancel":
		if p.session == nil || p.status.SessionID != input.SessionID {
			return p.status, ErrInvalid
		}
		p.session.cancel()
	}
	return p.status, nil
}
func (m *Manager) verifyLogin(ctx context.Context, p *profile, session *loginSession) {
	err := session.authenticate(ctx, func(state string) {
		p.mu.Lock()
		if p.session == session {
			p.status.State = state
			p.status.UpdatedAt = time.Now().UTC()
		}
		p.mu.Unlock()
	}, true)
	session.stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != session {
		return
	}
	p.session = nil
	p.busy = false
	p.status.ExpiresAt = nil
	p.status.SessionID = ""
	p.status.UpdatedAt = time.Now().UTC()
	p.status.State = "ready"
	p.status.ErrorCode = ""
	if err != nil {
		p.status.State = "failed"
		p.status.ErrorCode = errorCode(err)
	}
}

// Run never sends account credentials through argv. Login finishes before any
// download commands are sent, so a Guard prompt cannot consume a game command.
func (m *Manager) Run(ctx context.Context, executable string, args []string, output io.Writer) error {
	return m.runObserved(ctx, executable, args, output, ExecRunner{}, nil)
}
func (m *Manager) run(ctx context.Context, executable string, args []string, output io.Writer, base Runner) error {
	return m.runObserved(ctx, executable, args, output, base, nil)
}
func (m *Manager) RunObserved(ctx context.Context, executable string, args []string, out io.Writer, observe func(int) func()) error {
	return m.runObserved(ctx, executable, args, out, ExecRunner{}, observe)
}
func (m *Manager) runObserved(ctx context.Context, executable string, args []string, output io.Writer, base Runner, observe func(int) func()) error {
	p, err := m.profile(executable)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.loadErr != nil {
		p.mu.Unlock()
		return ErrCredentialsUnavailable
	}
	if p.busy {
		p.mu.Unlock()
		return ErrBusy
	}
	p.busy = true
	value := p.credentials
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.busy = false; p.mu.Unlock() }()
	executable = ResolveExecutable(executable)
	if value.Mode != "account" {
		return base.Run(ctx, executable, args, output)
	}
	commands, err := downloadCommands(args)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	preparation := []string{}
	remaining := []string{}
	for _, command := range commands {
		if strings.HasPrefix(command, "force_install_dir ") {
			preparation = append(preparation, command)
		} else {
			remaining = append(remaining, command)
		}
	}
	session, err := startSession(runCtx, executable, value, "", output, preparation...)
	if err != nil {
		return err
	}
	defer session.stop()
	if observe != nil {
		stop := observe(session.command.Process.Pid)
		if stop != nil {
			defer stop()
		}
	}
	loginCtx, loginCancel := context.WithTimeout(runCtx, 90*time.Second)
	err = session.authenticate(loginCtx, nil, false)
	loginCancel()
	p.mu.Lock()
	p.status.UpdatedAt = time.Now().UTC()
	p.status.State = "ready"
	p.status.ErrorCode = ""
	if err != nil {
		p.status.State = "failed"
		p.status.ErrorCode = errorCode(err)
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if err = session.send(strings.Join(remaining, "\n") + "\nquit\n"); err != nil {
		return err
	}
	select {
	case <-session.done:
		return session.result()
	case <-runCtx.Done():
		return runCtx.Err()
	}
}
func (m *Manager) Account(executable string) (bool, error) {
	s, e := m.Status(executable)
	if s.RecoveryRequired {
		return false, ErrCredentialsUnavailable
	}
	return s.Mode == "account", e
}

// Anonymous downloads may retain their existing concurrency, but credentials
// cannot change or a login begin until all active callers have finished.
func (m *Manager) AnonymousUsage(executable string) (bool, func(), error) {
	p, err := m.profile(executable)
	if err != nil {
		return false, nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loadErr != nil {
		return false, nil, ErrCredentialsUnavailable
	}
	if p.credentials.Mode == "account" {
		return true, func() {}, nil
	}
	p.uses++
	var once sync.Once
	return false, func() { once.Do(func() { p.mu.Lock(); p.uses--; p.mu.Unlock() }) }, nil
}
func (m *Manager) Close() {
	m.mu.Lock()
	profiles := make([]*profile, 0, len(m.profiles))
	for _, p := range m.profiles {
		profiles = append(profiles, p)
	}
	m.mu.Unlock()
	for _, p := range profiles {
		p.mu.Lock()
		if p.session != nil {
			p.session.cancel()
		}
		p.mu.Unlock()
	}
}
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.profiles {
		p.mu.Lock()
		busy := p.busy || p.uses > 0
		p.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}

func quote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}
func downloadCommands(args []string) ([]string, error) {
	commands := []string{}
	for i := 0; i < len(args); {
		if !strings.HasPrefix(args[i], "+") {
			return nil, ErrInvalid
		}
		name := strings.TrimPrefix(args[i], "+")
		i++
		values := []string{}
		for i < len(args) && !strings.HasPrefix(args[i], "+") {
			values = append(values, args[i])
			i++
		}
		if name == "login" || name == "quit" {
			continue
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\x00\r\n") {
				return nil, ErrInvalid
			}
		}
		if name != "force_install_dir" && name != "app_update" && name != "workshop_download_item" && name != "app_info_update" && name != "app_info_print" {
			return nil, ErrInvalid
		}
		line := name
		for _, value := range values {
			line += " " + quote(value)
		}
		commands = append(commands, line)
	}
	return commands, nil
}
