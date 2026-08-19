package rooms

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maximumProvisionFileBytes = int64(2 * 1024 * 1024)
	maximumProvisionBytes     = int64(16 * 1024 * 1024)
)

var provisionSharedFiles = []string{
	"cluster.ini", "cluster_token.txt", "adminlist.txt", "blocklist.txt", "whitelist.txt",
}

var provisionWorldFiles = []string{
	"server.ini", "leveldataoverride.lua", "worldgenoverride.lua", "modoverrides.lua", "customcommands.lua",
	filepath.Join("dst-admin", "bootstrap.lua"), filepath.Join("dst-admin", "telemetry.lua"),
	filepath.Join("dst-admin", "worldstate.lua"), filepath.Join("dst-admin", "commands.lua"),
	filepath.Join("dst-admin", "events.lua"), filepath.Join("dst-admin", "diagnostics.lua"),
	filepath.Join("dst-admin", "barriers.lua"), filepath.Join("dst-admin", "manifest.json"),
}

type ProvisionFile struct {
	Name string
	Data []byte
	Mode os.FileMode
}

type ProvisionWorld struct {
	World World
	Files []ProvisionFile
}

type ProvisionBundle struct {
	Room   Room
	Shared []ProvisionFile
	Worlds []ProvisionWorld
}

func (s *Service) ProvisionBundle(roomID string) (ProvisionBundle, error) {
	room, err := s.catalog.Room(roomID)
	if err != nil {
		return ProvisionBundle{}, err
	}
	if !room.Managed {
		return ProvisionBundle{}, ErrRoomNotManaged
	}
	worlds, err := s.catalog.Worlds(room.ID)
	if err != nil {
		return ProvisionBundle{}, err
	}
	root := filepath.Join(s.catalog.root, room.DirectoryName)
	shared, total, err := readProvisionFiles(root, provisionSharedFiles, map[string]bool{"cluster.ini": true})
	if err != nil {
		return ProvisionBundle{}, err
	}
	bundle := ProvisionBundle{Room: room, Shared: shared, Worlds: make([]ProvisionWorld, 0, len(worlds))}
	for _, world := range worlds {
		files, size, readErr := readProvisionFiles(filepath.Join(root, world.DirectoryName), provisionWorldFiles, map[string]bool{"server.ini": true})
		if readErr != nil {
			return ProvisionBundle{}, fmt.Errorf("read world %s provision files: %w", world.Name, readErr)
		}
		if total > maximumProvisionBytes-size {
			return ProvisionBundle{}, errors.New("managed room configuration exceeds the provisioning limit")
		}
		total += size
		bundle.Worlds = append(bundle.Worlds, ProvisionWorld{World: world, Files: files})
	}
	return bundle, nil
}

func readProvisionFiles(root string, names []string, required map[string]bool) ([]ProvisionFile, int64, error) {
	files := make([]ProvisionFile, 0, len(names))
	total := int64(0)
	for _, name := range names {
		clean := filepath.Clean(name)
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return nil, 0, ErrUnsafePath
		}
		path := filepath.Join(root, clean)
		if err := ensureContained(root, path); err != nil {
			return nil, 0, err
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && !required[name] {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximumProvisionFileBytes {
			return nil, 0, ErrUnsafePath
		}
		if total > maximumProvisionBytes-info.Size() {
			return nil, 0, errors.New("managed room configuration exceeds the provisioning limit")
		}
		data, err := os.ReadFile(path)
		if err != nil || int64(len(data)) != info.Size() {
			return nil, 0, errors.Join(err, ErrInvalidRoom)
		}
		total += int64(len(data))
		files = append(files, ProvisionFile{Name: filepath.ToSlash(clean), Data: data, Mode: info.Mode().Perm()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, total, nil
}
