package mods

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	managedSetupBegin = "-- DST Admin managed mods begin"
	managedSetupEnd   = "-- DST Admin managed mods end"
)

var setupCallPattern = regexp.MustCompile(`ServerModSetup\s*\(\s*["']([0-9]{1,20})["']\s*\)`)

type fileSnapshot struct {
	data   []byte
	mode   os.FileMode
	exists bool
}

type fileMutation struct {
	path     string
	data     []byte
	previous fileSnapshot
}

type directoryTarget struct {
	root string
	path string
}

type directorySnapshot struct {
	directoryTarget
	stagingRoot string
	stagedPath  string
}

func loadSetup(path string) (fileSnapshot, error) {
	data, mode, exists, err := readModFile(path, true)
	return fileSnapshot{data: data, mode: mode, exists: exists}, err
}

func setupIDs(data []byte) []string {
	matches := setupCallPattern.FindAllSubmatch(data, -1)
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) == 2 {
			values = append(values, string(match[1]))
		}
	}
	return uniqueModIDs(values)
}

func renderManagedSetup(data []byte, managed []string) ([]byte, error) {
	managed = uniqueModIDs(managed)
	sort.Strings(managed)
	source := string(data)
	begin := strings.Index(source, managedSetupBegin)
	end := strings.Index(source, managedSetupEnd)
	if (begin >= 0) != (end >= 0) || (begin >= 0 && end < begin) {
		return nil, errors.New("dedicated_server_mods_setup.lua contains an incomplete DST Admin block")
	}
	block := ""
	if len(managed) > 0 {
		var output strings.Builder
		output.WriteString(managedSetupBegin)
		output.WriteByte('\n')
		for _, id := range managed {
			output.WriteString(fmt.Sprintf("ServerModSetup(%q)\n", id))
		}
		output.WriteString(managedSetupEnd)
		output.WriteByte('\n')
		block = output.String()
	}
	if begin >= 0 {
		end += len(managedSetupEnd)
		for end < len(source) && (source[end] == '\r' || source[end] == '\n') {
			end++
		}
		result := source[:begin] + block + source[end:]
		return []byte(result), nil
	}
	if block == "" {
		return append([]byte(nil), data...), nil
	}
	if source != "" && !strings.HasSuffix(source, "\n") {
		source += "\n"
	}
	return []byte(source + block), nil
}

func applyFileMutations(mutations []fileMutation) error {
	written := make([]fileMutation, 0, len(mutations))
	for _, mutation := range mutations {
		mode := mutation.previous.mode
		if mode == 0 {
			mode = 0640
		}
		if err := os.MkdirAll(filepath.Dir(mutation.path), 0750); err != nil {
			return errors.Join(err, rollbackMutations(written))
		}
		if err := atomicWriteModFile(mutation.path, mutation.data, mode); err != nil {
			return errors.Join(err, rollbackMutations(written))
		}
		written = append(written, mutation)
	}
	return nil
}

func rollbackMutations(written []fileMutation) error {
	var result error
	for index := len(written) - 1; index >= 0; index-- {
		mutation := written[index]
		if !mutation.previous.exists {
			result = errors.Join(result, os.Remove(mutation.path))
			continue
		}
		result = errors.Join(result, atomicWriteModFile(mutation.path, mutation.previous.data, mutation.previous.mode))
	}
	return result
}

func safeRemoveDirectory(root, target string) error {
	targetAbs, err := safeDirectoryTarget(root, target)
	if err != nil {
		return err
	}
	info, err := os.Lstat(targetAbs)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to remove an unsafe Mod path")
	}
	return os.RemoveAll(targetAbs)
}

func safeDirectoryTarget(root, target string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("refusing to access path outside the configured Mod root")
	}
	return targetAbs, nil
}

