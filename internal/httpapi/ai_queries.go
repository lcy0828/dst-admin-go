package httpapi

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"dont/internal/aicontrol"
	"dont/internal/automation"
	"dont/internal/backups"
	"dont/internal/mods"
	"dont/internal/players"
)

// Reuse domain queries and applied Placement for both local and Agent rooms.
// No AI-specific collector, timer, Steam request or console script is needed.
type AIPlatformQueries struct {
	Players interface {
		RefreshPresence(context.Context, string) (players.PresenceSnapshot, error)
		List(string, players.ListFilter) (players.List, error)
	}
	Mods interface {
		RoomFacts(context.Context, string) (mods.ModList, error)
	}
	ModMetadata interface {
		CachedMetadata(context.Context, []string) (map[string]mods.SteamMod, error)
	}
	Backups interface {
		Policy(string) (backups.Policy, error)
		List(string) ([]backups.Backup, error)
	}
	BackupSets DistributedBackupService
	Automation interface {
		Groups(string) ([]automation.Group, error)
		Tasks(string) ([]automation.Task, error)
		Runs(string, automation.RunFilter) (automation.RunList, error)
	}
}

func aiPage(length int, a aicontrol.Arguments) (int, int) {
	limit := a.Limit
	if limit == 0 {
		limit = 10
	}
	start := min(a.Offset, length)
	return start, min(start+limit, length)
}

