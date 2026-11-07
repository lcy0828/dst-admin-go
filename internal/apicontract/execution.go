package apicontract

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
)

// ExecutionPolicy is enforced by the API gateway. Unlike x-dst-tool hints,
// these fields are server-owned authorization and execution requirements.
type ExecutionPolicy struct {
	Profile      string `yaml:"profile" json:"profile"`
	Access       string `yaml:"access" json:"access"`
	Confirmation string `yaml:"confirmation" json:"confirmation"`
	Category     string `yaml:"category" json:"category,omitempty"`
	Executor     string `yaml:"executor" json:"executor,omitempty"`
}

func (p ExecutionPolicy) validate() error {
	if p.Profile != "" && !slices.Contains([]string{"game-read", "game-manage", "panel-manage"}, p.Profile) ||
		p.Access != "" && !slices.Contains([]string{"read", "write"}, p.Access) ||
		p.Confirmation != "" && !slices.Contains([]string{"none", "confirm", "destructive"}, p.Confirmation) ||
		p.Executor != "" && !slices.Contains([]string{"workbench", "script"}, p.Executor) {
		return fmt.Errorf("invalid executable access policy")
	}
	return nil
}
func (op *Operation) ExecutionPolicy() ExecutionPolicy {
	p := op.Authorization
	if p.Profile == "" {
		p.Profile = "panel-manage"
	}
	if p.Access == "" {
		p.Access = "read"
		if op.Mutation() {
			p.Access = "write"
		}
	}
	if p.Confirmation == "" {
		p.Confirmation = "none"
		if p.Access == "write" {
			p.Confirmation = "confirm"
		}
	}
	return p
}

// ExecutionRevision deliberately excludes unrelated operations and display text.
func (op *Operation) ExecutionRevision() string {
	schema, _ := op.AIInputSchema()
	var strip func(any) any
	strip = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			m := map[string]any{}
			for k, value := range x {
				if k != "description" && k != "title" && k != "examples" {
					m[k] = strip(value)
				}
			}
			return m
		case []any:
			out := make([]any, len(x))
			for i, value := range x {
				out[i] = strip(value)
			}
			return out
		default:
			return v
		}
	}
	b, _ := json.Marshal([]any{op.ID, op.Method, op.Path, op.Transport, op.ExecutionPolicy(), strip(schema)})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func (op *Operation) Scope() string {
	for _, key := range []string{"playerId", "worldId", "roomId", "installationId", "targetId", "nodeId", "jobId", "taskRef"} {
		for _, p := range op.Parameters {
			if p.Name == key {
				return key
			}
		}
	}
	return "global"
}
