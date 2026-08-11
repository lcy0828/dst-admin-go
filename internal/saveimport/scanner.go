package saveimport

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-ini/ini"
	"golang.org/x/text/unicode/norm"
)

var workshopReferencePattern = regexp.MustCompile(`workshop-([0-9]{5,20})`)
var windowsDrivePattern = regexp.MustCompile(`^[A-Za-z]:`)

type Scanner struct {
	workshopRoot string
	now          func() time.Time
}

type extractionSummary struct {
	Format          string
	ContentSize     int64
	FileCount       int
	EntryCount      int
	NormalizedPaths int
}

func NewScanner(workshopRoot string) *Scanner {
	return &Scanner{workshopRoot: filepath.Clean(strings.TrimSpace(workshopRoot)), now: time.Now}
}

func (s *Scanner) Scan(ctx context.Context, archivePath, sourceName, destination string, compressedSize int64, archiveSHA string) (Manifest, error) {
	summary, err := extractArchive(ctx, archivePath, sourceName, destination)
	if err != nil {
		return Manifest{}, err
	}
	candidates, ignored, diagnostics, err := s.inspectCandidates(destination)
	if err != nil {
		return Manifest{}, err
	}
	if len(candidates) == 0 {
		return Manifest{}, fmt.Errorf("%w: no directory containing cluster.ini was found", ErrInvalidArchive)
	}
	return Manifest{
		SchemaVersion: "1", Format: summary.Format, SourceName: sourceName, CompressedSize: compressedSize,
		ContentSize: summary.ContentSize, FileCount: summary.FileCount, SHA256: archiveSHA,
		NormalizedPaths: summary.NormalizedPaths, IgnoredSystemFiles: ignored,
		Candidates: candidates, Diagnostics: diagnostics, AnalyzedAt: s.now().UTC(),
	}, nil
}

func extractArchive(ctx context.Context, archivePath, sourceName, destination string) (extractionSummary, error) {
	format, err := archiveFormat(archivePath, sourceName)
	if err != nil {
		return extractionSummary{}, err
	}
	summary := extractionSummary{Format: format}
	seen := make(map[string]string)
	writeEntry := func(name string, mode os.FileMode, size int64, directory bool, open func() (io.ReadCloser, error)) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		normalizedName := strings.ReplaceAll(name, "\\", "/")
		if directory && path.Clean(normalizedName) == "." {
			summary.EntryCount++
			if summary.EntryCount > MaxEntries {
				return ErrArchiveTooLarge
			}
			return nil
		}
		clean, normalized, err := safeArchiveName(name)
		if err != nil {
			return err
		}
		if normalized {
			summary.NormalizedPaths++
		}
		canonical := strings.ToLower(norm.NFC.String(clean))
		if previous, exists := seen[canonical]; exists {
			return fmt.Errorf("%w: path collision between %q and %q", ErrUnsafeArchive, previous, name)
		}
		seen[canonical] = name
		if mode&os.ModeSymlink != 0 || mode&os.ModeType != 0 && !mode.IsDir() {
			return fmt.Errorf("%w: unsupported entry %q", ErrUnsafeArchive, name)
		}
		summary.EntryCount++
		if summary.EntryCount > MaxEntries {
			return ErrArchiveTooLarge
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if !contained(destination, target) {
			return ErrUnsafeArchive
		}
		if directory {
			return os.MkdirAll(target, 0750)
		}
		if size < 0 || size > MaxContentBytes || summary.ContentSize > MaxContentBytes-size {
			return ErrArchiveTooLarge
		}
		if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			return err
		}
		source, err := open()
		if err != nil {
			return fmt.Errorf("%w: open %q: %v", ErrInvalidArchive, name, err)
		}
		defer source.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
		if err != nil {
			return err
		}
		written, copyErr := copyContext(ctx, output, io.LimitReader(source, size+1))
		closeErr := output.Close()
		if copyErr != nil {
			return fmt.Errorf("%w: read %q: %v", ErrInvalidArchive, name, copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if written != size {
			return fmt.Errorf("%w: entry %q declared %d bytes but contained %d", ErrInvalidArchive, name, size, written)
		}
		summary.ContentSize += written
		summary.FileCount++
		return nil
	}

	switch format {
	case "zip":
		reader, err := zip.OpenReader(archivePath)
		if err != nil {
			return summary, fmt.Errorf("%w: %v", ErrInvalidArchive, err)
		}
		defer reader.Close()
		if len(reader.File) == 0 {
			return summary, ErrInvalidArchive
		}
		for _, entry := range reader.File {
			entry := entry
			if err := writeEntry(entry.Name, entry.Mode(), int64(entry.UncompressedSize64), entry.FileInfo().IsDir(), entry.Open); err != nil {
				return summary, err
			}
		}
	case "tar", "tar.gz":
		file, err := os.Open(archivePath)
		if err != nil {
			return summary, err
		}
		defer file.Close()
		var input io.Reader = file
		if format == "tar.gz" {
			gzipReader, err := gzip.NewReader(file)
			if err != nil {
				return summary, fmt.Errorf("%w: %v", ErrInvalidArchive, err)
			}
			defer gzipReader.Close()
			input = gzipReader
		}
		reader := tar.NewReader(input)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return summary, fmt.Errorf("%w: %v", ErrInvalidArchive, err)
			}
			directory := header.Typeflag == tar.TypeDir
			if !directory && header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
				return summary, fmt.Errorf("%w: unsupported tar entry %q", ErrUnsafeArchive, header.Name)
			}
			open := func() (io.ReadCloser, error) { return io.NopCloser(reader), nil }
			if err := writeEntry(header.Name, header.FileInfo().Mode(), header.Size, directory, open); err != nil {
				return summary, err
			}
		}
	}
	if summary.FileCount == 0 {
		return summary, ErrInvalidArchive
	}
	return summary, nil
}

