package routers

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type openAPIParameter struct {
	Ref string `yaml:"$ref"`
}

type openAPIOperation struct {
	Parameters []openAPIParameter `yaml:"parameters"`
}

type openAPIPath struct {
	Parameters []openAPIParameter `yaml:"parameters"`
	Get        *openAPIOperation  `yaml:"get"`
	Post       *openAPIOperation  `yaml:"post"`
	Put        *openAPIOperation  `yaml:"put"`
	Patch      *openAPIOperation  `yaml:"patch"`
	Delete     *openAPIOperation  `yaml:"delete"`
}

type openAPIDocument struct {
	Paths map[string]openAPIPath `yaml:"paths"`
}

var ginPathParameter = regexp.MustCompile(`:([^/]+)`)

func TestOpenAPIMatchesPublishedV2Routes(t *testing.T) {
	configureRouterTestEnvironment(t)
	router, err := InitRouter()
	if err != nil {
		t.Fatalf("initialize router: %v", err)
	}

	content, err := os.ReadFile("../docs/openapi-v2.yaml")
	if err != nil {
		t.Fatalf("read OpenAPI contract: %v", err)
	}
	var document openAPIDocument
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatalf("parse OpenAPI contract: %v", err)
	}

	expected := make(map[string]struct{})
	for path, item := range document.Paths {
		for method, operation := range item.operations() {
			if operation == nil {
				continue
			}
			expected[method+" "+path] = struct{}{}
			if isProtectedWrite(method, path) {
				parameters := append(append([]openAPIParameter(nil), item.Parameters...), operation.Parameters...)
				for _, required := range []string{
					"#/components/parameters/CSRFToken",
					"#/components/parameters/IdempotencyKey",
				} {
					if !hasParameter(parameters, required) {
						t.Errorf("%s %s does not declare %s", method, path, required)
					}
				}
			}
		}
	}

	actual := make(map[string]struct{})
	for _, route := range router.Routes() {
		if !strings.HasPrefix(route.Path, "/api/v2") {
			continue
		}
		path := strings.TrimPrefix(route.Path, "/api/v2")
		path = ginPathParameter.ReplaceAllString(path, `{$1}`)
		actual[route.Method+" "+path] = struct{}{}
	}

	if missing := setDifference(expected, actual); len(missing) > 0 {
		t.Errorf("OpenAPI operations missing from Gin routes:\n%s", strings.Join(missing, "\n"))
	}
	if undocumented := setDifference(actual, expected); len(undocumented) > 0 {
		t.Errorf("Gin routes missing from OpenAPI:\n%s", strings.Join(undocumented, "\n"))
	}
}

func (path openAPIPath) operations() map[string]*openAPIOperation {
	return map[string]*openAPIOperation{
		"GET": path.Get, "POST": path.Post, "PUT": path.Put, "PATCH": path.Patch, "DELETE": path.Delete,
	}
}

func isProtectedWrite(method, path string) bool {
	if method == "GET" || path == "/auth/login" || path == "/auth/setup" {
		return false
	}
	return true
}

func hasParameter(parameters []openAPIParameter, ref string) bool {
	for _, parameter := range parameters {
		if parameter.Ref == ref {
			return true
		}
	}
	return false
}

func setDifference(left, right map[string]struct{}) []string {
	items := make([]string, 0)
	for item := range left {
		if _, exists := right[item]; !exists {
			items = append(items, fmt.Sprintf("- %s", item))
		}
	}
	sort.Strings(items)
	return items
}
