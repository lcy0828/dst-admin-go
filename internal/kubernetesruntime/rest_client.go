package kubernetesruntime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRESTTimeout   = 15 * time.Second
	maximumTokenBytes    = 64 << 10
	maximumResponseBytes = 4 << 20
)

type RESTConfig struct {
	APIServer       string `json:"apiServer"`
	BearerTokenFile string `json:"bearerTokenFile"`
	CAFile          string `json:"caFile"`
	TimeoutSeconds  int    `json:"timeoutSeconds,omitempty"`
}

type RESTClient struct {
	providerID string
	namespace  string
	baseURL    *url.URL
	tokenFile  string
	client     *http.Client
	now        func() time.Time
}

func NewRESTClient(provider Provider, config RESTConfig) (*RESTClient, error) {
	if err := validateProvider(provider); err != nil {
		return nil, err
	}
	baseURL, err := url.Parse(strings.TrimSpace(config.APIServer))
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" || baseURL.User != nil ||
		(baseURL.Path != "" && baseURL.Path != "/") || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("kubernetes API server must be an origin-only https URL")
	}
	if !filepath.IsAbs(config.BearerTokenFile) || !filepath.IsAbs(config.CAFile) {
		return nil, errors.New("kubernetes token and CA files must use absolute paths")
	}
	caPEM, err := readBoundedFile(config.CAFile, maximumResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("read kubernetes CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("kubernetes CA file contains no certificates")
	}
	if _, err := readToken(config.BearerTokenFile); err != nil {
		return nil, err
	}
	timeout := defaultRESTTimeout
	if config.TimeoutSeconds != 0 {
		if config.TimeoutSeconds < 1 || config.TimeoutSeconds > 60 {
			return nil, errors.New("kubernetes REST timeout must be between 1 and 60 seconds")
		}
		timeout = time.Duration(config.TimeoutSeconds) * time.Second
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		Proxy:           nil,
	}
	return &RESTClient{
		providerID: provider.ID, namespace: provider.Namespace, baseURL: baseURL,
		tokenFile: config.BearerTokenFile, client: &http.Client{Transport: transport, Timeout: timeout}, now: time.Now,
	}, nil
}

func (c *RESTClient) Observe(ctx context.Context, ref ShardRef) (Observation, error) {
	if ref.ProviderID != c.providerID {
		return Observation{}, errors.New("kubernetes observation escaped provider scope")
	}
	name := workloadName(ref)
	observation := Observation{Ref: ref}
	var stateful rawStatefulSet
	exists, err := c.get(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(c.namespace)+"/statefulsets/"+url.PathEscape(name), nil, &stateful)
	if err != nil {
		return Observation{}, err
	}
	if exists {
		observation.StatefulSet = stateful.Metadata.resource()
	}

	selector := url.Values{"labelSelector": {scopeSelector(c.providerID, ref)}}
	var pods rawPodList
	if _, err := c.get(ctx, "/api/v1/namespaces/"+url.PathEscape(c.namespace)+"/pods", selector, &pods); err != nil {
		return Observation{}, err
	}
	if len(pods.Items) > 1 {
		return Observation{}, errors.New("multiple Pods claim the same managed Shard")
	}
	if len(pods.Items) == 1 {
		pod := pods.Items[0]
		observation.Pod = PodObservation{ResourceObservation: pod.Metadata.resource(), Phase: pod.Status.Phase, DeletionTimestamp: pod.Metadata.DeletionTimestamp}
	}

	var pvc rawPVC
	exists, err = c.get(ctx, "/api/v1/namespaces/"+url.PathEscape(c.namespace)+"/persistentvolumeclaims/"+url.PathEscape(name+"-data"), nil, &pvc)
	if err != nil {
		return Observation{}, err
	}
	if exists {
		modes := make([]AccessMode, 0, len(pvc.Spec.AccessModes))
		for _, mode := range pvc.Spec.AccessModes {
			modes = append(modes, AccessMode(mode))
		}
		observation.PVC = PVCObservation{
			ResourceObservation: pvc.Metadata.resource(), Phase: pvc.Status.Phase,
			StorageClassName: pvc.Spec.StorageClassName, AccessModes: modes,
			CapacityGiB: parseGiB(pvc.Status.Capacity["storage"]),
		}
	}

	if observation.Service, err = c.observeResource(ctx, "/api/v1/namespaces/"+url.PathEscape(c.namespace)+"/services/"+url.PathEscape(name)); err != nil {
		return Observation{}, err
	}
	if observation.PublishedService, err = c.observeResource(ctx, "/api/v1/namespaces/"+url.PathEscape(c.namespace)+"/services/"+url.PathEscape(name+"-public")); err != nil {
		return Observation{}, err
	}
	if observation.NetworkPolicy, err = c.observeResource(ctx, "/apis/networking.k8s.io/v1/namespaces/"+url.PathEscape(c.namespace)+"/networkpolicies/"+url.PathEscape(name)); err != nil {
		return Observation{}, err
	}

	observedAt := c.now().UTC()
	observation.ObservedAt = observedAt
	// Core Kubernetes APIs cannot prove physical-core/SMT allocation. A future
	// trusted node observation adapter must replace this explicit stale state.
	observation.CPU = CPUObservation{ObservedAt: observedAt, Stale: true}
	return observation, nil
}

