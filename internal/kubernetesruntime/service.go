package kubernetesruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

const (
	EnvironmentEnabled    = "DST_ADMIN_KUBERNETES_EXPERIMENTAL_ENABLED"
	EnvironmentConfigPath = "DST_ADMIN_KUBERNETES_PROVIDER_CONFIG"
	maximumConfigBytes    = 1 << 20
)

type ServiceStatus string

const (
	ServiceDisabled              ServiceStatus = "disabled"
	ServiceConfigurationRequired ServiceStatus = "configuration_required"
	ServiceAvailable             ServiceStatus = "available"
)

type Feature struct {
	ID        string `json:"id"`
	Available bool   `json:"available"`
	Code      string `json:"code,omitempty"`
}

type SafetyGate struct {
	ID        string `json:"id"`
	Satisfied bool   `json:"satisfied"`
}

type ProviderView struct {
	ID                        string           `json:"id"`
	Namespace                 string           `json:"namespace"`
	RuntimeImage              string           `json:"runtimeImage"`
	MaximumObservationSeconds int64            `json:"maximumObservationSeconds"`
	Capabilities              Capabilities     `json:"capabilities"`
	StorageProfiles           []StorageProfile `json:"storageProfiles"`
	ComputeProfiles           []ComputeProfile `json:"computeProfiles"`
}

type ServiceView struct {
	Release      ReleaseStatus `json:"release"`
	Enabled      bool          `json:"enabled"`
	Configured   bool          `json:"configured"`
	Status       ServiceStatus `json:"status"`
	ApplyAllowed bool          `json:"applyAllowed"`
	ErrorCode    string        `json:"errorCode,omitempty"`
	ErrorMessage string        `json:"errorMessage,omitempty"`
	Provider     *ProviderView `json:"provider,omitempty"`
	Features     []Feature     `json:"features"`
	SafetyGates  []SafetyGate  `json:"safetyGates"`
}

type FileConfig struct {
	Provider   Provider   `json:"provider"`
	Connection RESTConfig `json:"connection"`
}

type ProviderService struct {
	view   ServiceView
	driver *Driver
}

func DisabledService() *ProviderService {
	return &ProviderService{view: serviceView(false, nil, "", "")}
}

func NewService(provider Provider, client Client) (*ProviderService, error) {
	driver, err := NewDriver(provider, client)
	if err != nil {
		return nil, err
	}
	return &ProviderService{driver: driver, view: serviceView(true, driver, "", "")}, nil
}

// LoadExperimentalService keeps the rest of the control plane available when
// the optional Provider is disabled or misconfigured.
func LoadExperimentalService() *ProviderService {
	return loadExperimentalService(os.LookupEnv, os.Open)
}