func archiveFormat(filePath, sourceName string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	header := make([]byte, 4)
	read, _ := io.ReadFull(file, header)
	if read >= 2 && header[0] == 'P' && header[1] == 'K' {
		return "zip", nil
	}
	if read >= 2 && header[0] == 0x1f && header[1] == 0x8b {
		return "tar.gz", nil
	}
	lower := strings.ToLower(sourceName)
	if strings.HasSuffix(lower, ".tar") {
		return "tar", nil
	}
	return "", fmt.Errorf("%w: only ZIP, TAR, and TAR.GZ are supported", ErrInvalidArchive)
}

func safeArchiveName(name string) (string, bool, error) {
	if name == "" || strings.ContainsAny(name, "\x00\r\n") {
		return "", false, ErrUnsafeArchive
	}
	normalized := strings.Contains(name, "\\")
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "//") || windowsDrivePattern.MatchString(name) {
		return "", normalized, ErrUnsafeArchive
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", normalized, ErrUnsafeArchive
	}
	return strings.TrimSuffix(clean, "/"), normalized, nil
}

func (s *Scanner) inspectCandidates(root string) ([]Candidate, int, []Diagnostic, error) {
	clusterFiles := make([]string, 0)
	ignored := 0
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrUnsafeArchive
		}
		relative, _ := filepath.Rel(root, filePath)
		if isSystemMetadata(relative, entry.Name()) || isInternalAdminDirectory(entry) {
			if !entry.IsDir() {
				ignored++
			}
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && strings.EqualFold(entry.Name(), "cluster.ini") {
			clusterFiles = append(clusterFiles, filePath)
		}
		return nil
	})
	if err != nil {
		return nil, ignored, nil, err
	}
	sort.Strings(clusterFiles)
	candidates := make([]Candidate, 0, len(clusterFiles))
	for _, clusterPath := range clusterFiles {
		candidate, err := s.inspectCandidate(root, filepath.Dir(clusterPath), clusterPath, len(candidates)+1)
		if err != nil {
			return nil, ignored, nil, err
		}
		candidate.IgnoredSystemFiles = ignored
		candidates = append(candidates, candidate)
	}
	diagnostics := make([]Diagnostic, 0)
	if len(candidates) > 1 {
		diagnostics = append(diagnostics, Diagnostic{Code: "MULTIPLE_CLUSTERS", Severity: SeverityInfo, Message: "压缩包中检测到多个 DST 房间，可分别选择导入", AutoFix: true})
	}
	return candidates, ignored, diagnostics, nil
}

