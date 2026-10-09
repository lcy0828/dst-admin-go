package softwareupdate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type childProcess struct {
	command   *exec.Cmd
	done      chan error
	exited    bool
	terminate func() error
}

type supervisor struct {
	root, base, baseVersion, platform, healthURL string
	configPath                                   string
	args, environment                            []string
	signals                                      <-chan os.Signal
	bootTimeout, stableDuration                  time.Duration
	validate                                     func(context.Context, string, string, string) error
	committed                                    *installedRelease
	previous                                     *installedRelease
	kind                                         string
	agent                                        *agentLauncherIPC
}

// RunSupervisor adds no polling while idle. Only an explicit update signal
// starts a bounded restart/health check. Its stable PID keeps the container's
// world-shutdown trap from firing when just the management process is replaced.
func RunSupervisor(ctx context.Context, root, configPath, address, version string, args []string) error {
	base, err := os.Executable()
	if err != nil {
		return err
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	channel, stop := updateSignals()
	defer stop()
	s := &supervisor{root: root, base: base, baseVersion: version, platform: runtime.GOOS + "-" + runtime.GOARCH,
		configPath: configPath,
		args:       args, environment: os.Environ(), signals: channel, bootTimeout: 60 * time.Second, stableDuration: 10 * time.Second,
		healthURL: "http://" + net.JoinHostPort(host, port) + "/api/v2/auth/session", validate: validateExecutable}
	return s.run(ctx)
}

func (s *supervisor) candidate(release *installedRelease) (string, error) {
	if release == nil {
		return "", ErrInvalid
	}
	if release.ID == "" {
		if release.Version != s.baseVersion {
			return "", ErrInvalid
		}
		return s.base, nil
	}
	directory, err := releaseDirectory(s.root, release.ID)
	if err != nil {
		return "", err
	}
	if _, err := validateBundleFor(directory, release.Version, s.platform, s.kind); err != nil {
		return "", err
	}
	return filepath.Join(directory, binaryFor(s.kind, s.platform)), nil
}

func environmentWith(values []string, replacements map[string]string) []string {
	result := make([]string, 0, len(values)+len(replacements))
	for _, value := range values {
		key, _, _ := strings.Cut(value, "=")
		if _, replaced := replacements[key]; !replaced {
			result = append(result, value)
		}
	}
	for key, value := range replacements {
		result = append(result, key+"="+value)
	}
	return result
}

func (s *supervisor) start(release *installedRelease) (*childProcess, string, error) {
	binary, err := s.candidate(release)
	if err != nil {
		return nil, "", err
	}
	bootID := newID()
	path := os.Getenv("PATH")
	if release.ID != "" {
		path = filepath.Dir(binary) + string(os.PathListSeparator) + path
	}
	command := exec.Command(binary, s.args...)
	replacements := map[string]string{
		"DST_ADMIN_SUPERVISED": "1", "DST_ADMIN_UPDATE_PARENT_PID": strconv.Itoa(os.Getpid()),
		"DST_ADMIN_UPDATE_DIR": s.root, "DST_ADMIN_UPDATE_BASE_VERSION": s.baseVersion,
		"DST_ADMIN_BOOT_ID": bootID, "PATH": path,
	}
	if s.configPath != "" {
		replacements["DST_ADMIN_CONFIG"] = s.configPath
	}
	command.Env = environmentWith(s.environment, replacements)
	command.Stdout, command.Stderr, command.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := command.Start(); err != nil {
		return nil, "", err
	}
	child := &childProcess{command: command, done: make(chan error, 1)}
	if s.agent != nil {
		child.terminate = func() error { return s.agent.stop(bootID) }
	}
	go func() { child.done <- command.Wait() }()
	return child, bootID, nil
}

func (c *childProcess) stop() error {
	if c == nil || c.exited {
		return nil
	}
	if c.terminate == nil || c.terminate() != nil {
		_ = terminateProcess(c.command.Process)
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-c.done:
		c.exited = true
		return nil
	case <-timer.C:
		_ = c.command.Process.Kill()
		<-c.done
		c.exited = true
		log.Printf("[SoftwareUpdate] management process required forced termination")
		return nil
	}
}

func (s *supervisor) healthy(ctx context.Context, child *childProcess, bootID, version string) error {
	checkCtx, cancel := context.WithTimeout(ctx, s.bootTimeout)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrInvalid }}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var since time.Time
	for {
		select {
		case <-checkCtx.Done():
			return checkCtx.Err()
		case err := <-child.done:
			child.exited = true
			return fmt.Errorf("new management process exited: %v", err)
		case <-ticker.C:
			address := s.healthURL
			if s.agent != nil {
				address = s.agent.address(bootID)
			}
			if address == "" {
				continue
			}
			req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, address, nil)
			if err != nil {
				return err
			}
			if s.agent != nil {
				req.Header.Set("Authorization", "Bearer "+s.agent.token)
			}
			response, err := client.Do(req)
			ready := err == nil && response.StatusCode == http.StatusOK && response.Header.Get("X-DST-Admin-Boot-ID") == bootID && response.Header.Get("X-DST-Admin-Version") == version
			if response != nil {
				response.Body.Close()
			}
			if !ready {
				since = time.Time{}
				continue
			}
			if since.IsZero() {
				since = time.Now()
			}
			if time.Since(since) >= s.stableDuration {
				return nil
			}
		}
	}
}