func (b *AIBackend) roomPerformance(ctx context.Context, roomID string) (json.RawMessage, error) {
	if b.overview == nil || b.overview.observations == nil {
		return nil, aicontrol.ErrUnavailable
	}
	room, err := b.rooms.rooms.Room(roomID)
	if err != nil {
		return nil, err
	}
	worlds, err := b.rooms.rooms.Worlds(roomID)
	if err != nil {
		return nil, err
	}
	value, err := b.overview.overview.Snapshot(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if b.refreshAIObservation(ctx, value, false) {
		value, err = b.overview.overview.Snapshot(ctx, roomID)
		if err != nil {
			return nil, err
		}
	}
	inventories, err := b.overview.observations.RuntimeTargetInventories(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(value.Shards))
	for _, shard := range value.Shards {
		row := map[string]any{"worldId": shard.WorldID, "worldName": shard.WorldName, "runtimeState": shard.Runtime.State, "available": false, "reason": "process_not_observed"}
		if shard.RuntimeProblem != nil {
			row["runtimeProblem"] = shard.RuntimeProblem
		}
		for _, item := range inventories {
			installation := item.Target.Config.InstallationID
			if installation == "" {
				installation = item.Inventory.Installation.ID
			}
			if item.Target.ID != shard.Placement.AppliedTargetID || installation != shard.Placement.AppliedInstallationID {
				continue
			}
			row["targetId"], row["targetName"], row["observedAt"] = item.Target.ID, item.Target.Name, item.ObservedAt
			row["observationState"], row["staleReason"], row["observationError"] = item.ObservationState, item.StaleReason, item.ObservationError
			if !item.Target.Online || !item.Available || item.Stale {
				row["reason"] = "inventory_unavailable_or_stale"
				break
			}
			if shard.Runtime.State != "running" || shard.RuntimeProblem != nil {
				row["reason"] = "world_not_confirmed_running"
				break
			}
			directory := ""
			for _, world := range worlds {
				if world.ID == shard.WorldID {
					directory = world.DirectoryName
					break
				}
			}
			matches := 0
			for _, process := range item.Inventory.Processes {
				if process.Cluster != room.DirectoryName || directory == "" || process.Shard != directory {
					continue
				}
				matches++
				row["available"], row["reason"], row["pid"] = true, "", process.PID
				row["cpuPercent"], row["memoryRssBytes"], row["startedAt"] = process.CPUPercent, process.RSSBytes, process.StartedAt
				row["memoryAvailable"] = process.RSSBytes > 0
			}
			if matches > 1 {
				row["available"], row["reason"] = false, "multiple_matching_processes"
				delete(row, "cpuPercent")
				delete(row, "memoryRssBytes")
			}
			break
		}
		rows = append(rows, row)
	}
	return json.Marshal(map[string]any{"roomId": roomID, "roomName": room.Name, "shards": rows, "cpuBasis": "process_lifetime_average; 100 percent = one logical CPU, not percentage of whole node", "limitations": "CPU is not an instantaneous sample; a zero CPU value can also mean the collector did not report it. RSS is resident process memory, not total node usage. FPS/tick latency are not measured by this tool."})
}

func (b *AIBackend) roomPlayers(ctx context.Context, a aicontrol.Arguments) (json.RawMessage, error) {
	if b.queries.Players == nil {
		return nil, aicontrol.ErrUnavailable
	}
	presence, refreshErr := b.queries.Players.RefreshPresence(ctx, a.RoomID)
	limit := a.Limit
	if limit == 0 {
		limit = 10
	}
	list, err := b.queries.Players.List(a.RoomID, players.ListFilter{Limit: limit, Offset: a.Offset, SkipAccessLists: true})
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(list.Items))
	for _, player := range list.Items {
		rows = append(rows, map[string]any{"id": player.ID, "name": player.Name, "prefab": player.Prefab, "worldId": player.WorldID, "worldName": player.WorldName, "worldConfirmed": player.WorldConfirmed, "localEntity": player.LocalEntity, "presenceConflict": player.PresenceConflict, "observedWorldIds": player.ObservedWorldIDs, "worldEvidence": player.Fields["world"], "online": player.Online, "presenceStatus": player.PresenceStatus, "presenceObservedAt": player.PresenceObservedAt, "lastSeenAt": player.LastSeenAt})
	}
	result := map[string]any{"roomId": a.RoomID, "items": rows, "total": list.Total, "online": list.Online, "staleOnline": list.StaleOnline, "offline": list.Offline, "limit": limit, "offset": a.Offset, "presence": presence, "countMeaning": "Room online is deduplicated cluster connections, including selecting/loading/migrating players, excluding dedicated [Host]. World counts are local entities only; countKnown=false is unknown, not an empty shard. unlocatedOnline counts online players without a confirmed current shard. worldId/worldName are last-known history when worldConfirmed=false, never a current location. Only simultaneous local entities create a presenceConflict. Stale presence is not confirmed online; total includes player history. Partial/unavailable presence cannot prove an empty room."}
	if refreshErr != nil {
		result["refreshError"] = refreshErr.Error()
		result["currentPresenceAvailable"] = false
	} else {
		result["currentPresenceAvailable"] = presence.Fresh
	}
	return json.Marshal(result)
}

