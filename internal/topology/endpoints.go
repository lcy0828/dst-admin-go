package topology

import (
	"fmt"
	"sort"
	"strings"

	"dont/internal/agents"
	"dont/shared"
)

type placementEndpoint struct {
	targetID       string
	installationID string
}

func endpointFor(targetID, installationID string) placementEndpoint {
	return placementEndpoint{
		targetID: strings.TrimSpace(targetID), installationID: strings.TrimSpace(installationID),
	}
}

func installationIDForInventory(inventory agents.RuntimeTargetInventory) string {
	for _, value := range []string{
		inventory.Target.Config.InstallationID,
		inventory.Inventory.Installation.ID,
		inventory.Target.DefaultInstallationID,
	} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "default"
}

func defaultInstallationID(target agents.RuntimeTarget) string {
	for _, value := range []string{target.DefaultInstallationID, target.Config.InstallationID} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	if len(target.Installations) > 0 {
		return strings.TrimSpace(target.Installations[0].ID)
	}
	return "default"
}

func endpointInventories(values []agents.RuntimeTargetInventory) map[placementEndpoint]agents.RuntimeTargetInventory {
	result := make(map[placementEndpoint]agents.RuntimeTargetInventory, len(values))
	for _, value := range values {
		result[endpointFor(value.Target.ID, installationIDForInventory(value))] = value
	}
	return result
}

func targetsByID(values []agents.RuntimeTargetInventory) map[string]agents.RuntimeTarget {
	result := make(map[string]agents.RuntimeTarget, len(values))
	for _, value := range values {
		current, exists := result[value.Target.ID]
		if !exists || installationIDForInventory(value) == defaultInstallationID(value.Target) {
			result[value.Target.ID] = value.Target
			continue
		}
		if len(current.Installations) == 0 && len(value.Target.Installations) > 0 {
			current.Installations = value.Target.Installations
			result[value.Target.ID] = current
		}
	}
	return result
}

func canonicalInstallationID(target agents.RuntimeTarget, requested string) (string, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = defaultInstallationID(target)
	}
	if len(target.Installations) == 0 {
		configured := strings.TrimSpace(target.Config.InstallationID)
		return requested, configured == "" || requested == configured
	}
	for _, installation := range target.Installations {
		if strings.TrimSpace(installation.ID) == requested {
			return requested, true
		}
	}
	return requested, false
}

func canonicalStoredPlacement(value storedPlacement, targets map[string]agents.RuntimeTarget) storedPlacement {
	if target, exists := targets[value.DesiredTargetID]; exists {
		value.DesiredInstallationID, _ = canonicalInstallationID(target, value.DesiredInstallationID)
	}
	if target, exists := targets[value.AppliedTargetID]; exists {
		value.AppliedInstallationID, _ = canonicalInstallationID(target, value.AppliedInstallationID)
	}
	return value
}

func sameEndpoint(leftTarget, leftInstallation, rightTarget, rightInstallation string) bool {
	return endpointFor(leftTarget, leftInstallation) == endpointFor(rightTarget, rightInstallation)
}

func machineInventories(values []agents.RuntimeTargetInventory) map[string]agents.RuntimeTargetInventory {
	grouped := make(map[string][]agents.RuntimeTargetInventory)
	for _, value := range values {
		grouped[value.Target.ID] = append(grouped[value.Target.ID], value)
	}
	result := make(map[string]agents.RuntimeTargetInventory, len(grouped))
	for targetID, group := range grouped {
		selected := group[0]
		for _, value := range group {
			if installationIDForInventory(value) == defaultInstallationID(value.Target) {
				selected = value
				break
			}
		}
		processes := make(map[string]shared.ShardProcessReport)
		rooms := make([]shared.RoomInventoryReport, 0)
		available, stale := false, true
		for _, value := range group {
			if value.Available {
				available = true
			}
			if value.Available && !value.Stale {
				stale = false
			}
			rooms = append(rooms, value.Inventory.Rooms...)
			for _, process := range value.Inventory.Processes {
				key := fmt.Sprintf("%s\x00%d\x00%s", process.RuntimeKind, process.PID, process.InstanceID)
				processes[key] = process
			}
			if value.ObservedAt != nil && (selected.ObservedAt == nil || value.ObservedAt.After(*selected.ObservedAt)) {
				selected.ObservedAt, selected.ReceivedAt = value.ObservedAt, value.ReceivedAt
				selected.Inventory.CPU, selected.Inventory.Memory = value.Inventory.CPU, value.Inventory.Memory
			}
		}
		selected.Available, selected.Stale = available, stale
		selected.Inventory.Rooms = rooms
		selected.Inventory.Processes = make([]shared.ShardProcessReport, 0, len(processes))
		for _, process := range processes {
			selected.Inventory.Processes = append(selected.Inventory.Processes, process)
		}
		sort.Slice(selected.Inventory.Processes, func(i, j int) bool {
			if selected.Inventory.Processes[i].PID == selected.Inventory.Processes[j].PID {
				return selected.Inventory.Processes[i].InstanceID < selected.Inventory.Processes[j].InstanceID
			}
			return selected.Inventory.Processes[i].PID < selected.Inventory.Processes[j].PID
		})
		selected.Capacity = agents.CapacityForResources(
			selected.Inventory.CPU.LogicalProcessors, selected.Inventory.CPU.PhysicalCores,
			selected.Inventory.CPU.PhysicalCoreEstimated, len(selected.Inventory.Processes), selected.Stale,
			selected.Inventory.Memory.TotalBytes, selected.Inventory.Memory.AvailableBytes, 0,
		)
		result[targetID] = selected
	}
	return result
}

func installationSummaries(target agents.RuntimeTarget, values []agents.RuntimeTargetInventory) []TargetInstallationSummary {
	byID := make(map[string]TargetInstallationSummary)
	for _, installation := range target.Installations {
		id := strings.TrimSpace(installation.ID)
		if id != "" {
			byID[id] = TargetInstallationSummary{ID: id, Driver: installation.Driver, Default: id == defaultInstallationID(target), Stale: true}
		}
	}
	for _, inventory := range values {
		if inventory.Target.ID != target.ID {
			continue
		}
		id := installationIDForInventory(inventory)
		summary := byID[id]
		summary.ID = id
		summary.Default = id == defaultInstallationID(target)
		summary.Available = inventory.Available
		summary.Stale = inventory.Stale
		byID[id] = summary
	}
	result := make([]TargetInstallationSummary, 0, len(byID))
	for _, value := range byID {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Default != result[j].Default {
			return result[i].Default
		}
		return result[i].ID < result[j].ID
	})
	return result
}
