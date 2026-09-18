package runtimeperformance

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	dstinstall "dont/internal/dstserver"
	"dont/shared"
)

const maxMetadataBytes = 1024 * 1024

var (
	versionPattern    = regexp.MustCompile(`^[0-9]{1,64}$`)
	modVersionPattern = regexp.MustCompile(`(?m)^\s*version\s*=\s*["']([^"']+)["']`)
	binaryHashCache   sync.Map
)

type Options struct {
	ServerPath          string
	ServerMode          string
	WorkshopContentPath string
	Platform            string
	Architecture        string
}

type hashCacheKey struct {
	Path             string
	Size             int64
	ModifiedUnixNano int64
}

type nativeFiles struct {
	binDirectory   string
	executable     string
	original       string
	injector       string
	luaJIT         string
	arenaGC        string
	signature      string
	wrapperEnabled bool
	markers        int
}

type pluginFiles struct {
	marker       string
	markerValid  bool
	modRoot      string
	realInjector string
	coreVM       string
	luaJIT       string
	arenaGC      string
	signature    string
}

func Inspect(options Options) shared.RuntimePerformanceReport {
	options.ServerPath = filepath.Clean(strings.TrimSpace(options.ServerPath))
	options.ServerMode = strings.TrimSpace(options.ServerMode)
	options.WorkshopContentPath = filepath.Clean(strings.TrimSpace(options.WorkshopContentPath))
	options.Platform = strings.ToLower(strings.TrimSpace(options.Platform))
	options.Architecture = strings.ToLower(strings.TrimSpace(options.Architecture))
	if options.Platform == "" {
		options.Platform = runtime.GOOS
	}
	if options.Architecture == "" {
		options.Architecture = runtime.GOARCH
	}

	report := shared.RuntimePerformanceReport{
		Provider: "game", Status: shared.RuntimePerformanceNotInstalled,
		SupportedModes: []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame}, Issues: []string{},
	}
	files := locateNativeFiles(options)
	report.GameVersion = readGameVersion(options.ServerPath, files.binDirectory)
	plugins := locatePluginFiles(options, files)
	if plugins.marker != "" || plugins.realInjector != "" {
		return inspectPluginLayout(options, files, plugins)
	}
	if files.markers == 0 {
		return report
	}

	report.Provider = "dontstarve-luajit2"
	report.SignatureVersion = readSignatureVersion(files.signature)
	report.PackageVersion = readPackageVersion(options, files.binDirectory)

	hardFailure := false
	uncertain := false
	addIssue := func(code string, hard bool) {
		report.Issues = append(report.Issues, code)
		if hard {
			hardFailure = true
		} else {
			uncertain = true
		}
	}

	if options.ServerMode != "64" {
		addIssue("server_architecture_unsupported", true)
	}
	if options.Architecture != "amd64" && options.Architecture != "x86_64" {
		addIssue("architecture_unsupported", true)
	}
	if options.Platform != "linux" {
		addIssue("platform_not_verified", false)
	}
	// Older layouts are detected without claiming their launch interface is verified.
	addIssue("legacy_layout_unverified", false)

	for _, path := range []string{files.original, files.injector, files.luaJIT, files.signature} {
		if !regularFile(path) {
			addIssue("installation_incomplete", true)
			break
		}
	}
	if !files.wrapperEnabled {
		addIssue("injector_wrapper_invalid", true)
	}
	if report.SignatureVersion == "" {
		addIssue("signature_unreadable", true)
	}
	if report.GameVersion == "" {
		addIssue("game_version_unknown", false)
	} else if report.SignatureVersion != "" && report.GameVersion != report.SignatureVersion {
		addIssue("signature_version_mismatch", true)
	}
	if report.PackageVersion == "" {
		addIssue("package_version_unknown", false)
	}
	if regularFile(files.original) {
		if digest, err := cachedFileSHA256(files.original); err == nil {
			report.BinarySHA256 = digest
		} else {
			addIssue("binary_hash_unavailable", false)
		}
	}

	report.Issues = uniqueStrings(report.Issues)
	switch {
	case hardFailure:
		report.Status = shared.RuntimePerformanceIncompatible
	case uncertain:
		report.Status = shared.RuntimePerformanceDetectedUnverified
	default:
		report.Status = shared.RuntimePerformanceReady
		report.CanEnable = true
		report.SupportedModes = append(report.SupportedModes,
			shared.RuntimePerformanceModeLuaJIT,
		)
		if regularFile(files.arenaGC) {
			report.SupportedModes = append(report.SupportedModes, shared.RuntimePerformanceModeArenaGC)
		}
	}
	return report
}