func (b *AIBackend) roomMods(ctx context.Context, a aicontrol.Arguments) (json.RawMessage, error) {
	if b.queries.Mods == nil {
		return nil, aicontrol.ErrUnavailable
	}
	list, err := b.queries.Mods.RoomFacts(ctx, a.RoomID)
	if err != nil {
		return nil, err
	}
	start, end := aiPage(len(list.Items), a)
	metadata := map[string]mods.SteamMod{}
	if b.queries.ModMetadata != nil && start < end {
		ids := make([]string, 0, end-start)
		for _, mod := range list.Items[start:end] {
			ids = append(ids, mod.ID)
		}
		// Metadata batches are limited to 100 by the shared cache API.
		for offset := 0; offset < len(ids); offset += 100 {
			cached, cacheErr := b.queries.ModMetadata.CachedMetadata(ctx, ids[offset:min(offset+100, len(ids))])
			if cacheErr != nil {
				list.MetadataWarning = cacheErr.Error()
				break
			}
			for id, mod := range cached {
				metadata[id] = mod
			}
		}
	}
	rows := make([]any, 0, end-start)
	for _, mod := range list.Items[start:end] {
		if mod.Name == "" {
			mod.Name = metadata[mod.ID].Name
		}
		rows = append(rows, map[string]any{"id": mod.ID, "name": mod.Name, "configured": mod.Configured, "enabled": mod.Enabled, "installed": mod.Installed, "downloaded": mod.Downloaded, "loaded": mod.Loaded, "configuredWorlds": mod.ConfiguredWorlds, "enabledWorlds": mod.EnabledWorlds, "installedWorlds": mod.InstalledWorlds, "loadedWorlds": mod.LoadedWorlds, "runtimeObserved": mod.RuntimeObserved, "runtimeFileStatus": mod.RuntimeFileStatus, "runtimeVersion": mod.RuntimeVersion, "runtimeVersionStatus": mod.RuntimeVersionStatus, "health": mod.Health, "healthMessage": mod.HealthMessage, "warnings": mod.Warnings})
	}
	return json.Marshal(map[string]any{"roomId": a.RoomID, "items": rows, "total": len(list.Items), "offset": start, "hasMore": end < len(list.Items), "checkedAt": list.CheckedAt, "metadataWarning": list.MetadataWarning, "meaning": "Room mod configuration/file/load facts; not pending load count. No new Steam update check was made; missing cached names do not mean a mod is absent."})
}

func (b *AIBackend) automationTasks(roomID string) ([]map[string]any, map[string]string) {
	errors := map[string]string{}
	if b.queries.Automation == nil {
		errors["automation"] = aicontrol.ErrUnavailable.Error()
		return nil, errors
	}
	tasks, err := b.queries.Automation.Tasks(roomID)
	if err != nil {
		errors["tasks"] = err.Error()
		return nil, errors
	}
	groups, groupErr := b.queries.Automation.Groups(roomID)
	if groupErr != nil {
		errors["groups"] = groupErr.Error()
	}
	enabled := map[string]bool{}
	for _, group := range groups {
		enabled[group.ID] = group.Enabled
	}
	rows := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		row := map[string]any{"id": task.ID, "name": task.Name, "action": task.Action, "enabled": task.Enabled, "groupName": task.GroupName, "schedule": task.Schedule, "timezone": task.Timezone, "worldIds": task.WorldIDs, "lastRunAt": task.LastRunAt, "lastStatus": task.LastStatus, "lastJobId": task.LastJobID}
		if task.LastRunAt != nil {
			row["lastRunLocal"] = aiScheduleTime(task.LastRunAt, task.Timezone)
		}
		groupEnabled, exists := enabled[task.GroupID]
		if !task.Enabled {
			row["effectiveEnabled"] = false
		} else if groupErr == nil && exists {
			row["groupEnabled"], row["effectiveEnabled"] = groupEnabled, groupEnabled
		} else {
			row["effectiveEnabled"] = nil
			row["enablementReason"] = "group_unavailable"
		}
		if row["effectiveEnabled"] == true {
			row["nextRunAt"] = task.NextRunAt
			if task.NextRunAt != nil {
				row["nextRunLocal"] = aiScheduleTime(task.NextRunAt, task.Timezone)
			}
		}
		// Only settings relevant to scheduled backup/update behavior. Lua and
		// notification bodies are not needed for a configuration overview.
		parameters := map[string]any{}
		for _, key := range []string{"keep", "name", "updateGame", "updateMods", "runtimeMode", "emptyMinutes"} {
			if value, ok := task.Parameters[key]; ok {
				parameters[key] = value
			}
		}
		row["parameters"] = parameters
		rows = append(rows, row)
	}
	return rows, errors
}

// Give models an already converted timestamp with an explicit UTC offset.
// A task's schedule timezone must not be used to relabel a UTC database value.
func aiScheduleTime(value *time.Time, zone string) string {
	location, err := time.LoadLocation(zone)
	if err != nil {
		location = time.UTC
	}
	return value.In(location).Format(time.RFC3339)
}

