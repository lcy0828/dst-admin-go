package containers

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sort"
	"strings"
)

const maximumDockerOutput = 128 * 1024

type ExecTransport struct{ binary string }

func NewExecTransport() *ExecTransport {
	binary, _ := exec.LookPath("docker")
	return &ExecTransport{binary: binary}
}

func (t *ExecTransport) Available() bool { return strings.TrimSpace(t.binary) != "" }

func (t *ExecTransport) List(ctx context.Context) ([]Container, error) {
	if !t.Available() {
		return nil, ErrUnavailable
	}
	output, err := exec.CommandContext(ctx, t.binary, "ps", "-a", "--no-trunc", "--format", "{{json .}}").Output()
	if err != nil {
		return nil, err
	}
	items := make([]Container, 0)
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var raw map[string]string
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		labels := parseLabels(raw["Labels"])
		name, image := strings.TrimSpace(raw["Names"]), strings.TrimSpace(raw["Image"])
		managed := labels["com.dst-admin.managed"] == "true"
		if !managed && !strings.Contains(strings.ToLower(name), "dstserver") && !strings.Contains(strings.ToLower(image), "dstserver") && !strings.Contains(strings.ToLower(name), "dst-admin") && !strings.Contains(strings.ToLower(image), "dst-admin") {
			continue
		}
		state := strings.ToLower(strings.TrimSpace(raw["State"]))
		items = append(items, Container{ID: strings.TrimSpace(raw["ID"]), Name: name, Image: image, Command: raw["Command"], State: state, Status: raw["Status"], Ports: raw["Ports"], Labels: labels, Running: state == "running", Managed: managed, CreatedAt: raw["CreatedAt"]})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func (t *ExecTransport) Run(ctx context.Context, id string, action Action) (ActionResult, error) {
	if !t.Available() {
		return ActionResult{}, ErrUnavailable
	}
	arguments := []string{}
	switch action {
	case ActionStart:
		arguments = []string{"start", id}
	case ActionStop:
		arguments = []string{"stop", "--time", "30", id}
	case ActionRestart:
		arguments = []string{"restart", "--time", "30", id}
	case ActionRemove:
		arguments = []string{"rm", id}
	default:
		return ActionResult{}, ErrInvalidInput
	}
	output, err := exec.CommandContext(ctx, t.binary, arguments...).CombinedOutput()
	text := strings.TrimSpace(string(output))
	if len(text) > maximumDockerOutput {
		text = text[:maximumDockerOutput] + "\n[输出已截断]"
	}
	if err != nil {
		return ActionResult{Output: text}, errors.New(nonEmpty(text, err.Error()))
	}
	return ActionResult{Output: text}, nil
}

func parseLabels(value string) map[string]string {
	labels := map[string]string{}
	for _, item := range strings.Split(value, ",") {
		key, raw, found := strings.Cut(item, "=")
		if found && strings.TrimSpace(key) != "" {
			labels[strings.TrimSpace(key)] = strings.TrimSpace(raw)
		}
	}
	return labels
}

func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