func (s *Scanner) inspectCandidate(archiveRoot, candidateRoot, clusterPath string, index int) (Candidate, error) {
	relative, err := filepath.Rel(archiveRoot, candidateRoot)
	if err != nil || !contained(archiveRoot, candidateRoot) {
		return Candidate{}, ErrUnsafeArchive
	}
	relative = filepath.ToSlash(relative)
	config, parseErr := ini.Load(clusterPath)
	candidate := Candidate{
		ID: fmt.Sprintf("candidate-%d", index), Root: relative, DirectoryName: suggestedDirectory(relative),
		Worlds: []WorldManifest{}, Mods: []ModReference{}, Diagnostics: []Diagnostic{}, Compatibility: "ready",
	}
	if relative != "." {
		candidate.Diagnostics = append(candidate.Diagnostics, Diagnostic{
			Code: "WRAPPED_CLUSTER_ROOT", Severity: SeverityInfo, Message: "存档位于压缩包子目录中，导入时会自动剥离外层目录", Path: relative, AutoFix: true,
		})
	}
	if parseErr != nil {
		candidate.Diagnostics = append(candidate.Diagnostics, Diagnostic{Code: "INVALID_CLUSTER_INI", Severity: SeverityError, Message: "cluster.ini 无法解析", Path: filepath.ToSlash(strings.TrimPrefix(clusterPath, archiveRoot+string(os.PathSeparator)))})
		candidate.Compatibility = "blocked"
		return candidate, nil
	}
	candidate.Name = strings.TrimSpace(config.Section("NETWORK").Key("cluster_name").String())
	if candidate.Name == "" {
		candidate.Name = candidate.DirectoryName
	}
	candidate.Description = strings.TrimSpace(config.Section("NETWORK").Key("cluster_description").String())
	candidate.GameMode = config.Section("GAMEPLAY").Key("game_mode").MustString("survival")
	candidate.MaxPlayers = config.Section("GAMEPLAY").Key("max_players").MustInt(6)
	candidate.PvP = config.Section("GAMEPLAY").Key("pvp").MustBool(false)
	candidate.TokenPresent = findFileFold(candidateRoot, "cluster_token.txt") != ""
	if !candidate.TokenPresent {
		candidate.Diagnostics = append(candidate.Diagnostics, Diagnostic{Code: "CLUSTER_TOKEN_MISSING", Severity: SeverityWarning, Message: "存档未包含 cluster_token.txt，部署前需要选择 Token 策略"})
	}

	entries, err := os.ReadDir(candidateRoot)
	if err != nil {
		return Candidate{}, err
	}
	modWorlds := make(map[string]map[string]bool)
	masterCount := 0
	portOwners := make(map[int]string)
	for _, entry := range entries {
		if !entry.IsDir() || isSystemMetadata(entry.Name(), entry.Name()) {
			continue
		}
		worldRoot := filepath.Join(candidateRoot, entry.Name())
		serverPath := findFileFold(worldRoot, "server.ini")
		if serverPath == "" {
			continue
		}
		world, diagnostics := inspectWorld(candidateRoot, worldRoot, serverPath)
		candidate.Diagnostics = append(candidate.Diagnostics, diagnostics...)
		if world.IsMaster {
			masterCount++
		}
		for _, port := range []int{world.Ports.Server, world.Ports.Authentication, world.Ports.MasterServer} {
			if port <= 0 {
				continue
			}
			if owner, exists := portOwners[port]; exists {
				candidate.Diagnostics = append(candidate.Diagnostics, Diagnostic{
					Code: "DUPLICATE_PORT", Severity: SeverityWarning, Message: "多个分片使用了相同端口，自动端口策略可修复", AutoFix: true,
					Details: map[string]interface{}{"port": port, "worlds": []string{owner, world.DirectoryName}},
				})
			} else {
				portOwners[port] = world.DirectoryName
			}
		}
		modPath := findFileFold(worldRoot, "modoverrides.lua")
		world.ModFile = modPath != ""
		if modPath != "" {
			content, readErr := os.ReadFile(modPath)
			if readErr != nil {
				return Candidate{}, readErr
			}
			for _, match := range workshopReferencePattern.FindAllStringSubmatch(string(content), -1) {
				if modWorlds[match[1]] == nil {
					modWorlds[match[1]] = make(map[string]bool)
				}
				modWorlds[match[1]][world.DirectoryName] = true
			}
		}
		candidate.Worlds = append(candidate.Worlds, world)
	}
	sort.Slice(candidate.Worlds, func(i, j int) bool {
		if candidate.Worlds[i].IsMaster != candidate.Worlds[j].IsMaster {
			return candidate.Worlds[i].IsMaster
		}
		return strings.ToLower(candidate.Worlds[i].DirectoryName) < strings.ToLower(candidate.Worlds[j].DirectoryName)
	})
	if len(candidate.Worlds) == 0 {
		candidate.Diagnostics = append(candidate.Diagnostics, Diagnostic{Code: "NO_WORLDS", Severity: SeverityError, Message: "未找到包含 server.ini 的分片"})
	}
	if masterCount == 0 {
		candidate.Diagnostics = append(candidate.Diagnostics, Diagnostic{Code: "MASTER_MISSING", Severity: SeverityWarning, Message: "没有检测到主世界，只能使用高级分片导入"})
	}
	if masterCount > 1 {
		candidate.Diagnostics = append(candidate.Diagnostics, Diagnostic{Code: "MULTIPLE_MASTERS", Severity: SeverityError, Message: "检测到多个主世界，无法直接部署"})
	}
	modIDs := make([]string, 0, len(modWorlds))
	for id := range modWorlds {
		modIDs = append(modIDs, id)
	}
	sort.Strings(modIDs)
	missing := 0
	for _, id := range modIDs {
		worlds := make([]string, 0, len(modWorlds[id]))
		for world := range modWorlds[id] {
			worlds = append(worlds, world)
		}
		sort.Strings(worlds)
		downloaded := s.workshopRoot != "" && regularDirectory(filepath.Join(s.workshopRoot, id))
		if !downloaded {
			missing++
		}
		candidate.Mods = append(candidate.Mods, ModReference{ID: id, Worlds: worlds, Downloaded: downloaded})
	}
	if missing > 0 {
		candidate.Diagnostics = append(candidate.Diagnostics, Diagnostic{
			Code: "WORKSHOP_MODS_MISSING", Severity: SeverityWarning, Message: "部分 Workshop 模组尚未下载，模组配置会原样保留", Details: map[string]interface{}{"count": missing},
		})
	}
	for _, diagnostic := range candidate.Diagnostics {
		if diagnostic.Severity == SeverityError {
			candidate.Compatibility = "blocked"
			break
		}
		if diagnostic.Severity == SeverityWarning && candidate.Compatibility == "ready" {
			candidate.Compatibility = "needs_attention"
		}
	}
	return candidate, nil
}