func (b *AIBackend) roomAutomation(a aicontrol.Arguments) (json.RawMessage, error) {
	rows, errors := b.automationTasks(a.RoomID)
	if rows == nil {
		return json.Marshal(map[string]any{"roomId": a.RoomID, "available": false, "errors": errors})
	}
	start, end := aiPage(len(rows), a)
	result := map[string]any{"roomId": a.RoomID, "available": true, "items": rows[start:end], "total": len(rows), "offset": start, "hasMore": end < len(rows), "errors": errors}
	runs, err := b.queries.Automation.Runs(a.RoomID, automation.RunFilter{Limit: 10})
	if err != nil {
		errors["recentRuns"] = err.Error()
	} else {
		recent := make([]any, 0, len(runs.Items))
		for _, run := range runs.Items {
			recent = append(recent, map[string]any{"taskName": run.TaskName, "taskId": run.TaskID, "action": run.Action, "status": run.Status, "startedAt": run.StartedAt, "finishedAt": run.FinishedAt, "error": run.Error, "jobId": run.JobID})
		}
		result["recentRuns"] = recent
	}
	return json.Marshal(result)
}

func (b *AIBackend) roomBackups(roomID string) (json.RawMessage, error) {
	result := map[string]any{"roomId": roomID}
	errors := map[string]string{}
	known, active := true, false
	if b.queries.Backups == nil {
		errors["snapshotPolicy"] = aicontrol.ErrUnavailable.Error()
		known = false
	} else {
		policy, err := b.queries.Backups.Policy(roomID)
		if err != nil {
			errors["snapshotPolicy"] = err.Error()
			known = false
		} else {
			result["snapshotPolicy"] = policy
			active = policy.Enabled
		}
		archives, err := b.queries.Backups.List(roomID)
		if err != nil {
			errors["archives"] = err.Error()
		} else {
			sort.Slice(archives, func(i, j int) bool { return archives[i].CreatedAt.After(archives[j].CreatedAt) })
			rows := make([]any, 0, min(10, len(archives)))
			for _, archive := range archives[:min(10, len(archives))] {
				rows = append(rows, map[string]any{"id": archive.ID, "name": archive.Name, "kind": archive.Kind, "status": archive.Status, "createdAt": archive.CreatedAt, "sizeBytes": archive.Size, "validationError": archive.ValidationError})
			}
			result["recentArchives"], result["archiveTotal"] = rows, len(archives)
		}
	}
	tasks, taskErrors := b.automationTasks(roomID)
	for key, value := range taskErrors {
		errors[key] = value
		known = false
	}
	backupTasks := []any{}
	for _, task := range tasks {
		if task["action"] != automation.ActionBackupCreate {
			continue
		}
		backupTasks = append(backupTasks, task)
		if task["effectiveEnabled"] == true {
			active = true
		} else if task["effectiveEnabled"] == nil {
			known = false
		}
	}
	result["scheduledBackups"] = backupTasks
	if known || active {
		result["automaticBackupEnabled"] = active
	} else {
		result["automaticBackupEnabled"] = nil
	}
	if b.queries.BackupSets == nil {
		errors["backupSets"] = aicontrol.ErrUnavailable.Error()
	} else {
		sets, err := b.queries.BackupSets.List(roomID)
		if err != nil {
			errors["backupSets"] = err.Error()
		} else {
			sort.Slice(sets, func(i, j int) bool { return sets[i].CreatedAt.After(sets[j].CreatedAt) })
			rows := make([]any, 0, min(10, len(sets)))
			for _, set := range sets[:min(10, len(sets))] {
				rows = append(rows, map[string]any{"id": set.ID, "name": set.Name, "status": set.Status, "createdAt": set.CreatedAt, "kind": set.Kind})
			}
			result["recentBackupSets"], result["backupSetTotal"] = rows, len(sets)
		}
	}
	result["errors"], result["checkedAt"] = errors, time.Now().UTC()
	return json.Marshal(result)
}
