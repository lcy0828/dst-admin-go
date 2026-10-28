package systemsettings

import (
	"context"
	"net/url"
	"strings"

	"dont/internal/mods"
)

// Return a readable endpoint without exposing embedded proxy credentials. An
// unchanged display value is never submitted as a replacement for the secret.
func proxyDisplayAddress(value string) string {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5h") || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	u.User = nil
	return u.String()
}

// WorkshopApplier prepares a local-only change. Commit cannot fail after Save.
type WorkshopApplier func(mods.NetworkConfig) (commit func(), discard func(), err error)

func (s *Service) SetWorkshopApplier(applier WorkshopApplier) { s.workshopApplier = applier }

func (s *Service) WorkshopConfig() (mods.NetworkConfig, error) {
	snapshot, err := s.repository.Snapshot()
	if err != nil {
		return mods.NetworkConfig{}, err
	}
	return s.workshopConfig(snapshot.Values).Normalize()
}

func (s *Service) workshopConfig(values map[string]string) mods.NetworkConfig {
	effective := make(map[string]string)
	for _, definition := range fieldDefinitions {
		effective[definition.ID] = s.effectiveValue(definition.ID, values[definition.ID])
	}
	return mods.NetworkConfigFromValues(effective)
}

// ResolveWorkshopTest supports testing unsaved settings without exposing secrets.
func (s *Service) ResolveWorkshopTest(input Input) (mods.NetworkConfig, error) {
	snapshot, err := s.repository.Snapshot()
	if err != nil {
		return mods.NetworkConfig{}, err
	}
	if input.Revision != snapshot.Revision {
		return mods.NetworkConfig{}, ErrConflict
	}
	for id := range input.Values {
		if !workshopField(id) {
			return mods.NetworkConfig{}, ErrInvalidInput
		}
	}
	for _, id := range input.ClearSecrets {
		if !workshopField(id) {
			return mods.NetworkConfig{}, ErrInvalidInput
		}
	}
	updates, err := s.normalizedUpdates(snapshot, input)
	if err != nil {
		return mods.NetworkConfig{}, err
	}
	for id, value := range updates {
		snapshot.Values[id] = value
	}
	config, err := s.workshopConfig(snapshot.Values).Normalize()
	if err != nil {
		return config, ErrInvalidInput
	}
	return config, nil
}

func workshopField(id string) bool {
	switch id {
	case "mod.steamAPIKey", "mod.steamAPIBase", "mod.steamCommunityBase", "mod.proxyMode", "mod.proxyURL":
		return true
	}
	return false
}

// ApplyWorkshop uses the same revision, lock, persistence and live apply path as
// the original system settings endpoint, without an extra confirmation dialog.
func (s *Service) ApplyWorkshop(ctx context.Context, input Input) (ApplyResult, error) {
	for id := range input.Values {
		if !workshopField(id) {
			return ApplyResult{}, ErrInvalidInput
		}
	}
	for _, id := range input.ClearSecrets {
		if !workshopField(id) {
			return ApplyResult{}, ErrInvalidInput
		}
	}
	input.Confirmation = ApplyConfirmation
	return s.ApplyContext(ctx, input)
}