func inspectWorld(candidateRoot, worldRoot, serverPath string) (WorldManifest, []Diagnostic) {
	worldName := filepath.Base(worldRoot)
	relative, _ := filepath.Rel(candidateRoot, serverPath)
	world := WorldManifest{DirectoryName: worldName, Name: worldName, Role: "custom", ServerFile: filepath.ToSlash(relative)}
	diagnostics := make([]Diagnostic, 0)
	config, err := ini.Load(serverPath)
	if err != nil {
		diagnostics = append(diagnostics, Diagnostic{Code: "INVALID_SERVER_INI", Severity: SeverityError, Message: "server.ini 无法解析", Path: world.ServerFile})
		return world, diagnostics
	}
	world.IsMaster = config.Section("SHARD").Key("is_master").MustBool(false)
	world.ShardID = config.Section("SHARD").Key("id").MustInt(0)
	world.Ports = PortManifest{
		Server:         config.Section("NETWORK").Key("server_port").MustInt(0),
		Authentication: config.Section("STEAM").Key("authentication_port").MustInt(0),
		MasterServer:   config.Section("STEAM").Key("master_server_port").MustInt(0),
	}
	if world.IsMaster {
		world.Role = "master"
	} else if strings.Contains(strings.ToLower(worldName), "cave") {
		world.Role = "caves"
	}
	sessionRoot := filepath.Join(worldRoot, "save", "session")
	if sessions, err := os.ReadDir(sessionRoot); err == nil {
		world.SessionCount = len(sessions)
	}
	if world.SessionCount == 0 {
		diagnostics = append(diagnostics, Diagnostic{Code: "WORLD_SESSION_MISSING", Severity: SeverityInfo, Message: "分片没有现有 session，将按新世界配置启动", Path: worldName})
	}
	return world, diagnostics
}

func suggestedDirectory(relative string) string {
	name := filepath.Base(filepath.FromSlash(relative))
	if relative == "." || name == "." || name == string(os.PathSeparator) {
		return "ImportedCluster"
	}
	var builder strings.Builder
	for _, character := range name {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			builder.WriteRune(character)
		}
	}
	value := strings.Trim(builder.String(), "_-")
	if value == "" {
		return "ImportedCluster"
	}
	if len(value) > 64 {
		value = value[:64]
	}
	return value
}

func isSystemMetadata(relative, name string) bool {
	clean := filepath.ToSlash(relative)
	return name == ".DS_Store" || strings.HasPrefix(clean, "__MACOSX/") || clean == "__MACOSX" || strings.HasPrefix(name, "._")
}

func isInternalAdminDirectory(entry os.DirEntry) bool {
	return entry.IsDir() && (entry.Name() == ".dst-admin-recovery" || entry.Name() == ".dst-admin-trash")
}

func findFileFold(directory, name string) string {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(entry.Name(), name) {
			return filepath.Join(directory, entry.Name())
		}
	}
	return ""
}

func regularFile(value string) bool {
	info, err := os.Lstat(value)
	return err == nil && info.Mode().IsRegular()
}

func regularDirectory(value string) bool {
	info, err := os.Lstat(value)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func contained(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			count, writeErr := destination.Write(buffer[:read])
			written += int64(count)
			if writeErr != nil {
				return written, writeErr
			}
			if count != read {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func fileSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func AnalysisErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrUnsafeArchive):
		return "UNSAFE_ARCHIVE"
	case errors.Is(err, ErrArchiveTooLarge):
		return "ARCHIVE_TOO_LARGE"
	case errors.Is(err, ErrInvalidArchive):
		return "INVALID_ARCHIVE"
	default:
		return "IMPORT_ANALYSIS_FAILED"
	}
}

func parseInt(value string) int {
	parsed, _ := strconv.Atoi(strings.TrimSpace(value))
	return parsed
}