func failState(root, phase string, err error) error {
	state, readErr := readState(root)
	if readErr != nil {
		return readErr
	}
	state.Pending = nil
	if state.Operation != nil {
		state.Operation.Phase = phase
		state.Operation.Error = err.Error()
		state.Operation.UpdatedAt = time.Now().UTC()
	}
	return writeState(root, state)
}

func (s *supervisor) apply(ctx context.Context, child *childProcess) (*childProcess, error) {
	state, err := readState(s.root)
	if err != nil {
		return s.recover(ctx, child, s.committed, err)
	}
	if state.Pending == nil {
		return child, nil
	}
	_ = os.Remove(filepath.Join(s.root, "ready.json"))
	target := *state.Pending
	binary, err := s.candidate(&target)
	if err == nil {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = s.validate(probeCtx, binary, target.Version, s.platform)
		cancel()
	}
	if err != nil {
		// The old process has already closed write admission. Restart it even
		// when preflight rejects the candidate so its HTTP barrier is released.
		return s.recover(ctx, child, state.Current, err)
	}
	if err := child.stop(); err != nil {
		return s.recover(ctx, child, state.Current, err)
	}
	trial, bootID, startErr := s.start(&target)
	if startErr == nil {
		startErr = s.healthy(ctx, trial, bootID, target.Version)
	}
	if ctx.Err() != nil {
		if trial != nil {
			_ = trial.stop()
		}
		return nil, ctx.Err()
	}
	if startErr != nil {
		return s.recover(ctx, trial, state.Current, startErr)
	}
	state.Previous, state.Current, state.Pending = state.Current, &target, nil
	if state.Operation != nil {
		state.Operation.Phase = "succeeded"
		state.Operation.Progress = 100
		state.Operation.Error = ""
		state.Operation.UpdatedAt = time.Now().UTC()
	}
	if err := writeState(s.root, state); err != nil {
		return s.recover(ctx, trial, s.committed, err)
	}
	s.committed = &target
	s.previous = state.Previous
	s.markReady(bootID)
	log.Printf("[SoftwareUpdate] management service updated to %s", target.Version)
	return trial, nil
}

