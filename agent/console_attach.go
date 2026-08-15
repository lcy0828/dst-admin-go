package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"dont/internal/consoledispatch"
	"dont/internal/shards"
)

const (
	consoleAttachProtocolVersion = 1
	maximumAttachRequestBytes    = 8 * 1024
	defaultMaintenanceDuration   = 10 * time.Minute
	maximumMaintenanceDuration   = time.Hour
)

type consoleAttachControl interface {
	ConsoleAttach(string, string, bool) (shards.ConsoleAttachSpec, error)
	BeginConsoleMaintenance(context.Context, string, string, string) (shards.ConsoleAttachSpec, *consoledispatch.MaintenanceLease, error)
	EndConsoleMaintenance(string, string, *consoledispatch.MaintenanceLease) error
}

type consoleAttachRequest struct {
	ProtocolVersion int    `json:"protocolVersion"`
	InstallationID  string `json:"installationId"`
	Cluster         string `json:"cluster"`
	Shard           string `json:"shard"`
	Owner           string `json:"owner"`
	Writable        bool   `json:"writable"`
	LeaseSeconds    int    `json:"leaseSeconds"`
	Release         bool   `json:"release,omitempty"`
}

type consoleAttachResponse struct {
	ProtocolVersion int       `json:"protocolVersion"`
	Granted         bool      `json:"granted"`
	Error           string    `json:"error,omitempty"`
	Command         []string  `json:"command,omitempty"`
	InstanceID      string    `json:"instanceId,omitempty"`
	LeaseExpiresAt  time.Time `json:"leaseExpiresAt,omitempty"`
}

type ConsoleAttachOptions struct {
	SocketPath     string
	InstallationID string
	Cluster        string
	Shard          string
	Owner          string
	Writable       bool
	LeaseDuration  time.Duration
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
}

func ConsoleAttachSocketPath(stateFile string) (string, error) {
	stateFile = strings.TrimSpace(stateFile)
	if stateFile == "" {
		return "", errors.New("Agent state file is required")
	}
	absolute, err := filepath.Abs(stateFile)
	if err != nil {
		return "", err
	}
	directory := filepath.Dir(absolute)
	if runtime.GOOS == "darwin" {
		digest := sha256.Sum256([]byte(directory))
		directory = filepath.Join(os.TempDir(), fmt.Sprintf("dst-admin-agent-%d-%s", os.Getuid(), hex.EncodeToString(digest[:6])))
	}
	return filepath.Join(directory, "console-attach.sock"), nil
}

func (a *Agent) startConsoleAttachServer() error {
	if runtime.GOOS == "windows" || len(a.Config.RuntimeInstallations) == 0 {
		return nil
	}
	path, err := ConsoleAttachSocketPath(a.Config.OperationStateFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := removeStaleAttachSocket(path); err != nil {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return err
	}
	a.attachMutex.Lock()
	a.attachListener = listener
	a.attachMutex.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer os.Remove(path)
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				select {
				case <-a.stopChan:
					return
				default:
					continue
				}
			}
			a.attachMutex.Lock()
			if a.attachConnections == nil {
				a.attachConnections = make(map[net.Conn]struct{})
			}
			a.attachConnections[connection] = struct{}{}
			a.attachMutex.Unlock()
			a.wg.Add(1)
			go func() {
				defer a.wg.Done()
				defer func() {
					a.attachMutex.Lock()
					delete(a.attachConnections, connection)
					a.attachMutex.Unlock()
				}()
				a.handleConsoleAttach(connection)
			}()
		}
	}()
	return nil
}

func removeStaleAttachSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("console attach path already exists and is not a socket")
	}
	connection, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return errors.New("another Agent owns the console attach socket")
	}
	return os.Remove(path)
}

func (a *Agent) handleConsoleAttach(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	decoder := json.NewDecoder(io.LimitReader(connection, maximumAttachRequestBytes))
	decoder.DisallowUnknownFields()
	var request consoleAttachRequest
	if err := decoder.Decode(&request); err != nil {
		writeAttachResponse(connection, consoleAttachResponse{Error: "invalid attach request"})
		return
	}
	if err := validateAttachRequest(request); err != nil {
		writeAttachResponse(connection, consoleAttachResponse{Error: err.Error()})
		return
	}
	installation, exists := a.runtimeInstallation(request.InstallationID)
	if !exists || (installation.Driver != "native" && installation.Driver != "container") {
		writeAttachResponse(connection, consoleAttachResponse{Error: "attach is only available for a registered Runtime"})
		return
	}
	if err := validateShardOwnership(installation, request.Cluster, request.Shard); err != nil {
		writeAttachResponse(connection, consoleAttachResponse{Error: err.Error()})
		return
	}
	control, err := a.runtimeControl(installation)
	if err != nil {
		writeAttachResponse(connection, consoleAttachResponse{Error: err.Error()})
		return
	}
	attach, ok := control.(consoleAttachControl)
	if !ok {
		writeAttachResponse(connection, consoleAttachResponse{Error: "Runtime does not support local attach"})
		return
	}
	if !request.Writable {
		spec, attachErr := attach.ConsoleAttach(request.Cluster, request.Shard, true)
		if attachErr != nil {
			writeAttachResponse(connection, consoleAttachResponse{Error: attachErr.Error()})
			return
		}
		writeAttachResponse(connection, consoleAttachResponse{Granted: true, Command: spec.Command, InstanceID: spec.InstanceID})
		return
	}

	duration := time.Duration(request.LeaseSeconds) * time.Second
	acquireContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	spec, lease, err := attach.BeginConsoleMaintenance(acquireContext, request.Cluster, request.Shard, request.Owner)
	cancel()
	if err != nil {
		writeAttachResponse(connection, consoleAttachResponse{Error: err.Error()})
		return
	}
	expiresAt := time.Now().Add(duration).UTC()
	if err := writeAttachResponse(connection, consoleAttachResponse{Granted: true, Command: spec.Command, InstanceID: spec.InstanceID, LeaseExpiresAt: expiresAt}); err != nil {
		_ = attach.EndConsoleMaintenance(request.Cluster, request.Shard, lease)
		return
	}
	_ = connection.SetReadDeadline(expiresAt)
	var release consoleAttachRequest
	_ = json.NewDecoder(io.LimitReader(connection, maximumAttachRequestBytes)).Decode(&release)
	_ = attach.EndConsoleMaintenance(request.Cluster, request.Shard, lease)
}

