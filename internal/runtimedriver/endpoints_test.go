package runtimedriver

import (
	"context"
	"errors"
	"testing"

	"dont/shared"
)

func TestEndpointRegistryResolvesLocalAndAgentTargets(t *testing.T) {
	local := &cpuLifecycleDriver{}
	agent := &cpuLifecycleDriver{}
	registry, err := NewEndpointRegistry(local, agent)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		targetID string
		want     Driver
	}{
		{targetID: LocalTargetID, want: local},
		{targetID: "agent:node-a", want: agent},
	}
	for _, test := range tests {
		got, resolveErr := registry.Resolve(test.targetID)
		if resolveErr != nil || got != test.want {
			t.Fatalf("Resolve(%q) = %#v, %v", test.targetID, got, resolveErr)
		}
	}
}

func TestEndpointRegistryExactRegistrationOverridesFamily(t *testing.T) {
	local := &cpuLifecycleDriver{}
	agent := &cpuLifecycleDriver{}
	override := &cpuLifecycleDriver{}
	registry, err := NewEndpointRegistry(local, agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("agent:node-a", override); err != nil {
		t.Fatal(err)
	}
	got, err := registry.Resolve("agent:node-a")
	if err != nil || got != override {
		t.Fatalf("exact endpoint = %#v, %v", got, err)
	}
	if fallback, err := registry.Resolve("agent:node-b"); err != nil || fallback != agent {
		t.Fatalf("family endpoint = %#v, %v", fallback, err)
	}
}

func TestEndpointRegistryResolvesLocalInstallationsIndependently(t *testing.T) {
	defaultDriver := &cpuLifecycleDriver{}
	secondaryDriver := &cpuLifecycleDriver{}
	registry, err := NewEndpointRegistry(defaultDriver, &cpuLifecycleDriver{})
	if err != nil {
		t.Fatal(err)
	}
	secondary, err := NewRuntimeEndpoint(secondaryDriver)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterInstallation(LocalTargetID, "secondary", secondary); err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.ResolveEndpoint(LocalTargetID, "secondary")
	if err != nil || resolved.Driver != secondaryDriver {
		t.Fatalf("secondary endpoint=%#v err=%v", resolved, err)
	}
	if _, err := registry.ResolveEndpoint(LocalTargetID, "missing"); !errors.Is(err, ErrUnsupportedRuntime) {
		t.Fatalf("missing installation error=%v", err)
	}
}

func TestRuntimeEndpointCombinesOptionalCapability(t *testing.T) {
	driver := &cpuLifecycleDriver{}
	endpoint, err := NewRuntimeEndpoint(driver)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.GameVersion = &fakeGameVersionDriver{}
	target := Target{CapabilitiesKnown: true, Capabilities: []Capability{CapabilityGameUpdate}}
	if !HasEndpointCapability(endpoint, target, CapabilityGameUpdate) {
		t.Fatal("combined endpoint did not expose game update")
	}
}

type fakeGameVersionDriver struct{}

func (*fakeGameVersionDriver) ObserveGameVersion(context.Context, Target) (shared.RuntimeGameVersionResult, error) {
	return shared.RuntimeGameVersionResult{}, nil
}

func (*fakeGameVersionDriver) UpdateGameVersion(context.Context, Target, Operation, string, bool) (shared.RuntimeGameVersionResult, error) {
	return shared.RuntimeGameVersionResult{}, nil
}

func TestEndpointRegistryRejectsUnknownTargetFamilies(t *testing.T) {
	registry, err := NewEndpointRegistry(&cpuLifecycleDriver{}, &cpuLifecycleDriver{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve("container:node-a"); !errors.Is(err, ErrUnsupportedRuntime) {
		t.Fatalf("error = %v", err)
	}
}

func TestHasTargetCapabilityUsesAdvertisedRuntimeSnapshot(t *testing.T) {
	driver := &cpuLifecycleDriver{cpuCap: true}
	target := Target{
		TargetID: "agent:legacy", CapabilitiesKnown: true,
		Capabilities: runtimeCapabilities([]string{"shard.control.v1", "runtime.logs.v1"}),
	}
	if !HasTargetCapability(driver, target, CapabilityLifecycle) {
		t.Fatalf("advertised target capabilities=%#v", target.Capabilities)
	}
	if HasTargetCapability(driver, target, CapabilityExclusiveCPU) {
		t.Fatal("missing target capability fell back to the driver's static capability set")
	}
	if HasTargetCapability(driver, target, CapabilityLogContinuation) {
		t.Fatal("target metadata overrode a capability missing from the driver")
	}
	target.CapabilitiesKnown = false
	if !HasTargetCapability(driver, target, CapabilityExclusiveCPU) {
		t.Fatal("metadata-free test target did not retain the driver fallback")
	}
}
