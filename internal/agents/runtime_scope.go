package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// RuntimeNodeScope identifies the last registered process and save location.
// It deliberately avoids Sync, inventory collection and heartbeat/metrics data:
// a normal read or connection error must not invalidate installation evidence.
func (s *Service) RuntimeNodeScope(targetID, installationID string) (string, error) {
	if !strings.HasPrefix(targetID, "agent:") {
		return "", ErrInvalidInput
	}
	agent, err := s.store.Agent(strings.TrimPrefix(targetID, "agent:"))
	if err != nil {
		return "", err
	}
	agent.InstallationRegistrySupported = runtimeInstallationRegistrySupported(agent.Details)
	agent.Installations = advertisedRuntimeInstallations(agent.Details)
	var driver, savePath string
	if agent.InstallationRegistrySupported && installationID != "" {
		for _, installation := range agent.Installations {
			if installation.ID == installationID {
				driver, savePath = installation.Driver, installation.SavePath
				break
			}
		}
		if savePath == "" {
			return "", ErrRuntimeInstallationNotRegistered
		}
	} else {
		config, err := s.runtimeConfigForAgent(agent)
		if err != nil {
			return "", err
		}
		if installationID != "" && config.InstallationID != installationID {
			return "", ErrRuntimeInstallationNotRegistered
		}
		installationID, savePath = config.InstallationID, config.SavePath
	}
	data, err := json.Marshal([]string{targetID, installationID, agent.Version,
		stringValue(agent.Details["runtime_instance_id"]), driver, savePath})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