func stageDirectories(targets []directoryTarget) ([]directorySnapshot, error) {
	snapshots := make([]directorySnapshot, 0, len(targets))
	for _, target := range targets {
		targetAbs, err := safeDirectoryTarget(target.root, target.path)
		if err != nil {
			return nil, errors.Join(err, restoreDirectories(snapshots))
		}
		info, err := os.Lstat(targetAbs)
		if os.IsNotExist(err) {
			snapshots = append(snapshots, directorySnapshot{directoryTarget: directoryTarget{root: target.root, path: targetAbs}})
			continue
		}
		if err != nil {
			return nil, errors.Join(err, restoreDirectories(snapshots))
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(errors.New("refusing to stage an unsafe Mod path"), restoreDirectories(snapshots))
		}
		stagingRoot, err := os.MkdirTemp(filepath.Dir(targetAbs), ".dst-admin-repair-")
		if err != nil {
			return nil, errors.Join(err, restoreDirectories(snapshots))
		}
		stagedPath := filepath.Join(stagingRoot, "original")
		if err := os.Rename(targetAbs, stagedPath); err != nil {
			_ = os.Remove(stagingRoot)
			return nil, errors.Join(err, restoreDirectories(snapshots))
		}
		snapshots = append(snapshots, directorySnapshot{
			directoryTarget: directoryTarget{root: target.root, path: targetAbs},
			stagingRoot:     stagingRoot,
			stagedPath:      stagedPath,
		})
	}
	return snapshots, nil
}

func snapshotDirectories(targets []directoryTarget) ([]directorySnapshot, error) {
	snapshots := make([]directorySnapshot, 0, len(targets))
	for _, target := range targets {
		targetAbs, err := safeDirectoryTarget(target.root, target.path)
		if err != nil {
			return nil, errors.Join(err, restoreDirectories(snapshots))
		}
		info, err := os.Lstat(targetAbs)
		if os.IsNotExist(err) {
			snapshots = append(snapshots, directorySnapshot{directoryTarget: directoryTarget{root: target.root, path: targetAbs}})
			continue
		}
		if err != nil {
			return nil, errors.Join(err, restoreDirectories(snapshots))
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(errors.New("refusing to snapshot an unsafe Mod path"), restoreDirectories(snapshots))
		}
		stagingRoot, err := os.MkdirTemp(filepath.Dir(targetAbs), ".dst-admin-repair-")
		if err != nil {
			return nil, errors.Join(err, restoreDirectories(snapshots))
		}
		stagedPath := filepath.Join(stagingRoot, "original")
		if err := copyDirectory(targetAbs, stagedPath); err != nil {
			_ = os.RemoveAll(stagingRoot)
			return nil, errors.Join(err, restoreDirectories(snapshots))
		}
		snapshots = append(snapshots, directorySnapshot{
			directoryTarget: directoryTarget{root: target.root, path: targetAbs},
			stagingRoot:     stagingRoot,
			stagedPath:      stagedPath,
		})
	}
	return snapshots, nil
}

func copyDirectory(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("refusing to snapshot a Mod directory containing symlinks")
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return errors.New("refusing to snapshot a Mod directory containing special files")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		return errors.Join(copyErr, output.Close(), input.Close())
	})
}

func restoreDirectories(snapshots []directorySnapshot) error {
	var result error
	for index := len(snapshots) - 1; index >= 0; index-- {
		snapshot := snapshots[index]
		result = errors.Join(result, safeRemoveDirectory(snapshot.root, snapshot.path))
		if snapshot.stagingRoot == "" {
			continue
		}
		if err := os.Rename(snapshot.stagedPath, snapshot.path); err != nil {
			result = errors.Join(result, err)
			continue
		}
		result = errors.Join(result, os.Remove(snapshot.stagingRoot))
	}
	return result
}

func discardDirectories(snapshots []directorySnapshot) error {
	var result error
	for _, snapshot := range snapshots {
		if snapshot.stagingRoot == "" {
			continue
		}
		result = errors.Join(result, safeRemoveDirectory(snapshot.root, snapshot.stagingRoot))
	}
	return result
}
