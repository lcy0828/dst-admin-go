package moddiagnostics

import (
	"context"
	"slices"
	"time"

	"dont/internal/logstream"
	"dont/internal/mods"
	"dont/internal/rooms"
	"dont/internal/runtimeaudit"
	"dont/internal/runtimedriver"
)

type Catalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}
type ModReader interface {
	RoomCompatibilityFacts(context.Context, string) (mods.ModList, error)
	ConfigurationFile(context.Context, string, string) (mods.ConfigurationFile, error)
}
type LogReader interface {
	SnapshotContext(context.Context, string, string, int, string) (logstream.Snapshot, error)
}
type EventReader interface {
	List(string, runtimeaudit.ListFilter) (runtimeaudit.List, error)
}
type RuntimeRouter interface {
	EndpointTarget(context.Context, string, string) (runtimedriver.RuntimeEndpoint, runtimedriver.Target, error)
}

type RuntimeSource struct {
	Catalog Catalog
	Mods    ModReader
	Logs    LogReader
	Events  EventReader
	Router  RuntimeRouter
}

func (s *RuntimeSource) Collect(ctx context.Context, scope Scope, facts *mods.ModList, factsErr error, includeLogs bool) (Input, error) {
	room, err := s.Catalog.Room(scope.RoomID)
	if err != nil {
		return Input{}, err
	}
	world, err := s.Catalog.World(scope.RoomID, scope.WorldID)
	if err != nil {
		return Input{}, err
	}
	o := Observation{RoomName: room.Name, RoomID: scope.RoomID, WorldID: scope.WorldID, WorldName: world.Name, Mods: []Mod{},
		ObservedAt: time.Now().UTC(), Limitations: []string{}, RuntimeMode: scope.RuntimeMode, OS: scope.OS}
	in := Input{Observation: o, IncludeLogs: includeLogs}
	endpoint, target, err := s.Router.EndpointTarget(ctx, scope.RoomID, scope.WorldID)
	if err != nil {
		in.Observation.Limitations = append(in.Observation.Limitations, "RUNTIME_UNAVAILABLE")
		return in, nil // Never read Controller files on behalf of an unavailable Agent.
	}
	o.TargetID, o.InstallationID, o.TopologyRevision = target.TargetID, target.InstallationID, target.TopologyRevision
	if scope.TargetID != "" && (scope.TargetID != target.TargetID || scope.InstallationID != target.InstallationID || scope.TopologyRevision != "" && scope.TopologyRevision != target.TopologyRevision) {
		o.Limitations = append(o.Limitations, "PLACEMENT_CHANGED")
		in.Observation = o
		return in, nil
	}
	if endpoint.Driver != nil {
		if status, statusErr := endpoint.Driver.Status(ctx, target); statusErr == nil {
			o.RuntimeState = status.State
			if status.RuntimeMode != "" {
				o.RuntimeMode = string(status.RuntimeMode)
			}
		} else {
			o.Limitations = append(o.Limitations, "RUNTIME_STATUS_UNAVAILABLE")
		}
	}
	o.Version = scope.Version
	if o.Version.AppID == "" && endpoint.GameVersion != nil {
		if v, versionErr := endpoint.GameVersion.ObserveGameVersion(ctx, target); versionErr == nil {
			o.Version = Version{AppID: v.AppID, SteamBuild: v.SteamBuild, GameVersion: v.GameVersion}
		} else {
			o.Limitations = append(o.Limitations, "GAME_VERSION_UNAVAILABLE")
		}
	}
	if o.Version.AppID == "" {
		o.Limitations = append(o.Limitations, "GAME_VERSION_UNAVAILABLE")
	}
	if factsErr != nil || facts == nil {
		o.Limitations = append(o.Limitations, "MOD_FACTS_UNAVAILABLE")
	} else {
		for _, m := range facts.Items {
			if !slices.Contains(m.EnabledWorlds, scope.WorldID) {
				continue
			}
			if len(o.Mods) == MaxMods {
				o.Limitations = append(o.Limitations, "MOD_LIMIT_REACHED")
				break
			}
			item := Mod{ID: m.ID, Name: redact(m.Name), Dependencies: append([]string(nil), m.Dependencies...)}
			if !m.CachedDependenciesAvailable {
				o.Limitations = append(o.Limitations, "DECLARED_DEPENDENCIES_UNKNOWN")
			}
			if len(item.Dependencies) > 64 {
				item.Dependencies = item.Dependencies[:64]
				o.Limitations = append(o.Limitations, "DEPENDENCY_LIMIT_REACHED")
			}
			// Room-wide current/latest values may describe another installation.
			// Only this exact placement supplies the installed version evidence.
			for _, v := range m.RuntimeVersions {
				if v.TargetID == target.TargetID && v.InstallationID == target.InstallationID {
					item.Version, item.Manifest = v.Version, v.SteamManifestID
					if v.Name != "" {
						item.Name = redact(v.Name)
					}
					item.DSTCompatible, item.APIVersion, item.FileStatus = v.DSTCompatible, v.APIVersion, v.FileStatus
					// A file may exist in Workshop content while DST's local
					// entry/link is missing or invalid. Presence is not usable installation.
					if v.Status == "not_installed" && item.FileStatus == "ready" {
						item.FileStatus = "invalid"
					}
					if v.MetadataReason != "" {
						o.Limitations = append(o.Limitations, "MOD_METADATA_UNAVAILABLE")
					}
					break
				}
			}

			if item.FileStatus == "ready" {
				item.FileStatus = "present"
			}
			if item.FileStatus == "" {
				item.FileStatus = "unknown"
			}
			if item.DSTCompatible == nil {
				o.Limitations = append(o.Limitations, "DST_DECLARATION_UNKNOWN")
			}

			if item.FileStatus == "unknown" {
				o.Limitations = append(o.Limitations, "MOD_FILE_STATUS_UNKNOWN")
			}
			o.Mods = append(o.Mods, item)
		}
		slices.SortFunc(o.Mods, func(a, b Mod) int {
			if a.ID < b.ID {
				return -1
			}
			if a.ID > b.ID {
				return 1
			}
			return 0
		})
	}
	if config, configErr := s.Mods.ConfigurationFile(ctx, scope.RoomID, scope.WorldID); configErr == nil {
		o.ConfigurationRevision = config.Revision
	} else {
		o.Limitations = append(o.Limitations, "CONFIGURATION_UNREADABLE_OR_UNSUPPORTED")
	}
	if includeLogs {
		if events, eventErr := s.Events.List(scope.RoomID, runtimeaudit.ListFilter{WorldID: scope.WorldID, Limit: 100}); eventErr == nil {
			for _, e := range events.Items {
				if e.TargetID != "" && e.TargetID != target.TargetID || e.TopologyRevision != "" && e.TopologyRevision != target.TopologyRevision {
					continue
				}
				if in.LatestStart == nil && (e.Type == runtimeaudit.EventStartRequested || e.Type == runtimeaudit.EventRestartRequested) {
					t := e.OccurredAt
					in.LatestStart = &t
				}
				if in.IncidentAt == nil && (e.Type == runtimeaudit.EventFailed || e.Type == runtimeaudit.EventUnexpectedExit) {
					t := e.OccurredAt
					in.IncidentAt = &t
				}
			}
		} else {
			o.Limitations = append(o.Limitations, "RUNTIME_EVENTS_UNAVAILABLE")
		}
		if in.LatestStart != nil && in.IncidentAt != nil && in.IncidentAt.Before(*in.LatestStart) {
			in.IncidentAt = nil
		}
		if log, logErr := s.Logs.SnapshotContext(ctx, scope.RoomID, scope.WorldID, 1000, ""); logErr == nil {
			in.Logs = &log
		}
	}
	// Each subsystem resolves the applied Placement independently. Refuse to
	// combine observations if a migration raced these bounded reads.
	_, after, afterErr := s.Router.EndpointTarget(ctx, scope.RoomID, scope.WorldID)
	if afterErr != nil || after.TargetID != target.TargetID || after.InstallationID != target.InstallationID || after.TopologyRevision != target.TopologyRevision {
		o.Limitations = append(o.Limitations, "PLACEMENT_CHANGED", "MOD_FACTS_UNAVAILABLE")
		o.Mods = []Mod{}
		o.Version = Version{}
		o.ConfigurationRevision = ""
		in.Logs = nil
		in.LatestStart = nil
		in.IncidentAt = nil
	}
	o.Limitations = unique(o.Limitations)
	in.Observation = o
	return in, nil
}
