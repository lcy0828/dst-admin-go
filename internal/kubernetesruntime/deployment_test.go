package kubernetesruntime

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestKubernetesRBACIsNamespaceScopedAndHasNoDangerousVerbs(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "kubernetes", "namespace-rbac.yaml")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	roleFound, bindingFound, runtimeAccountFound := false, false, false
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		switch document["kind"] {
		case "ClusterRole", "ClusterRoleBinding":
			t.Fatalf("cluster-scoped RBAC is forbidden: %#v", document)
		case "Role":
			roleFound = true
			metadata := document["metadata"].(map[string]any)
			if metadata["namespace"] != "dst-admin-runtime" {
				t.Fatalf("Role namespace = %v", metadata["namespace"])
			}
			allowedResources := map[string]bool{
				"statefulsets": true, "statefulsets/scale": true, "pods": true, "services": true,
				"persistentvolumeclaims": true, "networkpolicies": true, "events": true,
			}
			for _, rawRule := range document["rules"].([]any) {
				rule := rawRule.(map[string]any)
				for _, resource := range stringValues(rule["resources"]) {
					if !allowedResources[resource] {
						t.Fatalf("forbidden RBAC resource %q", resource)
					}
				}
				for _, verb := range stringValues(rule["verbs"]) {
					if verb == "delete" || verb == "deletecollection" || verb == "impersonate" || verb == "bind" || verb == "escalate" || verb == "*" {
						t.Fatalf("forbidden RBAC verb %q", verb)
					}
				}
				if containsString(stringValues(rule["resources"]), "pods") {
					for _, verb := range stringValues(rule["verbs"]) {
						if verb != "get" && verb != "list" && verb != "watch" {
							t.Fatalf("Pod mutation verb %q is forbidden", verb)
						}
					}
				}
			}
		case "RoleBinding":
			bindingFound = true
			roleRef := document["roleRef"].(map[string]any)
			if roleRef["kind"] != "Role" {
				t.Fatalf("RoleBinding escalates to %v", roleRef["kind"])
			}
		case "ServiceAccount":
			metadata := document["metadata"].(map[string]any)
			if metadata["name"] == "dst-admin-runtime" {
				runtimeAccountFound = true
				if document["automountServiceAccountToken"] != false {
					t.Fatalf("runtime ServiceAccount token automount is not disabled")
				}
			}
		}
	}
	if !roleFound || !bindingFound || !runtimeAccountFound {
		t.Fatalf("deployment boundary incomplete: role=%v binding=%v runtimeAccount=%v", roleFound, bindingFound, runtimeAccountFound)
	}
}

func TestKustomizationReferencesOnlyReviewedRBAC(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "kubernetes", "kustomization.yaml")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Resources []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Resources) != 1 || document.Resources[0] != "namespace-rbac.yaml" {
		t.Fatalf("unexpected Kubernetes deployment resources: %#v", document.Resources)
	}
}

func stringValues(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
