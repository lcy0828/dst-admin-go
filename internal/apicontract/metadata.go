package apicontract

import (
	"fmt"
	"slices"
	"strings"
)

// ToolPolicy contains operation-specific semantics in the OpenAPI contract.
// Hints describe behavior; they never grant authority or add approval steps.
type ToolPolicy struct {
	Tags        []string `yaml:"tags"`
	ReadOnly    *bool    `yaml:"readOnly"`
	Risk        string   `yaml:"risk"`
	Runtime     string   `yaml:"runtime"`
	Idempotent  *bool    `yaml:"idempotent"`
	Destructive *bool    `yaml:"destructive"`
	OpenWorld   *bool    `yaml:"openWorld"`
}
type ToolMetadata struct {
	Title       string   `json:"title"`
	Tags        []string `json:"tags"`
	Access      string   `json:"access"`
	Risk        string   `json:"risk"`
	Runtime     string   `json:"runtime"`
	Idempotent  bool     `json:"idempotent"`
	Destructive bool     `json:"destructive"`
	OpenWorld   bool     `json:"openWorld"`
}

var domainTags = map[string][]string{
	"AI": {"ai", "system"}, "Agents": {"agent"}, "Auth": {"access"}, "Automation": {"schedule"},
	"Backups": {"backup"}, "Configuration": {"configuration"}, "Console": {"console"}, "Containers": {"container"},
	"Entity Catalog": {"entity"}, "Game": {"game"}, "GameNotifications": {"notification"}, "Jobs": {"job"},
	"Kubernetes": {"runtime", "kubernetes"}, "Logs": {"log"}, "Maps": {"map"}, "Mods": {"mod"}, "Players": {"player"},
	"Rooms": {"room"}, "Runtime": {"runtime"}, "Runtime Audit": {"runtime", "log"}, "Runtime Targets": {"runtime", "agent"},
	"Save Imports": {"save"}, "System": {"system"}, "Topology": {"topology"}, "WorldState": {"world"},
}
var resourceTags = map[string]string{
	"rooms": "room", "worlds": "world", "players": "player", "mods": "mod", "mod-update": "mod", "workshop": "mod",
	"backups": "backup", "backup-sets": "backup", "backup-policy": "backup", "backup-schedules": "backup", "backup-operations": "backup",
	"maps": "map", "logs": "log", "structured-logs": "log", "chat-logs": "log", "log-rules": "log",
	"jobs": "job", "automation": "schedule", "access": "access", "cluster-token": "access",
	"commands": "console", "raw-commands": "console", "command-runs": "console",
	"notifications": "notification", "game-notification-policy": "notification", "software": "software",
	"game-installations": "game", "luajit": "game", "entity-catalog": "entity", "configuration": "configuration", "settings": "configuration",
}

func (op *Operation) ToolMetadata() ToolMetadata {
	readOnly := !op.Mutation()
	if op.Policy.ReadOnly != nil {
		readOnly = *op.Policy.ReadOnly
	}
	m := ToolMetadata{Title: op.Summary, Tags: []string{}, Access: "read", Risk: "low", Runtime: "any", Idempotent: readOnly, Destructive: !readOnly}
	if !readOnly {
		m.Access = "write"
		m.Risk = "normal"
	}
	for _, tag := range op.Tags {
		m.Tags = append(m.Tags, domainTags[tag]...)
	}
	for _, part := range strings.Split(op.Path, "/") {
		if tag := resourceTags[part]; tag != "" {
			m.Tags = append(m.Tags, tag)
		}
	}
	m.Tags = append(m.Tags, op.Policy.Tags...)
	slices.Sort(m.Tags)
	m.Tags = slices.Compact(m.Tags)
	if op.Policy.Risk != "" {
		m.Risk = op.Policy.Risk
	}
	if op.Policy.Runtime != "" {
		m.Runtime = op.Policy.Runtime
	}
	if op.Policy.Idempotent != nil {
		m.Idempotent = *op.Policy.Idempotent
	}
	if op.Policy.Destructive != nil {
		m.Destructive = *op.Policy.Destructive
	}
	if op.Policy.OpenWorld != nil {
		m.OpenWorld = *op.Policy.OpenWorld
	}
	return m
}
func (op *Operation) validateToolMetadata() error {
	m := op.ToolMetadata()
	if len(m.Tags) == 0 || m.Title == "" || !slices.Contains([]string{"low", "normal", "high"}, m.Risk) || !slices.Contains([]string{"any", "server-running", "shard-ready"}, m.Runtime) {
		return fmt.Errorf("invalid tool metadata for %s", op.ID)
	}
	for _, tag := range m.Tags {
		if tag == "" || strings.ContainsAny(tag, " ,/\t\r\n") {
			return fmt.Errorf("invalid tool tag for %s", op.ID)
		}
	}
	return nil
}

// MetadataFilter uses OR within tags and AND across the remaining dimensions.
// Filtering is discovery only. The ordinary execution guards still apply.
type MetadataFilter struct {
	Tags    []string `json:"tags,omitempty"`
	Access  string   `json:"access,omitempty"`
	Risk    string   `json:"risk,omitempty"`
	Runtime string   `json:"runtime,omitempty"`
}

func (f MetadataFilter) Validate() error {
	if len(f.Tags) > 16 || f.Access != "" && !slices.Contains([]string{"read", "write"}, f.Access) || f.Risk != "" && !slices.Contains([]string{"low", "normal", "high"}, f.Risk) || f.Runtime != "" && !slices.Contains([]string{"any", "server-running", "shard-ready"}, f.Runtime) {
		return fmt.Errorf("invalid tool filter")
	}
	for _, tag := range f.Tags {
		if tag == "" || len(tag) > 40 || strings.ContainsAny(tag, " ,/\t\n\r") {
			return fmt.Errorf("invalid tool tag")
		}
	}
	return nil
}
func (f MetadataFilter) Matches(m ToolMetadata) bool {
	if f.Access != "" && f.Access != m.Access || f.Risk != "" && f.Risk != m.Risk || f.Runtime != "" && f.Runtime != m.Runtime {
		return false
	}
	if len(f.Tags) == 0 {
		return true
	}
	for _, tag := range f.Tags {
		if slices.Contains(m.Tags, tag) {
			return true
		}
	}
	return false
}
