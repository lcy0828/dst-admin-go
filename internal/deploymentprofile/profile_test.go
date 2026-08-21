package deploymentprofile

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

func profileTestKey() string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'f'}, 32))
}

func TestResolveDeploymentRoles(t *testing.T) {
	statePath := t.TempDir()
	tests := []struct {
		name   string
		values Values
		role   Role
	}{
		{name: "legacy controller worker", values: Values{}, role: RoleControllerWorker},
		{name: "standalone", values: Values{ControllerEnabled: "false"}, role: RoleStandalone},
		{name: "controller only", values: Values{Packaging: "control_plane", LocalExecutorEnabled: "false", ControllerEnabled: "true"}, role: RoleControllerOnly},
		{name: "managed worker", values: Values{
			ControllerEnabled: "false", MemberEnabled: "true", ControllerURL: "wss://dst.example.com",
			MemberKey: profileTestKey(), StatePath: statePath,
		}, role: RoleManagedWorker},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile, err := Resolve(test.values)
			if err != nil {
				t.Fatal(err)
			}
			if profile.Role != test.role {
				t.Fatalf("role = %q, want %q", profile.Role, test.role)
			}
			if profile.MemberEnabled && profile.ControllerURL != "wss://dst.example.com/agent" {
				t.Fatalf("controller URL = %q", profile.ControllerURL)
			}
		})
	}
}

func TestResolveRejectsUnsafeRoleCombinations(t *testing.T) {
	tests := []Values{
		{LocalExecutorEnabled: "false", ControllerEnabled: "false"},
		{ControllerEnabled: "true", MemberEnabled: "true"},
		{Packaging: "control_plane", LocalExecutorEnabled: "true", ControllerEnabled: "true"},
		{Packaging: "control_plane", LocalExecutorEnabled: "false", ControllerEnabled: "false"},
		{ControllerEnabled: "false", MemberEnabled: "true", ControllerURL: "https://dst.example.com/agent", MemberKey: profileTestKey(), StatePath: t.TempDir()},
		{ControllerEnabled: "false", MemberEnabled: "true", ControllerURL: "wss://dst.example.com/agent?key=secret", MemberKey: profileTestKey(), StatePath: t.TempDir()},
	}
	for _, values := range tests {
		_, err := Resolve(values)
		var validation *ValidationError
		if !errors.As(err, &validation) || validation.Field == "" {
			t.Fatalf("error = %v, want field validation", err)
		}
	}
}
