package gameupdate

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/agents"
	"dont/internal/runtimedriver"
	"dont/shared"
)

type versionTargetCatalog interface {
	RuntimeTargets() ([]agents.RuntimeTarget, error)
}

type installedVersionDriver interface {
	ObserveGameVersion(context.Context, runtimedriver.Target) (shared.RuntimeGameVersionResult, error)
}

type InstalledVersion struct {
	TargetID       string    `json:"targetId"`
	TargetName     string    `json:"targetName"`
	InstallationID string    `json:"installationId"`
	OS             string    `json:"os"`
	Arch           string    `json:"arch"`
	Online         bool      `json:"online"`
	Installed      bool      `json:"installed"`
	AppID          string    `json:"appId"`
	UpdateMethod   string    `json:"updateMethod"`
	GameVersion    string    `json:"gameVersion"`
	SteamBuild     string    `json:"steamBuild"`
	Branch         string    `json:"branch"`
	CheckedAt      time.Time `json:"checkedAt"`
	Error          string    `json:"error,omitempty"`
}

type InstalledVersionService struct {
	rooms         releaseRoomCatalog
	placements    releasePlacementReader
	targets       versionTargetCatalog
	local, remote installedVersionDriver
}

func NewInstalledVersionService(rooms releaseRoomCatalog, placements releasePlacementReader, targets versionTargetCatalog, local, remote installedVersionDriver) *InstalledVersionService {
	return &InstalledVersionService{rooms: rooms, placements: placements, targets: targets, local: local, remote: remote}
}

// Installed reads each applied installation once without a topology inventory,
// world status probe, Steam lookup, or update plan.
func (s *InstalledVersionService) Installed(ctx context.Context, targetIDs []string) ([]InstalledVersion, error) {
	selected, err := normalizeReleaseTargetIDs(targetIDs)
	if err != nil {
		return nil, err
	}
	targets, err := s.targets.RuntimeTargets()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]agents.RuntimeTarget, len(targets))
	for _, target := range targets {
		byID[target.ID] = target
	}
	rooms, err := s.rooms.List()
	if err != nil {
		return nil, err
	}
	values := []InstalledVersion{}
	routes := []runtimedriver.Target{}
	seen := map[string]bool{}
	for _, room := range rooms {
		if !room.Managed {
			continue
		}
		worlds, err := s.rooms.Worlds(room.ID)
		if err != nil {
			return nil, err
		}
		for _, world := range worlds {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			placement, err := s.placements.AppliedPlacement(room.ID, world.ID)
			if err != nil {
				return nil, err
			}
			id := placement.AppliedTargetID
			if len(selected) > 0 && !selected[id] {
				continue
			}
			target := byID[id]
			installation := placement.AppliedInstallationID
			if installation == "" {
				installation = target.Config.InstallationID
			}
			if installation == "default" && target.DefaultInstallationID != "" {
				exact := false
				for _, candidate := range target.Installations {
					if candidate.ID == "default" {
						exact = true
					}
				}
				if !exact {
					installation = target.DefaultInstallationID
				}
			}
			if installation == "" && id == "local" {
				installation = "default"
			}
			key := releaseInstallationKey(id, installation)
			if seen[key] {
				continue
			}
			seen[key] = true
			name := target.Name
			if name == "" {
				name = id
			}
			values = append(values, InstalledVersion{TargetID: id, TargetName: name, InstallationID: installation, OS: target.OS, Arch: target.Arch, Online: target.Online})
			routes = append(routes, runtimedriver.Target{TargetID: id, InstallationID: installation, Cluster: room.DirectoryName, Shard: world.DirectoryName, RoomID: room.ID, WorldID: world.ID, TopologyRevision: placement.Revision})
		}
	}
	slots := make(chan struct{}, 2)
	var wait sync.WaitGroup
	for index := range values {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			value, route := &values[index], routes[index]
			if !value.Online {
				value.Error = "运行机器当前离线"
				return
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				value.Error = ctx.Err().Error()
				return
			}
			started := time.Now()
			readContext, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			driver := s.remote
			if route.TargetID == "local" {
				driver = s.local
			}
			if driver == nil || !releaseIdentityPattern.MatchString(route.InstallationID) || route.TargetID != "local" && !strings.HasPrefix(route.TargetID, "agent:") {
				value.Error = "运行机器的游戏安装未配置"
				return
			}
			observed, err := driver.ObserveGameVersion(readContext, route)
			value.Installed, value.GameVersion, value.SteamBuild = observed.Installed, observed.GameVersion, observed.SteamBuild
			value.AppID, value.UpdateMethod, value.Branch, value.CheckedAt = observed.AppID, observed.UpdateMethod, observed.Branch, observed.ObservedAt
			if err != nil {
				value.Error = fmt.Sprintf("读取游戏版本失败: %v", err)
			}
			log.Printf("[GameVersion] target=%s installation=%s stage=installed duration_ms=%d failed=%t", route.TargetID, route.InstallationID, time.Since(started).Milliseconds(), err != nil)
		}(index)
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(values, func(i, j int) bool {
		return releaseInstallationKey(values[i].TargetID, values[i].InstallationID) < releaseInstallationKey(values[j].TargetID, values[j].InstallationID)
	})
	return values, nil
}

func (s *Service) OfficialVersion(ctx context.Context, refresh bool) (OfficialRelease, error) {
	if s.official == nil {
		return OfficialRelease{}, fmt.Errorf("官方版本查询未配置")
	}
	started := time.Now()
	defer func() { log.Printf("[GameVersion] stage=official duration_ms=%d", time.Since(started).Milliseconds()) }()
	ctx, cancel := context.WithTimeout(ctx, defaultReleaseOfficialTimeout)
	defer cancel()
	if checker, ok := s.official.(interface {
		Refresh(context.Context) (OfficialRelease, error)
	}); ok && refresh {
		return checker.Refresh(ctx)
	}
	return s.official.Check(ctx)
}
