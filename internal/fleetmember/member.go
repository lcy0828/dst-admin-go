package fleetmember

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dont/agent"
	"dont/shared"

	"github.com/go-ini/ini"
)

type Config struct {
	ControllerURL  string
	SecurityKey    string
	NodeID         string
	StatePath      string
	Runtime        agent.RuntimeInstallation
	ReportInterval time.Duration
}

type Member struct {
	agent    *agent.Agent
	stopOnce sync.Once
}

func New(config Config) (*Member, error) {
	statePath := filepath.Clean(strings.TrimSpace(config.StatePath))
	if !filepath.IsAbs(statePath) {
		return nil, errors.New("Fleet member state path must be absolute")
	}
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		return nil, fmt.Errorf("create Fleet member state directory: %w", err)
	}
	if err := os.Chmod(statePath, 0o700); err != nil {
		return nil, fmt.Errorf("restrict Fleet member state directory: %w", err)
	}
	identityPath := filepath.Join(statePath, "member.conf")
	if err := prepareIdentity(identityPath, config.ControllerURL, config.SecurityKey, config.NodeID); err != nil {
		return nil, err
	}
	if config.ReportInterval <= 0 {
		config.ReportInterval = 5 * time.Minute
	}
	client, err := agent.NewAgent(&agent.Config{
		ServerURL: config.ControllerURL, AgentID: config.NodeID, SecurityKey: config.SecurityKey,
		KeyFile: identityPath, OperationStateFile: filepath.Join(statePath, "runtime-state.json"),
		RuntimeInstallations: []agent.RuntimeInstallation{config.Runtime}, ReportInterval: config.ReportInterval,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize embedded Fleet member: %w", err)
	}
	return &Member{agent: client}, nil
}

func (m *Member) Start(ctx context.Context) error {
	if err := m.agent.Start(); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		m.Stop()
	}()
	return nil
}

func (m *Member) Stop() {
	if m == nil || m.agent == nil {
		return
	}
	m.stopOnce.Do(m.agent.Stop)
}

func (m *Member) Connected() bool {
	return m != nil && m.agent != nil && m.agent.Connected()
}

func prepareIdentity(path, controllerURL, securityKey, nodeID string) error {
	configuration := ini.Empty()
	if raw, err := os.ReadFile(path); err == nil {
		loaded, loadErr := ini.Load(raw)
		if loadErr != nil {
			return fmt.Errorf("parse Fleet member identity: %w", loadErr)
		}
		configuration = loaded
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read Fleet member identity: %w", err)
	}
	section := configuration.Section("agent")
	existingID := strings.TrimSpace(section.Key("AGENT_UUID").String())
	nodeID = strings.TrimSpace(nodeID)
	if existingID != "" && nodeID != "" && existingID != nodeID {
		return errors.New("configured Fleet node ID conflicts with the persisted node identity")
	}
	if existingID == "" && nodeID != "" {
		section.Key("AGENT_UUID").SetValue(nodeID)
	}
	section.Key("SERVER_URL").SetValue(strings.TrimSpace(controllerURL))
	section.Key("SECURITY_KEY").SetValue(strings.TrimSpace(securityKey))
	var encoded bytes.Buffer
	if _, err := configuration.WriteTo(&encoded); err != nil {
		return fmt.Errorf("encode Fleet member identity: %w", err)
	}
	if err := shared.WritePrivateFile(path, encoded.Bytes()); err != nil {
		return fmt.Errorf("write Fleet member identity: %w", err)
	}
	return nil
}