func inspectPluginLayout(options Options, files nativeFiles, plugins pluginFiles) shared.RuntimePerformanceReport {
	report := shared.RuntimePerformanceReport{
		Provider: "dontstarve-luajit2", Status: shared.RuntimePerformanceDetectedUnverified,
		SupportedModes:      []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame},
		Issues:              []string{},
		GameVersion:         readGameVersion(options.ServerPath, files.binDirectory),
		PackageVersion:      readPackageVersionAtRoot(plugins.modRoot),
		SignatureVersion:    readSignatureVersion(plugins.signature),
		AutomaticSignatures: true,
	}
	hardFailure := false
	uncertain := false
	addIssue := func(code string, hard bool) {
		report.Issues = append(report.Issues, code)
		if hard {
			hardFailure = true
		} else {
			uncertain = true
		}
	}
	if options.ServerMode != "64" {
		addIssue("server_architecture_unsupported", true)
	}
	if options.Architecture != "amd64" && options.Architecture != "x86_64" {
		addIssue("architecture_unsupported", true)
	}
	if options.Platform != "linux" {
		addIssue("platform_not_verified", false)
	}
	if !plugins.markerValid {
		addIssue("injector_marker_invalid", true)
	}
	if !regularFile(files.original) || !regularFile(files.injector) || !regularFile(plugins.realInjector) ||
		!regularFile(plugins.coreVM) || !regularFile(plugins.luaJIT) {
		addIssue("installation_incomplete", true)
	}
	if !files.wrapperEnabled {
		addIssue("injector_wrapper_invalid", true)
	}
	if report.GameVersion == "" {
		addIssue("game_version_unknown", false)
	}
	if report.PackageVersion == "" {
		addIssue("package_version_unknown", false)
	}
	if regularFile(files.original) {
		if digest, err := cachedFileSHA256(files.original); err == nil {
			report.BinarySHA256 = digest
		} else {
			addIssue("binary_hash_unavailable", false)
		}
	}
	report.Issues = uniqueStrings(report.Issues)
	switch {
	case hardFailure:
		report.Status = shared.RuntimePerformanceIncompatible
	case uncertain:
		report.Status = shared.RuntimePerformanceDetectedUnverified
	default:
		report.Status = shared.RuntimePerformanceReady
		report.CanEnable = true
		report.SupportedModes = append(report.SupportedModes,
			shared.RuntimePerformanceModeLuaJIT,
		)
		if regularFile(plugins.arenaGC) {
			report.SupportedModes = append(report.SupportedModes, shared.RuntimePerformanceModeArenaGC)
		}
	}
	return report
}

func locateNativeFiles(options Options) nativeFiles {
	best := nativeFiles{}
	for _, directory := range candidateBinDirectories(options.ServerPath) {
		candidate := nativeFiles{binDirectory: directory}
		switch options.Platform {
		case "darwin":
			candidate.executable = firstExisting(filepath.Join(directory, dstinstall.Binary), filepath.Join(directory, dstinstall.BinaryX64))
			candidate.original = firstExisting(candidate.executable+"_1", filepath.Join(directory, dstinstall.Binary+"_1"), filepath.Join(directory, dstinstall.BinaryX64+"_1"))
			candidate.injector = firstExisting(filepath.Join(directory, "libInjector.dylib"), filepath.Join(filepath.Dir(directory), "Library", "libInjector.dylib"))
			candidate.luaJIT = firstExisting(filepath.Join(directory, "liblua51DS.dylib"), filepath.Join(filepath.Dir(directory), "Library", "liblua51DS.dylib"))
			candidate.arenaGC = firstExisting(filepath.Join(directory, "liblua51DS_gengc.dylib"), filepath.Join(filepath.Dir(directory), "Library", "liblua51DS_gengc.dylib"))
			candidate.signature = filepath.Join(directory, "signatures_server.json")
			candidate.wrapperEnabled = wrapperContains(candidate.executable, "DYLD_INSERT_LIBRARIES", "libInjector.dylib")
		case "windows":
			candidate.executable = firstExisting(filepath.Join(directory, dstinstall.BinaryX64+".exe"), filepath.Join(directory, dstinstall.Binary+".exe"))
			candidate.injector = firstExisting(filepath.Join(directory, "winmm.dll"), filepath.Join(directory, "Winmm.dll"))
			candidate.luaJIT = filepath.Join(directory, "liblua51DS.dll")
			candidate.arenaGC = filepath.Join(directory, "liblua51DS_gengc.dll")
			candidate.signature = filepath.Join(directory, "signatures_server.json")
			candidate.original = candidate.executable
			candidate.wrapperEnabled = regularFile(candidate.injector)
		default:
			candidate.executable = firstExisting(filepath.Join(directory, dstinstall.BinaryX64), filepath.Join(directory, dstinstall.Binary))
			candidate.original = firstExisting(candidate.executable+"_1", filepath.Join(directory, dstinstall.BinaryX64+"_1"), filepath.Join(directory, dstinstall.Binary+"_1"))
			candidate.injector = filepath.Join(directory, "lib64", "libInjector.so")
			candidate.luaJIT = filepath.Join(directory, "lib64", "liblua51DS.so")
			candidate.arenaGC = filepath.Join(directory, "lib64", "liblua51DS_gengc.so")
			candidate.signature = filepath.Join(directory, "signatures_server.json")
			candidate.wrapperEnabled = wrapperContains(candidate.executable, "LD_PRELOAD", "libInjector.so")
		}
		candidate.markers = markerCount(candidate)
		if options.Platform == "windows" && regularFile(candidate.original) {
			// Windows uses a proxy DLL and does not rename the game executable.
			// The unmodified executable alone is therefore not an installation marker.
			candidate.markers--
		}
		if candidate.markers > best.markers {
			best = candidate
		}
	}
	return best
}

