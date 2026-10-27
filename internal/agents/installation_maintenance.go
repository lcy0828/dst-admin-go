package agents

import (
	"dont/internal/installationmaintenance"
	"strings"
)

// Resolve only this registered endpoint; ordinary lifecycle requests must not
// synchronize the global fleet catalog to check a maintenance lock.
func (s *Service) InstallationMaintenanceResource(targetID, installationID string) (installationmaintenance.Resource, error) {
	resource := installationmaintenance.Resource{TargetID: targetID, InstallationID: installationID}
	if targetID == "local" {
		s.localMu.RLock()
		config, ok := s.localInstallations[installationID]
		s.localMu.RUnlock()
		if !ok {
			return resource, ErrRuntimeInstallationNotRegistered
		}
		resource.Directory = config.ServerPath
		return resource, nil
	}
	if !strings.HasPrefix(targetID, "agent:") {
		return resource, ErrInvalidInput
	}
	agent, err := s.store.Agent(strings.TrimPrefix(targetID, "agent:"))
	if err != nil {
		return resource, err
	}
	canonicalRoot := ""
	if snapshot, readErr := s.store.InventoryForInstallation(agent.ID, installationID); readErr == nil {
		canonicalRoot = snapshot.Inventory.Installation.ServerRoot
	}
	for _, installation := range agent.Installations {
		if installation.ID == installationID {
			resource.Directory = installation.ServerPath
			if canonicalRoot != "" {
				resource.Directory = canonicalRoot
			}
			return resource, nil
		}
	}
	config, err := s.store.RuntimeConfig(agent.ID)
	if err != nil {
		return resource, err
	}
	if config.InstallationID != installationID {
		return resource, ErrRuntimeInstallationNotRegistered
	}
	resource.Directory = config.ServerPath
	if canonicalRoot != "" {
		resource.Directory = canonicalRoot
	}
	return resource, nil
}