func validateAttachRequest(request consoleAttachRequest) error {
	if request.ProtocolVersion != consoleAttachProtocolVersion || !runtimeInstallationID.MatchString(request.InstallationID) ||
		!shardResourceName.MatchString(request.Cluster) || !shardResourceName.MatchString(request.Shard) {
		return errors.New("invalid attach target")
	}
	if request.Writable {
		if strings.TrimSpace(request.Owner) == "" || len(request.Owner) > 128 || strings.ContainsAny(request.Owner, "\x00\r\n") ||
			request.LeaseSeconds < 60 || time.Duration(request.LeaseSeconds)*time.Second > maximumMaintenanceDuration {
			return errors.New("writable attach requires an owner and a lease between 1 minute and 1 hour")
		}
	}
	return nil
}

func writeAttachResponse(writer io.Writer, response consoleAttachResponse) error {
	response.ProtocolVersion = consoleAttachProtocolVersion
	return json.NewEncoder(writer).Encode(response)
}

func RunConsoleAttach(options ConsoleAttachOptions) error {
	if runtime.GOOS == "windows" {
		return errors.New("local console attach is unavailable on Windows")
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = defaultMaintenanceDuration
	}
	request := consoleAttachRequest{
		ProtocolVersion: consoleAttachProtocolVersion, InstallationID: options.InstallationID,
		Cluster: options.Cluster, Shard: options.Shard, Owner: options.Owner, Writable: options.Writable,
		LeaseSeconds: int(options.LeaseDuration / time.Second),
	}
	if err := validateAttachRequest(request); err != nil {
		return err
	}
	connection, err := net.DialTimeout("unix", options.SocketPath, 3*time.Second)
	if err != nil {
		return fmt.Errorf("connect to the local Agent attach socket: %w", err)
	}
	defer connection.Close()
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return err
	}
	var response consoleAttachResponse
	if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&response); err != nil {
		return err
	}
	if !response.Granted {
		return errors.New(response.Error)
	}
	if err := validateAttachCommand(response.Command); err != nil {
		return err
	}
	stdin, stdout, stderr := options.Stdin, options.Stdout, options.Stderr
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	ctx := context.Background()
	cancel := func() {}
	if options.Writable {
		ctx, cancel = context.WithTimeout(ctx, options.LeaseDuration)
	}
	defer cancel()
	command := exec.CommandContext(ctx, response.Command[0], response.Command[1:]...)
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	runErr := command.Run()
	if options.Writable {
		_ = json.NewEncoder(connection).Encode(consoleAttachRequest{ProtocolVersion: consoleAttachProtocolVersion, Release: true})
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("writable console maintenance lease expired")
	}
	return runErr
}

func validateAttachCommand(command []string) error {
	if len(command) < 4 || len(command) > 14 {
		return errors.New("Agent returned an invalid attach command")
	}
	for _, argument := range command {
		if argument == "" || strings.ContainsAny(argument, "\x00\r\n") {
			return errors.New("Agent returned an invalid attach command")
		}
	}
	if command[0] == "tmux" {
		return validateNativeAttachCommand(command)
	}
	if command[0] == "docker" || command[0] == "podman" {
		return validateContainerAttachCommand(command)
	}
	return errors.New("Agent returned an invalid attach command")
}

func validateNativeAttachCommand(command []string) error {
	index := 1
	if len(command) > index && command[index] == "-S" {
		if len(command) <= index+1 || !filepath.IsAbs(command[index+1]) {
			return errors.New("Agent returned an invalid native attach command")
		}
		index += 2
	}
	if len(command) <= index || command[index] != "attach-session" {
		return errors.New("Agent returned an invalid native attach command")
	}
	return validateAttachArguments(command[index+1:])
}

func validateContainerAttachCommand(command []string) error {
	if len(command) < 10 || command[1] != "exec" || command[2] != "-it" || !managedContainerID.MatchString(command[3]) || command[4] != "tmux" {
		return errors.New("Agent returned an invalid container attach command")
	}
	index := 5
	if len(command) <= index+1 || command[index] != "-S" || !filepath.IsAbs(command[index+1]) {
		return errors.New("Agent returned an invalid container attach command")
	}
	index += 2
	if len(command) <= index || command[index] != "attach-session" {
		return errors.New("Agent returned an invalid container attach command")
	}
	return validateAttachArguments(command[index+1:])
}

func validateAttachArguments(arguments []string) error {
	if len(arguments) == 2 && arguments[0] == "-t" && strings.HasPrefix(arguments[1], "=") && len(arguments[1]) > 1 {
		return nil
	}
	if len(arguments) == 3 && arguments[0] == "-r" && arguments[1] == "-t" && strings.HasPrefix(arguments[2], "=") && len(arguments[2]) > 1 {
		return nil
	}
	return errors.New("Agent returned an invalid attach command")
}