func (s *supervisor) recover(ctx context.Context, child *childProcess, release *installedRelease, failure error) (*childProcess, error) {
	if child != nil {
		_ = child.stop()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	rollback, bootID, err := s.start(release)
	if err == nil {
		err = s.healthy(ctx, rollback, bootID, release.Version)
	}
	if err != nil {
		if rollback != nil {
			_ = rollback.stop()
		}
		return nil, errors.Join(failure, err)
	}
	// Rename may have succeeded even if the following directory fsync failed.
	// Restore both pointers explicitly to match the program actually running.
	state, stateErr := readState(s.root)
	if stateErr == nil {
		state.Current, state.Previous, state.Pending = release, s.previous, nil
		if state.Operation != nil {
			state.Operation.Phase = "rolled_back"
			state.Operation.Error = failure.Error()
			state.Operation.UpdatedAt = time.Now().UTC()
		}
		stateErr = writeState(s.root, state)
	}
	if stateErr != nil {
		// A disk error must not take down a successfully restored service.
		log.Printf("[SoftwareUpdate] cannot persist failed-update state: %v", stateErr)
	}
	s.committed = release
	s.markReady(bootID)
	log.Printf("[SoftwareUpdate] restored %s after update failed: %v", release.Version, failure)
	return rollback, nil
}

func (s *supervisor) markReady(bootID string) {
	if err := writeJSON(filepath.Join(s.root, "ready.json"), readyMarker{ParentPID: os.Getpid(), BootID: bootID}); err != nil {
		log.Printf("[SoftwareUpdate] launcher readiness could not be persisted: %v", err)
	}
}

func (s *supervisor) prune(state diskState) {
	keep := map[string]bool{}
	for _, release := range []*installedRelease{state.Current, state.Previous, state.Pending} {
		if release != nil {
			keep[release.ID] = true
		}
	}
	if state.Operation != nil && state.Operation.Phase == "prepared" {
		keep[state.Operation.ReleaseID] = true
	}
	entries, _ := os.ReadDir(filepath.Join(s.root, "releases"))
	for _, entry := range entries {
		if releaseIDPattern.MatchString(entry.Name()) && !keep[entry.Name()] {
			_ = os.RemoveAll(filepath.Join(s.root, "releases", entry.Name()))
		}
	}
	// Called only while the supervisor owns the store and no download runs.
	for _, directory := range []string{s.root, filepath.Join(s.root, "releases")} {
		entries, _ := os.ReadDir(directory)
		for _, entry := range entries {
			name := entry.Name()
			if directory == s.root && (strings.HasPrefix(name, ".download-") || strings.HasPrefix(name, ".state-")) || directory != s.root && strings.HasPrefix(name, ".stage-") {
				_ = os.RemoveAll(filepath.Join(directory, name))
			}
		}
	}
}

func (s *supervisor) run(ctx context.Context) error {
	if err := ensureRoot(s.root); err != nil {
		return err
	}
	unlock, err := lockSupervisor(s.root)
	if err != nil {
		return err
	}
	defer unlock()
	_ = os.Remove(filepath.Join(s.root, "ready.json"))
	defer os.Remove(filepath.Join(s.root, "ready.json"))
	state, err := readState(s.root)
	if err != nil {
		return err
	}
	if state.Pending != nil || state.Operation != nil && state.Operation.Busy() {
		if err := failState(s.root, "rolled_back", fmt.Errorf("previous update was interrupted; retaining the committed version")); err != nil {
			return err
		}
		state, err = readState(s.root)
		if err != nil {
			return err
		}
	}
	base := &installedRelease{Version: s.baseVersion}
	baseChanged := state.BaseVersion != s.baseVersion
	legacyRecovery := state.BaseVersion == "" && state.Operation != nil && state.Operation.Phase == "rolled_back"
	replaceBase := state.Current == nil || state.Current.ID == "" && state.Current.Version != s.baseVersion || baseChanged && !legacyRecovery && CompareVersions(s.baseVersion, state.Current.Version) > 0
	if replaceBase {
		state.Previous, state.Current = state.Current, base
	}
	if baseChanged || replaceBase {
		// Consume a new base only once. A failed base upgrade may restore an
		// older downloaded program; restarting the same image must retain it.
		state.BaseVersion = s.baseVersion
		if err := writeState(s.root, state); err != nil {
			return err
		}
	}
	s.prune(state)
	s.committed = state.Current
	s.previous = state.Previous
	child, bootID, err := s.start(state.Current)
	if err == nil && (state.Current.ID != "" || baseChanged && state.Previous != nil) {
		err = s.healthy(ctx, child, bootID, state.Current.Version)
	}
	if err != nil {
		if child != nil {
			_ = child.stop()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fallback := state.Previous
		if fallback == nil {
			fallback = base
		}
		child, bootID, startErr := s.start(fallback)
		if startErr == nil {
			startErr = s.healthy(ctx, child, bootID, fallback.Version)
		}
		if startErr != nil {
			if child != nil {
				_ = child.stop()
			}
			return errors.Join(err, startErr)
		}
		state.Current, state.Previous = fallback, nil
		s.committed = fallback
		s.previous = nil
		state.Pending = nil
		if state.Operation != nil {
			state.Operation.Phase = "rolled_back"
			state.Operation.Error = err.Error()
		}
		if err := writeState(s.root, state); err != nil {
			log.Printf("[SoftwareUpdate] restored service is running but its state could not be persisted: %v", err)
		}
		// Keep the restored process as the supervised child.
		s.markReady(bootID)
		return s.wait(ctx, child)
	}
	s.markReady(bootID)
	return s.wait(ctx, child)
}

func (s *supervisor) wait(ctx context.Context, child *childProcess) error {
	defer func() {
		if child != nil {
			_ = child.stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-child.done:
			child.exited = true
			return fmt.Errorf("management process exited: %v", err)
		case <-s.signals:
			var err error
			child, err = s.apply(ctx, child)
			if err != nil {
				return err
			}
		}
	}
}
