package moddistribution

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/disk"
)

// WriteBundle streams a verified immutable cache version as a deterministic
// tar archive. The receiver can import it and reproduce the same tree SHA.
func (m *Manager) WriteBundle(ctx context.Context, workshopID, treeSHA string, output io.Writer) error {
	if output == nil {
		return ErrInvalidInput
	}
	manifest, err := m.Verify(ctx, workshopID, treeSHA)
	if err != nil {
		return err
	}
	content := filepath.Join(m.cacheVersionRoot(manifest.WorkshopID, manifest.TreeSHA256), "content")
	archive := tar.NewWriter(output)
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			_ = archive.Close()
			return err
		}
		header := &tar.Header{
			Name: entry.Path, Mode: int64(entry.Mode), ModTime: time.Unix(0, 0).UTC(),
			AccessTime: time.Unix(0, 0).UTC(), ChangeTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX,
		}
		if entry.Kind == "directory" {
			header.Typeflag = tar.TypeDir
			header.Name += "/"
		} else {
			header.Typeflag = tar.TypeReg
			header.Size = entry.Size
		}
		if err := archive.WriteHeader(header); err != nil {
			_ = archive.Close()
			return err
		}
		if entry.Kind == "directory" {
			continue
		}
		file, err := os.Open(filepath.Join(content, filepath.FromSlash(entry.Path)))
		if err != nil {
			_ = archive.Close()
			return err
		}
		written, copyErr := io.Copy(archive, &contextReader{ctx: ctx, reader: file})
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			_ = archive.Close()
			return err
		}
		if written != entry.Size {
			_ = archive.Close()
			return ErrIntegrity
		}
	}
	return archive.Close()
}

func (m *Manager) Import(ctx context.Context, workshopID, source string, metadata Metadata) (Manifest, error) {
	if !validWorkshopID(workshopID) {
		return Manifest{}, ErrInvalidInput
	}
	source, err := trustedExistingDirectory(source)
	if err != nil {
		return Manifest{}, err
	}
	if pathWithin(source, m.cacheRoot) || pathWithin(m.cacheRoot, source) {
		return Manifest{}, ErrUnsafePath
	}
	entries, size, err := scanTree(ctx, source)
	if err != nil {
		return Manifest{}, err
	}
	treeSHA := hashEntries(entries)
	manifest := Manifest{
		Version: ManifestVersion, WorkshopID: workshopID, TreeSHA256: treeSHA,
		Size: size, FileCount: countFiles(entries), Entries: entries,
		Metadata: metadata, CreatedAt: time.Now().UTC(),
	}
	manifest.ManifestSHA256 = manifestDigest(manifest)
	versionRoot := m.cacheVersionRoot(workshopID, treeSHA)
	if current, err := m.readManifest(workshopID, treeSHA); err == nil {
		if verifyErr := m.verifyManifest(ctx, current); verifyErr != nil {
			return Manifest{}, verifyErr
		}
		merged := mergeMetadata(current.Metadata, metadata)
		if merged != current.Metadata {
			current.Metadata = merged
			current.ManifestSHA256 = manifestDigest(current)
			if writeErr := rewriteCachedManifest(versionRoot, current); writeErr != nil {
				return Manifest{}, writeErr
			}
		}
		return current, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	if err := ensureDiskSpace(m.cacheRoot, size, m.reserveBytes); err != nil {
		return Manifest{}, err
	}
	stage, err := os.MkdirTemp(filepath.Join(m.cacheRoot, ".staging"), workshopID+"-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(stage)
	defer func() { _ = makeTreeWritable(stage) }()
	content := filepath.Join(stage, "content")
	if err := copyManifestTree(ctx, source, content, manifest); err != nil {
		return Manifest{}, err
	}
	if err := writeJSONAtomic(filepath.Join(stage, "manifest.json"), manifest, 0o600); err != nil {
		return Manifest{}, err
	}
	if err := syncTree(stage); err != nil {
		return Manifest{}, err
	}
	if err := os.MkdirAll(filepath.Dir(versionRoot), 0o700); err != nil {
		return Manifest{}, err
	}
	if err := os.Rename(stage, versionRoot); err != nil {
		if _, statErr := os.Stat(versionRoot); statErr == nil {
			current, readErr := m.readManifest(workshopID, treeSHA)
			if readErr == nil && m.verifyManifest(ctx, current) == nil {
				return current, nil
			}
		}
		return Manifest{}, err
	}
	if err := os.Chmod(filepath.Join(versionRoot, "manifest.json"), 0o444); err != nil {
		return Manifest{}, err
	}
	if err := os.Chmod(versionRoot, 0o555); err != nil {
		return Manifest{}, err
	}
	if err := syncDirectory(filepath.Dir(versionRoot)); err != nil {
		return Manifest{}, err
	}
	return manifest, m.verifyManifest(ctx, manifest)
}

func mergeMetadata(current, incoming Metadata) Metadata {
	if incoming.Title != "" {
		current.Title = incoming.Title
	}
	if incoming.Version != "" {
		current.Version = incoming.Version
	}
	if incoming.PublishedFileSize > 0 {
		current.PublishedFileSize = incoming.PublishedFileSize
	}
	if incoming.SteamManifestID != "" && (current.SteamManifestID == "" || current.SteamUpdatedAt.IsZero() || !incoming.SteamUpdatedAt.Before(current.SteamUpdatedAt)) {
		current.SteamManifestID = incoming.SteamManifestID
		current.SteamUpdatedAt = incoming.SteamUpdatedAt
	} else if current.SteamUpdatedAt.IsZero() && !incoming.SteamUpdatedAt.IsZero() {
		current.SteamUpdatedAt = incoming.SteamUpdatedAt
	}
	return current
}

func rewriteCachedManifest(versionRoot string, manifest Manifest) (result error) {
	info, err := os.Stat(versionRoot)
	if err != nil {
		return err
	}
	originalMode := info.Mode().Perm()
	if err := os.Chmod(versionRoot, 0o700); err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.Chmod(versionRoot, originalMode)) }()
	path := filepath.Join(versionRoot, "manifest.json")
	if err := writeJSONAtomic(path, manifest, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o444); err != nil {
		return err
	}
	return syncDirectory(versionRoot)
}