func locatePluginFiles(options Options, files nativeFiles) pluginFiles {
	result := pluginFiles{}
	// The managed launcher sets DS_LUAJIT_INJECTOR explicitly. Upstream may
	// replace its shared marker during concurrent shard startup, so inspect
	// the launcher's actual destination before consulting that optional cache.
	if options.Platform == "linux" {
		if layout, ok := dstinstall.Resolve(files.executable, options.ServerMode); ok {
			expected, buildErr := ManagedLinuxLauncher(layout)
			actual, readErr := readLimited(files.executable, 64*1024)
			if buildErr == nil && readErr == nil && string(actual) == expected {
				result.realInjector = filepath.Join(layout.ContentRoot, "mods", "DontStarveLuaJIT2", "libInjector.so")
				result.markerValid = true
			}
		}
	}
	if result.realInjector == "" {
		for _, root := range candidateGameRoots(options.ServerPath, files.binDirectory) {
			marker := filepath.Join(root, "data", "unsafedata", "ds_luajit_injector.path")
			if regularFile(marker) {
				result.marker = marker
				break
			}
		}
		if result.marker == "" {
			return result
		}
		data, err := readLimited(result.marker, 4096)
		if err != nil {
			return result
		}
		value := strings.TrimSpace(string(data))
		if value == "" || strings.ContainsAny(value, "\x00\r\n") || !filepath.IsAbs(value) {
			return result
		}
		result.markerValid = true
		result.realInjector = filepath.Clean(value)
	}
	result.modRoot = filepath.Dir(result.realInjector)
	result.signature = filepath.Join(result.modRoot, "signatures_server.json")
	switch options.Platform {
	case "darwin":
		result.coreVM = filepath.Join(result.modRoot, "plugins", "plugin_core_vm", "plugin_core_vm.dylib")
		result.luaJIT = filepath.Join(result.modRoot, "deps", "liblua51DS.dylib")
		result.arenaGC = filepath.Join(result.modRoot, "deps", "liblua51DS_gengc.dylib")
	case "windows":
		result.coreVM = filepath.Join(result.modRoot, "plugins", "plugin_core_vm", "plugin_core_vm.dll")
		result.luaJIT = filepath.Join(result.modRoot, "deps", "lua51DS.dll")
		result.arenaGC = filepath.Join(result.modRoot, "deps", "lua51DS_gengc.dll")
	default:
		result.coreVM = filepath.Join(result.modRoot, "plugins", "plugin_core_vm", "plugin_core_vm.so")
		result.luaJIT = filepath.Join(result.modRoot, "deps", "liblua51DS.so")
		result.arenaGC = filepath.Join(result.modRoot, "deps", "liblua51DS_gengc.so")
	}
	return result
}

