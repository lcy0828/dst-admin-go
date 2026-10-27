// Package apidocs embeds the API contract shipped with this executable.
package apidocs

import _ "embed"

// OpenAPI is the same contract published in the repository.
//
//go:embed openapi-v2.yaml
var OpenAPI []byte

// AIToolSemantics supplies reviewed Chinese discovery text; API behavior and
// schemas remain defined by OpenAPI.
//
//go:embed ai-tool-semantics.json
var AIToolSemantics []byte