func (m *Manager) Verify(ctx context.Context, workshopID, treeSHA string) (Manifest, error) {
	manifest, err := m.readManifest(workshopID, treeSHA)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Manifest{}, ErrNotFound
		}
		return Manifest{}, err
	}
	if err := m.verifyManifest(ctx, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m *Manager) readManifest(workshopID, treeSHA string) (Manifest, error) {
	if !validWorkshopID(workshopID) || !validSHA256(treeSHA) {
		return Manifest{}, ErrInvalidInput
	}
	var manifest Manifest
	err := readJSON(filepath.Join(m.cacheVersionRoot(workshopID, strings.ToLower(treeSHA)), "manifest.json"), &manifest)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.Version != ManifestVersion || manifest.WorkshopID != workshopID || !strings.EqualFold(manifest.TreeSHA256, treeSHA) || !validSHA256(manifest.ManifestSHA256) || manifestDigest(manifest) != strings.ToLower(manifest.ManifestSHA256) {
		return Manifest{}, ErrIntegrity
	}
	return manifest, nil
}

func (m *Manager) verifyManifest(ctx context.Context, manifest Manifest) error {
	if manifest.Version != ManifestVersion || !validWorkshopID(manifest.WorkshopID) || !validSHA256(manifest.TreeSHA256) {
		return ErrIntegrity
	}
	content := filepath.Join(m.cacheVersionRoot(manifest.WorkshopID, manifest.TreeSHA256), "content")
	entries, size, err := scanTree(ctx, content)
	if err != nil {
		return errors.Join(ErrIntegrity, err)
	}
	if size != manifest.Size || countFiles(entries) != manifest.FileCount || hashEntries(entries) != strings.ToLower(manifest.TreeSHA256) || !sameEntries(entries, manifest.Entries) {
		return ErrIntegrity
	}
	return nil
}

