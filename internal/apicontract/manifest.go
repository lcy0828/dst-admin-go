package apicontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type Descriptor struct {
	ID          string   `json:"operationId"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	ContentType string   `json:"contentType,omitempty"`
	Transport   string   `json:"transport"`
	Tags        []string `json:"tags"`
	Access      string   `json:"access"`
	Risk        string   `json:"risk"`
	Runtime     string   `json:"runtime"`
	Title       string   `json:"title"`
	Deprecated  bool     `json:"deprecated,omitempty"`
	ReplacedBy  string   `json:"replacedBy,omitempty"`
}

// Manifest is a small generated HTTP mapping for browser/SDK clients; schemas
// remain in OpenAPI. AI and MCP use the same operations without a copied map.
func Manifest() ([]byte, error) {
	c, err := Default()
	if err != nil {
		return nil, err
	}
	items := make([]Descriptor, 0, len(c.ordered))
	for _, op := range c.ordered {
		m := op.ToolMetadata()
		items = append(items, Descriptor{ID: op.ID, Method: op.Method, Path: op.Path, ContentType: op.ContentType(), Transport: op.Transport, Tags: m.Tags, Access: m.Access, Risk: m.Risk, Runtime: m.Runtime, Title: m.Title, Deprecated: op.Deprecated, ReplacedBy: op.ReplacedBy})
	}
	mapping, _ := json.Marshal(items)
	sum := sha256.Sum256(mapping)
	data, err := json.MarshalIndent(struct {
		Digest     string       `json:"operationsSHA256"`
		Operations []Descriptor `json:"operations"`
	}{hex.EncodeToString(sum[:]), items}, "", "  ")
	return append(data, '\n'), err
}
