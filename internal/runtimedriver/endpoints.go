package runtimedriver

import (
	"strings"
	"sync"
)

const (
	LocalTargetID     = "local"
	AgentTargetPrefix = "agent:"
)

// EndpointRegistry resolves every Runtime target through one driver boundary.
// Exact registrations take precedence over target-family fallbacks.
type EndpointRegistry struct {
	mu       sync.RWMutex
	exact    map[endpointKey]RuntimeEndpoint
	families []endpointFamily
}

type endpointKey struct {
	targetID       string
	installationID string
}

type endpointFamily struct {
	prefix   string
	endpoint RuntimeEndpoint
}

// RuntimeEndpoint combines the base lifecycle driver with installation-level
// optional capabilities. Business services resolve this value once and do not
// need to branch on whether the transport is local or Agent-backed.
type RuntimeEndpoint struct {
	Driver        Driver
	Mods          ModDriver
	GameVersion   GameVersionDriver
	CPU           CPUDriver
	Configuration ConfigurationDriver
	ConfigReader  ConfigurationReader
	Secrets       ClusterTokenReader
	Maps          MapDriver
	ChatLogs      ChatLogDriver
	Barriers      SnapshotBarrierDriver
}

func (e RuntimeEndpoint) HasCapability(expected Capability) bool {
	if HasCapability(e.Driver, expected) {
		return true
	}
	switch expected {
	case CapabilityModPrepare, CapabilityModPublish:
		return e.Mods != nil
	case CapabilityGameUpdate:
		return e.GameVersion != nil
	case CapabilityExclusiveCPU:
		return e.CPU != nil
	case CapabilityConfigPublish:
		return e.Configuration != nil
	case CapabilityConfigRead:
		return e.ConfigReader != nil
	case CapabilityConfigSecrets:
		return e.Secrets != nil
	case CapabilityMapRender:
		return e.Maps != nil
	case CapabilityChatHistory:
		return e.ChatLogs != nil
	case CapabilitySnapshotBarrier:
		return e.Barriers != nil
	default:
		return false
	}
}

func HasEndpointCapability(endpoint RuntimeEndpoint, target Target, expected Capability) bool {
	if !endpoint.HasCapability(expected) {
		return false
	}
	if !target.CapabilitiesKnown {
		return true
	}
	for _, capability := range target.Capabilities {
		if capability == expected {
			return true
		}
	}
	return false
}

func NewRuntimeEndpoint(driver Driver) (RuntimeEndpoint, error) {
	if driver == nil {
		return RuntimeEndpoint{}, ErrInvalidTarget
	}
	endpoint := RuntimeEndpoint{Driver: driver}
	endpoint.Mods, _ = driver.(ModDriver)
	endpoint.GameVersion, _ = driver.(GameVersionDriver)
	endpoint.CPU, _ = driver.(CPUDriver)
	endpoint.Configuration, _ = driver.(ConfigurationDriver)
	endpoint.ConfigReader, _ = driver.(ConfigurationReader)
	endpoint.Secrets, _ = driver.(ClusterTokenReader)
	endpoint.Maps, _ = driver.(MapDriver)
	endpoint.ChatLogs, _ = driver.(ChatLogDriver)
	endpoint.Barriers, _ = driver.(SnapshotBarrierDriver)
	return endpoint, nil
}

func NewEndpointRegistry(local, agent Driver) (*EndpointRegistry, error) {
	localEndpoint, localErr := NewRuntimeEndpoint(local)
	agentEndpoint, agentErr := NewRuntimeEndpoint(agent)
	if localErr != nil || agentErr != nil {
		return nil, ErrInvalidTarget
	}
	return &EndpointRegistry{
		exact: map[endpointKey]RuntimeEndpoint{
			{targetID: LocalTargetID, installationID: "default"}: localEndpoint,
		},
		families: []endpointFamily{{prefix: AgentTargetPrefix, endpoint: agentEndpoint}},
	}, nil
}

func (r *EndpointRegistry) Register(targetID string, driver Driver) error {
	endpoint, err := NewRuntimeEndpoint(driver)
	if err != nil {
		return err
	}
	return r.RegisterInstallation(targetID, "default", endpoint)
}

func (r *EndpointRegistry) RegisterInstallation(targetID, installationID string, endpoint RuntimeEndpoint) error {
	targetID = strings.TrimSpace(targetID)
	installationID = strings.TrimSpace(installationID)
	if targetID == "" || installationID == "" || endpoint.Driver == nil {
		return ErrInvalidTarget
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exact[endpointKey{targetID: targetID, installationID: installationID}] = endpoint
	return nil
}

func (r *EndpointRegistry) Resolve(targetID string) (Driver, error) {
	endpoint, err := r.ResolveEndpoint(targetID, "default")
	return endpoint.Driver, err
}

func (r *EndpointRegistry) ResolveEndpoint(targetID, installationID string) (RuntimeEndpoint, error) {
	targetID = strings.TrimSpace(targetID)
	installationID = strings.TrimSpace(installationID)
	if targetID == "" {
		return RuntimeEndpoint{}, ErrInvalidTarget
	}
	if installationID == "" {
		installationID = "default"
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if endpoint, exists := r.exact[endpointKey{targetID: targetID, installationID: installationID}]; exists {
		return endpoint, nil
	}
	if targetID != LocalTargetID {
		if endpoint, exists := r.exact[endpointKey{targetID: targetID, installationID: "default"}]; exists {
			return endpoint, nil
		}
	}
	for _, family := range r.families {
		if strings.HasPrefix(targetID, family.prefix) && len(targetID) > len(family.prefix) {
			return family.endpoint, nil
		}
	}
	return RuntimeEndpoint{}, ErrUnsupportedRuntime
}
