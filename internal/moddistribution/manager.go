package moddistribution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	workshopIDPattern  = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)
)

type Manager struct {
	mu            sync.Mutex
	cacheRoot     string
	stateRoot     string
	nodeID        string
	reserveBytes  int64
	installations map[string]TrustedInstallation
}

func New(config Config) (*Manager, error) {
	cacheRoot, err := cleanConfiguredRoot(config.CacheRoot, true)
	if err != nil {
		return nil, err
	}
	stateRoot, err := cleanConfiguredRoot(config.StateRoot, true)
	if err != nil {
		return nil, err
	}
	if cacheRoot == stateRoot || pathWithin(cacheRoot, stateRoot) || pathWithin(stateRoot, cacheRoot) {
		return nil, ErrUnsafePath
	}
	nodeID := strings.TrimSpace(config.NodeID)
	if !validIdentity(nodeID) || config.ReserveBytes < 0 {
		return nil, ErrInvalidInput
	}
	manager := &Manager{
		cacheRoot: cacheRoot, stateRoot: stateRoot, nodeID: nodeID,
		reserveBytes: config.ReserveBytes, installations: make(map[string]TrustedInstallation, len(config.Installations)),
	}
	for _, input := range config.Installations {
		if !validIdentity(input.ID) || strings.TrimSpace(input.NodeID) != nodeID {
			return nil, ErrInvalidInput
		}
		if _, exists := manager.installations[input.ID]; exists {
			return nil, ErrConflict
		}
		server, err := cleanConfiguredRoot(input.ServerPath, false)
		if err != nil {
			return nil, err
		}
		save, err := cleanConfiguredRoot(input.SavePath, false)
		if err != nil {
			return nil, err
		}
		if pathWithin(server, cacheRoot) || pathWithin(save, cacheRoot) || pathWithin(server, stateRoot) || pathWithin(save, stateRoot) {
			return nil, ErrUnsafePath
		}
		input.ServerPath, input.SavePath, input.NodeID = server, save, nodeID
		manager.installations[input.ID] = input
	}
	if len(manager.installations) == 0 {
		return nil, ErrInvalidInput
	}
	for _, directory := range []string{filepath.Join(cacheRoot, ".staging"), filepath.Join(stateRoot, "journals")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
	}
	if _, err := manager.Recover(context.Background()); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) BuildPlan(ctx context.Context, input PlanInput) (Plan, error) {
	if !operationIDPattern.MatchString(input.OperationID) || input.NodeID != m.nodeID || len(input.Shards) == 0 {
		return Plan{}, ErrInvalidInput
	}
	groups := make(map[string]*InstallationPlan)
	destinations := make(map[string]string)
	totalConfigBytes := 0
	for _, raw := range input.Shards {
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		installation, exists := m.installations[raw.InstallationID]
		if !exists || !validIdentity(raw.RoomID) || !validIdentity(raw.WorldID) || !safeComponent(raw.RoomDirectory) || !safeComponent(raw.WorldDirectory) {
			return Plan{}, ErrInvalidInput
		}
		if len(raw.ModOverrides) == 0 {
			raw.ModOverrides = RenderDefaultOverrides(raw.Mods)
		}
		if len(raw.ModOverrides) > maxOverridesBytes || strings.IndexByte(string(raw.ModOverrides), 0) >= 0 {
			return Plan{}, ErrInvalidInput
		}
		if totalConfigBytes > maxPlanConfigBytes-len(raw.ModOverrides) {
			return Plan{}, ErrInvalidInput
		}
		totalConfigBytes += len(raw.ModOverrides)
		targetKey := raw.InstallationID + "\x00" + raw.RoomDirectory + "\x00" + raw.WorldDirectory
		if _, duplicate := destinations[targetKey]; duplicate {
			return Plan{}, ErrConflict
		}
		destinations[targetKey] = raw.RoomID + "\x00" + raw.WorldID
		group := groups[raw.InstallationID]
		if group == nil {
			group = &InstallationPlan{InstallationID: raw.InstallationID, NodeID: installation.NodeID}
			groups[raw.InstallationID] = group
		}
		seen := make(map[string]string, len(group.Mods)+len(raw.Mods))
		for _, mod := range group.Mods {
			seen[mod.WorkshopID] = mod.TreeSHA256
		}
		normalizedMods := make([]ModVersion, 0, len(raw.Mods))
		shardSeen := make(map[string]bool, len(raw.Mods))
		for _, mod := range raw.Mods {
			mod.TreeSHA256 = strings.ToLower(strings.TrimSpace(mod.TreeSHA256))
			if !validWorkshopID(mod.WorkshopID) || !validSHA256(mod.TreeSHA256) || shardSeen[mod.WorkshopID] {
				return Plan{}, ErrInvalidInput
			}
			if prior, exists := seen[mod.WorkshopID]; exists && prior != mod.TreeSHA256 {
				return Plan{}, fmt.Errorf("%w: installation %s requests Workshop %s as both %s and %s", ErrConflict, raw.InstallationID, mod.WorkshopID, prior, mod.TreeSHA256)
			}
			if _, err := m.Verify(ctx, mod.WorkshopID, mod.TreeSHA256); err != nil {
				return Plan{}, err
			}
			seen[mod.WorkshopID], shardSeen[mod.WorkshopID] = mod.TreeSHA256, true
			normalizedMods = append(normalizedMods, mod)
		}
		sortMods(normalizedMods)
		raw.Mods = normalizedMods
		group.Shards = append(group.Shards, raw)
		group.Mods = group.Mods[:0]
		for id, hash := range seen {
			group.Mods = append(group.Mods, ModVersion{WorkshopID: id, TreeSHA256: hash})
		}
		sortMods(group.Mods)
	}
	plan := Plan{OperationID: input.OperationID, NodeID: m.nodeID, CreatedAt: time.Now().UTC()}
	for _, group := range groups {
		sort.Slice(group.Shards, func(i, j int) bool {
			if group.Shards[i].RoomDirectory == group.Shards[j].RoomDirectory {
				return group.Shards[i].WorldDirectory < group.Shards[j].WorldDirectory
			}
			return group.Shards[i].RoomDirectory < group.Shards[j].RoomDirectory
		})
		setupPath := filepath.Join(m.installations[group.InstallationID].ServerPath, "mods", "dedicated_server_mods_setup.lua")
		current, err := readOptionalRegular(setupPath)
		if err != nil {
			return Plan{}, err
		}
		group.ManagedSetup, err = composeManagedSetup(current, group.Mods)
		if err != nil {
			return Plan{}, err
		}
		if current != nil {
			group.SetupBaseSHA256 = shaBytes(current)
		}
		plan.Installations = append(plan.Installations, *group)
	}
	sort.Slice(plan.Installations, func(i, j int) bool {
		return plan.Installations[i].InstallationID < plan.Installations[j].InstallationID
	})
	return plan, nil
}

