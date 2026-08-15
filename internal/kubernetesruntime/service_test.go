package kubernetesruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExperimentalServiceIsDisabledByDefault(t *testing.T) {
	service := DisabledService()
	view := service.Status()
	if view.Enabled || view.Configured || view.Status != ServiceDisabled || view.Release != ReleaseExperimental || view.ApplyAllowed {
		t.Fatalf("disabled service view = %#v", view)
	}
	if _, err := service.Observe(context.Background(), "provider", "room", "world"); err != ErrDisabled {
		t.Fatalf("disabled observe error = %v", err)
	}
	for _, feature := range view.Features {
		if feature.Available {
			t.Fatalf("disabled feature reported available: %#v", feature)
		}
	}
}

func TestExperimentalServiceExposesReadOnlyPreflightAndTypedPlan(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	client := &memoryClient{observation: testObservation(request, now)}
	service, err := NewService(provider, client)
	if err != nil {
		t.Fatal(err)
	}
	view := service.Status()
	if !view.Enabled || !view.Configured || view.Status != ServiceAvailable || view.Provider == nil || view.Provider.ID != provider.ID || view.ApplyAllowed {
		t.Fatalf("available service view = %#v", view)
	}
	if featureAvailable(view.Features, "apply") || !featureAvailable(view.Features, "observe") || !featureAvailable(view.Features, "typed_plan") {
		t.Fatalf("unexpected feature boundary: %#v", view.Features)
	}
	preview, err := service.Preview(context.Background(), provider.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Preflight.Ready || preview.Mutation == nil || preview.ApplyAllowed {
		t.Fatalf("read-only preview = %#v", preview)
	}
	if client.applied != nil {
		t.Fatal("preview unexpectedly applied a Kubernetes mutation")
	}
}

func TestExperimentalServiceReturnsBlockersWithoutInventingAPlan(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Action = ActionStart
	provider.Capabilities = Capabilities{}
	service, err := NewService(provider, &memoryClient{observation: testObservation(request, now)})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(context.Background(), provider.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Preflight.Ready || preview.Mutation != nil {
		t.Fatalf("blocked start produced a plan: %#v", preview)
	}
	want := map[string]bool{"LEASE_ADMISSION_UNAVAILABLE": false, "LEASE_SUPERVISOR_UNAVAILABLE": false, "NETWORK_POLICY_UNAVAILABLE": false}
	for _, issue := range preview.Preflight.Issues {
		if _, exists := want[issue.Code]; exists {
			want[issue.Code] = true
		}
	}
	for code, found := range want {
		if !found {
			t.Errorf("missing preflight blocker %s in %#v", code, preview.Preflight.Issues)
		}
	}
}

func TestExperimentalServiceEnvironmentConfigurationDegradesLocally(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
		status ServiceStatus
		code   string
	}{
		{name: "unset", values: map[string]string{}, status: ServiceDisabled},
		{name: "explicit false", values: map[string]string{EnvironmentEnabled: "false"}, status: ServiceDisabled},
		{name: "invalid flag", values: map[string]string{EnvironmentEnabled: "sometimes"}, status: ServiceConfigurationRequired, code: "KUBERNETES_FLAG_INVALID"},
		{name: "missing config", values: map[string]string{EnvironmentEnabled: "true"}, status: ServiceConfigurationRequired, code: "KUBERNETES_CONFIG_REQUIRED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) { value, ok := test.values[key]; return value, ok }
			service := loadExperimentalService(lookup, os.Open)
			view := service.Status()
			if view.Status != test.status || view.ErrorCode != test.code || view.Configured {
				t.Fatalf("environment view = %#v", view)
			}
		})
	}
}

func TestExperimentalServiceRejectsOversizedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	if err := os.WriteFile(path, []byte(`{}`+strings.Repeat(" ", maximumConfigBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{EnvironmentEnabled: "true", EnvironmentConfigPath: path}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	view := loadExperimentalService(lookup, os.Open).Status()
	if view.Status != ServiceConfigurationRequired || view.ErrorCode != "KUBERNETES_CONFIG_TOO_LARGE" || view.Configured {
		t.Fatalf("oversized configuration view = %#v", view)
	}
}

func featureAvailable(features []Feature, id string) bool {
	for _, feature := range features {
		if feature.ID == id {
			return feature.Available
		}
	}
	return false
}