func scanTree(ctx context.Context, root string) ([]Entry, int64, error) {
	root, err := trustedExistingDirectory(root)
	if err != nil {
		return nil, 0, err
	}
	entries := make([]Entry, 0, 64)
	total := int64(0)
	err = filepath.WalkDir(root, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return ErrUnsafePath
		}
		if relative == "." {
			return nil
		}
		if len(entries) >= maxManifestEntries || len(relative) > 2048 {
			return ErrIntegrity
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
		relative = filepath.ToSlash(relative)
		if info.IsDir() {
			entries = append(entries, Entry{Path: relative, Kind: "directory", Mode: 0o555})
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxSingleFileBytes || total > maxManifestBytes-info.Size() {
			return ErrIntegrity
		}
		digest, err := hashFile(ctx, path)
		if err != nil {
			return err
		}
		mode := uint32(0o444)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o555
		}
		entries = append(entries, Entry{Path: relative, Kind: "file", Mode: mode, Size: info.Size(), SHA256: digest})
		total += info.Size()
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, total, nil
}

func hashEntries(entries []Entry) string {
	hash := sha256.New()
	writer := bufio.NewWriter(hash)
	for _, entry := range entries {
		fmt.Fprintf(writer, "%s\x00%s\x00%o\x00%d\x00%s\n", entry.Kind, entry.Path, entry.Mode, entry.Size, strings.ToLower(entry.SHA256))
	}
	_ = writer.Flush()
	return hex.EncodeToString(hash.Sum(nil))
}

func manifestDigest(manifest Manifest) string {
	manifest.ManifestSHA256 = ""
	encoded, _ := json.Marshal(manifest)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func hashFile(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			if _, err := hash.Write(buffer[:read]); err != nil {
				return "", err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyManifestTree(ctx context.Context, source, target string, manifest Manifest) error {
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !safeManifestPath(entry.Path) {
			return ErrIntegrity
		}
		relative := filepath.FromSlash(entry.Path)
		from := filepath.Join(source, relative)
		to := filepath.Join(target, relative)
		if entry.Kind == "directory" {
			if err := os.MkdirAll(to, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		if err := copyVerifiedFile(ctx, from, to, entry); err != nil {
			return err
		}
	}
	for index := len(manifest.Entries) - 1; index >= 0; index-- {
		entry := manifest.Entries[index]
		if entry.Kind == "directory" {
			if err := os.Chmod(filepath.Join(target, filepath.FromSlash(entry.Path)), fs.FileMode(entry.Mode)); err != nil {
				return err
			}
		}
	}
	return os.Chmod(target, 0o555)
}

func safeManifestPath(value string) bool {
	if value == "" || strings.Contains(value, "\\") || strings.ContainsRune(value, 0) || strings.HasPrefix(value, "/") {
		return false
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	return cleaned == value && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func copyVerifiedFile(ctx context.Context, source, target string, entry Entry) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(output, hash), &contextReader{ctx: ctx, reader: input})
	syncErr := output.Sync()
	closeErr := output.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return err
	}
	if written != entry.Size || hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(entry.SHA256) {
		return ErrIntegrity
	}
	return os.Chmod(target, fs.FileMode(entry.Mode))
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(value []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(value)
}

func sameEntries(first, second []Entry) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func countFiles(entries []Entry) int {
	count := 0
	for _, entry := range entries {
		if entry.Kind == "file" {
			count++
		}
	}
	return count
}

func ensureDiskSpace(path string, required, reserve int64) error {
	if required < 0 || reserve < 0 {
		return ErrInvalidInput
	}
	usage, err := disk.Usage(path)
	if err != nil {
		return err
	}
	if required > int64(usage.Free) || reserve > int64(usage.Free)-required {
		return ErrInsufficientSpace
	}
	return nil
}

func readJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 32<<20))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrIntegrity
		}
		return err
	}
	return nil
}

func writeJSONAtomic(path string, value any, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".json-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	keep = true
	return syncDirectory(filepath.Dir(path))
}

func syncTree(root string) error {
	return filepath.WalkDir(root, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() {
			return syncDirectory(path)
		}
		return nil
	})
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