func loadExperimentalService(lookup func(string) (string, bool), open func(string) (*os.File, error)) *ProviderService {
	rawEnabled, exists := lookup(EnvironmentEnabled)
	if !exists || strings.TrimSpace(rawEnabled) == "" {
		return DisabledService()
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(rawEnabled))
	if err != nil {
		return unavailableService(true, "KUBERNETES_FLAG_INVALID", EnvironmentEnabled+" must be true or false")
	}
	if !enabled {
		return DisabledService()
	}
	path, _ := lookup(EnvironmentConfigPath)
	path = strings.TrimSpace(path)
	if path == "" {
		return unavailableService(true, "KUBERNETES_CONFIG_REQUIRED", EnvironmentConfigPath+" is required when the experimental Provider is enabled")
	}
	file, err := open(path)
	if err != nil {
		return unavailableService(true, "KUBERNETES_CONFIG_UNREADABLE", err.Error())
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return unavailableService(true, "KUBERNETES_CONFIG_UNREADABLE", err.Error())
	}
	if info.Size() > maximumConfigBytes {
		return unavailableService(true, "KUBERNETES_CONFIG_TOO_LARGE", "Kubernetes Provider configuration exceeds 1 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximumConfigBytes+1))
	if err != nil {
		return unavailableService(true, "KUBERNETES_CONFIG_UNREADABLE", err.Error())
	}
	if len(raw) > maximumConfigBytes {
		return unavailableService(true, "KUBERNETES_CONFIG_TOO_LARGE", "Kubernetes Provider configuration exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config FileConfig
	if err := decoder.Decode(&config); err != nil {
		return unavailableService(true, "KUBERNETES_CONFIG_INVALID", err.Error())
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return unavailableService(true, "KUBERNETES_CONFIG_INVALID", err.Error())
	}
	client, err := NewRESTClient(config.Provider, config.Connection)
	if err != nil {
		return unavailableService(true, "KUBERNETES_CONNECTION_INVALID", err.Error())
	}
	service, err := NewService(config.Provider, client)
	if err != nil {
		return unavailableService(true, "KUBERNETES_PROVIDER_INVALID", err.Error())
	}
	return service
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("configuration contains multiple JSON values")
		}
		return err
	}
	return nil
}

func unavailableService(enabled bool, code, message string) *ProviderService {
	return &ProviderService{view: serviceView(enabled, nil, code, message)}
}

func serviceView(enabled bool, driver *Driver, code, message string) ServiceView {
	view := ServiceView{
		Release: ReleaseExperimental, Enabled: enabled, Configured: driver != nil,
		Status: ServiceDisabled, ApplyAllowed: false, ErrorCode: code, ErrorMessage: message,
	}
	if enabled {
		view.Status = ServiceConfigurationRequired
	}
	if driver != nil {
		provider := providerView(driver.Provider())
		view.Provider = &provider
		view.Status = ServiceAvailable
	}
	view.Features = featureStates(enabled, driver != nil)
	if driver != nil {
		view.SafetyGates = safetyGates(driver.Provider().Capabilities)
	} else {
		view.SafetyGates = safetyGates(Capabilities{})
	}
	return view
}

func providerView(provider Provider) ProviderView {
	storage := make([]StorageProfile, 0, len(provider.StorageProfiles))
	for _, profile := range provider.StorageProfiles {
		storage = append(storage, profile)
	}
	sort.Slice(storage, func(i, j int) bool { return storage[i].ID < storage[j].ID })
	compute := make([]ComputeProfile, 0, len(provider.ComputeProfiles))
	for _, profile := range provider.ComputeProfiles {
		profile.NodeSelector = cloneMap(profile.NodeSelector)
		compute = append(compute, profile)
	}
	sort.Slice(compute, func(i, j int) bool { return compute[i].ID < compute[j].ID })
	return ProviderView{
		ID: provider.ID, Namespace: provider.Namespace, RuntimeImage: provider.RuntimeImage,
		MaximumObservationSeconds: int64(provider.MaximumObservationAge.Seconds()),
		Capabilities:              provider.Capabilities, StorageProfiles: storage, ComputeProfiles: compute,
	}
}

func featureStates(enabled, configured bool) []Feature {
	available := enabled && configured
	code := "KUBERNETES_PROVIDER_UNAVAILABLE"
	if !enabled {
		code = "KUBERNETES_EXPERIMENT_DISABLED"
	}
	features := []Feature{
		{ID: "observe", Available: available},
		{ID: "preflight", Available: available},
		{ID: "typed_plan", Available: available},
		{ID: "apply", Available: false, Code: "KUBERNETES_MUTATION_API_DISABLED"},
		{ID: "lifecycle", Available: false, Code: "KUBERNETES_RUNTIME_UNVERIFIED"},
		{ID: "console", Available: false, Code: "KUBERNETES_CONSOLE_UNAVAILABLE"},
		{ID: "mods", Available: false, Code: "KUBERNETES_MODS_UNAVAILABLE"},
		{ID: "backup_restore", Available: false, Code: "KUBERNETES_BACKUP_UNAVAILABLE"},
	}
	if !available {
		for index := 0; index < 3; index++ {
			features[index].Code = code
		}
	}
	return features
}

func safetyGates(value Capabilities) []SafetyGate {
	return []SafetyGate{
		{ID: "lease_fencing_admission", Satisfied: value.LeaseFencingAdmission},
		{ID: "lease_aware_supervisor", Satisfied: value.LeaseAwareRuntimeSupervisor},
		{ID: "pod_uid_ownership", Satisfied: value.PodUIDOwnershipGate},
		{ID: "pvc_uid_ownership", Satisfied: value.PVCUIDOwnershipGate},
		{ID: "network_policy", Satisfied: value.NetworkPolicyEnforced},
		{ID: "secondary_master_dns", Satisfied: value.SecondaryMasterDNSVerified},
		{ID: "published_udp", Satisfied: value.PublishedUDPEndpointVerified},
		{ID: "exclusive_cpu", Satisfied: value.CPUManagerStatic && value.FullPhysicalCoreOnly && value.SMTTopologyKnown},
	}
}

func (s *ProviderService) Status() ServiceView {
	view := s.view
	if view.Provider != nil {
		provider := *view.Provider
		provider.Capabilities = view.Provider.Capabilities
		provider.StorageProfiles = append([]StorageProfile(nil), view.Provider.StorageProfiles...)
		provider.ComputeProfiles = append([]ComputeProfile(nil), view.Provider.ComputeProfiles...)
		for index := range provider.ComputeProfiles {
			provider.ComputeProfiles[index].NodeSelector = cloneMap(provider.ComputeProfiles[index].NodeSelector)
		}
		view.Provider = &provider
	}
	view.Features = append([]Feature(nil), view.Features...)
	view.SafetyGates = append([]SafetyGate(nil), view.SafetyGates...)
	return view
}

func (s *ProviderService) Observe(ctx context.Context, providerID, roomID, worldID string) (Observation, error) {
	if err := s.available(providerID); err != nil {
		return Observation{}, err
	}
	return s.driver.Observe(ctx, ShardRef{ProviderID: providerID, RoomID: roomID, WorldID: worldID})
}

func (s *ProviderService) Preview(ctx context.Context, providerID string, request Request) (Preview, error) {
	if err := s.available(providerID); err != nil {
		return Preview{}, err
	}
	if request.Ref.ProviderID != "" && request.Ref.ProviderID != providerID {
		return Preview{}, fmt.Errorf("%w: provider id does not match the route", ErrObservation)
	}
	request.Ref.ProviderID = providerID
	return s.driver.Preview(ctx, request)
}

func (s *ProviderService) available(providerID string) error {
	if !s.view.Enabled {
		return ErrDisabled
	}
	if s.driver == nil || s.view.Provider == nil {
		return ErrUnavailable
	}
	if strings.TrimSpace(providerID) != s.view.Provider.ID {
		return ErrProviderMissing
	}
	return nil
}