func RenderDefaultOverrides(mods []ModVersion) []byte {
	copyMods := append([]ModVersion(nil), mods...)
	sortMods(copyMods)
	var builder strings.Builder
	builder.WriteString("return {\n")
	for _, mod := range copyMods {
		if !validWorkshopID(mod.WorkshopID) {
			continue
		}
		fmt.Fprintf(&builder, "  [\"workshop-%s\"] = { enabled = true },\n", mod.WorkshopID)
	}
	builder.WriteString("}\n")
	return []byte(builder.String())
}

func (m *Manager) cacheVersionRoot(workshopID, treeSHA string) string {
	return filepath.Join(m.cacheRoot, workshopID, strings.ToLower(treeSHA))
}

func (m *Manager) journalPath(operationID string) string {
	return filepath.Join(m.stateRoot, "journals", operationID+".json")
}

func (m *Manager) statePath() string {
	return filepath.Join(m.stateRoot, "installations.json")
}

func cleanConfiguredRoot(value string, create bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !filepath.IsAbs(value) {
		return "", ErrInvalidInput
	}
	value = filepath.Clean(value)
	if create {
		if err := os.MkdirAll(value, 0o700); err != nil {
			return "", err
		}
	}
	return trustedExistingDirectory(value)
}

func trustedExistingDirectory(value string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(value))
	if err != nil || strings.TrimSpace(value) == "" {
		return "", ErrInvalidInput
	}
	absolute = filepath.Clean(absolute)
	if err := rejectSymlinkComponents(absolute); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(resolved)
	if err := rejectSymlinkComponents(absolute); err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrUnsafePath
	}
	return absolute, nil
}

func rejectSymlinkComponents(path string) error {
	current := filepath.Clean(path)
	components := make([]string, 0, 16)
	for {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	for index := len(components) - 1; index >= 0; index-- {
		info, err := os.Lstat(components[index])
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
	}
	return nil
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func validWorkshopID(value string) bool { return workshopIDPattern.MatchString(value) }

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validIdentity(value string) bool {
	trimmed := strings.TrimSpace(value)
	return value == trimmed && value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\x00\r\n")
}

func safeComponent(value string) bool {
	trimmed := strings.TrimSpace(value)
	return value == trimmed && value != "" && value != "." && value != ".." && len(value) <= 255 && filepath.Base(value) == value && !strings.ContainsAny(value, "/\\\x00\r\n")
}

func sortMods(mods []ModVersion) {
	sort.Slice(mods, func(i, j int) bool { return mods[i].WorkshopID < mods[j].WorkshopID })
}

func shaBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