func (c *RESTClient) Apply(context.Context, TypedMutation) (Observation, error) {
	return Observation{}, ErrMutationDisabled
}

func (c *RESTClient) observeResource(ctx context.Context, path string) (ResourceObservation, error) {
	var value struct {
		Metadata rawMetadata `json:"metadata"`
	}
	exists, err := c.get(ctx, path, nil, &value)
	if err != nil || !exists {
		return ResourceObservation{}, err
	}
	return value.Metadata.resource(), nil
}

func (c *RESTClient) get(ctx context.Context, path string, query url.Values, output interface{}) (bool, error) {
	endpoint := *c.baseURL
	endpoint.Path = path
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return false, err
	}
	token, err := readToken(c.tokenFile)
	if err != nil {
		return false, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.client.Do(request)
	if err != nil {
		return false, fmt.Errorf("kubernetes API request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return false, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return false, fmt.Errorf("kubernetes API returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err := decoder.Decode(output); err != nil {
		return false, fmt.Errorf("decode kubernetes API response: %w", err)
	}
	return true, nil
}

func readToken(path string) (string, error) {
	raw, err := readBoundedFile(path, maximumTokenBytes)
	if err != nil {
		return "", fmt.Errorf("read kubernetes bearer token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, "\x00\r\n \t") {
		return "", errors.New("kubernetes bearer token is empty or malformed")
	}
	return token, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("file exceeds the allowed size")
	}
	return raw, nil
}

func scopeSelector(providerID string, ref ShardRef) string {
	labels := expectedScopeLabels(providerID, ref.RoomID, ref.WorldID)
	keys := []string{LabelManagedBy, LabelComponent, LabelProviderID, LabelRoomID, LabelWorldID}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, ",")
}

func parseGiB(value string) int64 {
	value = strings.TrimSpace(value)
	for _, unit := range []struct {
		suffix  string
		divisor int64
	}{
		{"Ti", 1024}, {"Gi", 1}, {"Mi", 1024}, {"Ki", 1024 * 1024},
	} {
		if !strings.HasSuffix(value, unit.suffix) {
			continue
		}
		number, err := strconv.ParseInt(strings.TrimSuffix(value, unit.suffix), 10, 64)
		if err != nil || number < 0 {
			return 0
		}
		if unit.suffix == "Ti" {
			return number * unit.divisor
		}
		return number / unit.divisor
	}
	return 0
}

type rawMetadata struct {
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
}

func (value rawMetadata) resource() ResourceObservation {
	return ResourceObservation{
		Exists: true, UID: value.UID, ResourceVersion: value.ResourceVersion,
		Labels: cloneMap(value.Labels), Annotations: cloneMap(value.Annotations),
	}
}

type rawStatefulSet struct {
	Metadata rawMetadata `json:"metadata"`
}
type rawPodList struct {
	Items []rawPod `json:"items"`
}
type rawPod struct {
	Metadata rawMetadata `json:"metadata"`
	Status   struct {
		Phase string `json:"phase"`
	} `json:"status"`
}
type rawPVC struct {
	Metadata rawMetadata `json:"metadata"`
	Spec     struct {
		StorageClassName string   `json:"storageClassName"`
		AccessModes      []string `json:"accessModes"`
	} `json:"spec"`
	Status struct {
		Phase    string            `json:"phase"`
		Capacity map[string]string `json:"capacity"`
	} `json:"status"`
}