func candidateGameRoots(serverPath, binDirectory string) []string {
	values := []string{}
	serverPath = filepath.Clean(strings.TrimSpace(serverPath))
	if info, err := os.Stat(serverPath); err == nil && !info.IsDir() {
		serverPath = filepath.Dir(serverPath)
	}
	values = append(values, serverPath)
	for current, depth := filepath.Clean(binDirectory), 0; current != "" && current != "." && depth < 4; depth++ {
		values = append(values, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return uniquePaths(values)
}

func candidateBinDirectories(serverPath string) []string {
	serverPath = filepath.Clean(strings.TrimSpace(serverPath))
	if serverPath == "" || serverPath == "." {
		return nil
	}
	values := []string{}
	if layout, ok := dstinstall.Resolve(serverPath, "64"); ok {
		values = append(values, layout.WorkingDirectory)
	}
	if info, err := os.Stat(serverPath); err == nil && !info.IsDir() {
		serverPath = filepath.Dir(serverPath)
	}
	values = append(values,
		serverPath,
		filepath.Join(serverPath, "bin64"),
		filepath.Join(serverPath, "bin"),
		filepath.Join(serverPath, "Contents", "MacOS"),
		filepath.Join(serverPath, "dontstarve_steam.app", "Contents", "MacOS"),
		filepath.Join(serverPath, "dontstarve_dedicated_server_nullrenderer.app", "Contents", "MacOS"),
	)
	return uniquePaths(values)
}

func markerCount(files nativeFiles) int {
	count := 0
	for _, path := range []string{files.original, files.injector, files.luaJIT, files.arenaGC, files.signature} {
		if regularFile(path) {
			count++
		}
	}
	if files.wrapperEnabled {
		count++
	}
	return count
}

func wrapperContains(path string, expected ...string) bool {
	data, err := readLimited(path, 64*1024)
	if err != nil {
		return false
	}
	value := string(data)
	for _, part := range expected {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}

func readGameVersion(serverPath, binDirectory string) string {
	candidates := []string{filepath.Join(serverPath, "version.txt")}
	for current, depth := filepath.Clean(binDirectory), 0; current != "" && current != "." && depth < 5; depth++ {
		candidates = append(candidates, filepath.Join(current, "version.txt"))
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	for _, path := range uniquePaths(candidates) {
		data, err := readLimited(path, 4096)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		if scanner.Scan() {
			value := strings.TrimSpace(scanner.Text())
			if versionPattern.MatchString(value) {
				return value
			}
		}
	}
	return ""
}

func readSignatureVersion(path string) string {
	data, err := readLimited(path, maxMetadataBytes)
	if err != nil {
		return ""
	}
	var payload struct {
		Version json.RawMessage `json:"version"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return ""
	}
	value := strings.Trim(strings.TrimSpace(string(payload.Version)), `"`)
	if versionPattern.MatchString(value) {
		return value
	}
	return ""
}

func readPackageVersion(options Options, binDirectory string) string {
	roots := []string{}
	for current, depth := filepath.Clean(binDirectory), 0; current != "" && current != "." && depth < 5; depth++ {
		roots = append(roots, filepath.Join(current, "mods"))
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	if options.WorkshopContentPath != "" && options.WorkshopContentPath != "." {
		roots = append(roots, options.WorkshopContentPath)
	}
	for _, root := range uniquePaths(roots) {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		if len(entries) > 256 {
			entries = entries[:256]
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			directory := filepath.Join(root, entry.Name())
			if version := readPackageVersionAtRoot(directory); version != "" {
				return version
			}
		}
	}
	return ""
}

func readPackageVersionAtRoot(directory string) string {
	if !regularFile(filepath.Join(directory, "inject_server_only_mod.lua")) {
		return ""
	}
	data, err := readLimited(filepath.Join(directory, "modinfo.lua"), maxMetadataBytes)
	if err != nil {
		return ""
	}
	if match := modVersionPattern.FindSubmatch(data); len(match) == 2 {
		value := strings.TrimSpace(string(match[1]))
		if value != "" && utf8.RuneCountInString(value) <= 64 && !strings.ContainsAny(value, "\x00\r\n") {
			return value
		}
	}
	return ""
}

func cachedFileSHA256(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	key := hashCacheKey{Path: filepath.Clean(path), Size: info.Size(), ModifiedUnixNano: info.ModTime().UnixNano()}
	if cached, ok := binaryHashCache.Load(key); ok {
		return cached.(string), nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	value := hex.EncodeToString(digest.Sum(nil))
	binaryHashCache.Store(key, value)
	return value, nil
}

func readLimited(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, maximum))
}

func regularFile(path string) bool {
	if strings.TrimSpace(path) == "" || path == "." {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func firstExisting(values ...string) string {
	for _, value := range values {
		if regularFile(value) {
			return filepath.Clean(value)
		}
	}
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return filepath.Clean(value)
		}
	}
	return ""
}

func uniquePaths(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = filepath.Clean(strings.TrimSpace(value))
		if value == "" || value == "." || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
