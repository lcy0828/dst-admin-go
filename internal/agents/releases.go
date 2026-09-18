package agents

import (
	"crypto/rand"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/tempfiles"

	"github.com/google/uuid"
)

const MaxAgentReleaseBytes int64 = 128 * 1024 * 1024

var agentReleaseVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z._+-]{0,63}$`)

var (
	ErrReleaseNotFound = errors.New("agent release not found")
	ErrReleaseInUse    = errors.New("agent release download is active")
	ErrInvalidRelease  = errors.New("agent release is invalid")
	ErrReleaseTooLarge = errors.New("agent release exceeds the upload limit")
	ErrDownloadToken   = errors.New("agent release download token is invalid")
)

type AgentRelease struct {
	ID         string    `json:"id"`
	Version    string    `json:"version"`
	OS         string    `json:"os"`
	Arch       string    `json:"arch"`
	FileName   string    `json:"fileName"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	UploadedAt time.Time `json:"uploadedAt"`
}

type releaseDownloadGrant struct {
	AgentID   string
	ReleaseID string
	ExpiresAt time.Time
}

type ReleaseStore struct {
	root   string
	now    func() time.Time
	mu     sync.Mutex
	grants map[string]releaseDownloadGrant
}

func DefaultAgentReleaseDir() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("DST_ADMIN_AGENT_RELEASE_DIR")); configured != "" {
		return filepath.Abs(configured)
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(root, "dst-admin", "agent-releases"), nil
}

func NewReleaseStore(root string) (*ReleaseStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("agent release directory is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve agent release directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create agent release directory: %w", err)
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("secure agent release directory: %w", err)
	}
	if err := tempfiles.Cleanup(absolute, tempfiles.Uploads); err != nil {
		return nil, fmt.Errorf("recover agent uploads: %w", err)
	}
	return &ReleaseStore{root: absolute, now: time.Now, grants: make(map[string]releaseDownloadGrant)}, nil
}

func (s *ReleaseStore) Save(version, fileName string, source io.Reader) (AgentRelease, error) {
	version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(version), "v"))
	fileName = filepath.Base(strings.TrimSpace(fileName))
	if s == nil || !agentReleaseVersionPattern.MatchString(version) || fileName == "." || fileName == "" || source == nil {
		return AgentRelease{}, ErrInvalidRelease
	}

	id := uuid.NewString()
	temporary, err := os.CreateTemp(s.root, ".upload-*")
	if err != nil {
		return AgentRelease{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, digest), io.LimitReader(source, MaxAgentReleaseBytes+1))
	if copyErr == nil {
		copyErr = temporary.Sync()
	}
	closeErr := temporary.Close()
	if copyErr != nil {
		return AgentRelease{}, copyErr
	}
	if closeErr != nil {
		return AgentRelease{}, closeErr
	}
	if written > MaxAgentReleaseBytes {
		return AgentRelease{}, ErrReleaseTooLarge
	}
	if written < 1 {
		return AgentRelease{}, ErrInvalidRelease
	}
	info, err := buildinfo.ReadFile(temporaryPath)
	if err != nil || info == nil || info.Path != "dont/agent/cmd/agent" {
		return AgentRelease{}, fmt.Errorf("%w: uploaded file is not a DST Admin Agent Go binary", ErrInvalidRelease)
	}
	goos, goarch := buildSetting(info, "GOOS"), buildSetting(info, "GOARCH")
	if !supportedAgentReleasePlatform(goos, goarch) {
		return AgentRelease{}, fmt.Errorf("%w: unsupported Agent platform %s/%s", ErrInvalidRelease, goos, goarch)
	}

	release := AgentRelease{
		ID: id, Version: version, OS: goos, Arch: goarch, FileName: fileName,
		Size: written, SHA256: hex.EncodeToString(digest.Sum(nil)), UploadedAt: s.now().UTC(),
	}
	binaryPath := s.binaryPath(id)
	if err := os.Chmod(temporaryPath, 0o700); err != nil {
		return AgentRelease{}, err
	}
	if err := os.Rename(temporaryPath, binaryPath); err != nil {
		return AgentRelease{}, err
	}
	if err := s.writeMetadata(release); err != nil {
		_ = os.Remove(binaryPath)
		return AgentRelease{}, err
	}
	return release, nil
}

func (s *ReleaseStore) List() ([]AgentRelease, error) {
	if s == nil {
		return nil, errors.New("agent release store is unavailable")
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	items := make([]AgentRelease, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		value, readErr := s.Get(id)
		if readErr == nil {
			items = append(items, value)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if compared := compareAgentVersions(items[i].Version, items[j].Version); compared != 0 {
			return compared > 0
		}
		return items[i].UploadedAt.After(items[j].UploadedAt)
	})
	return items, nil
}

func (s *ReleaseStore) Get(id string) (AgentRelease, error) {
	if s == nil || !validReleaseID(id) {
		return AgentRelease{}, ErrReleaseNotFound
	}
	data, err := os.ReadFile(s.metadataPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return AgentRelease{}, ErrReleaseNotFound
	}
	if err != nil {
		return AgentRelease{}, err
	}
	var release AgentRelease
	if err := json.Unmarshal(data, &release); err != nil || release.ID != id || !validReleaseMetadata(release) {
		return AgentRelease{}, ErrInvalidRelease
	}
	info, err := os.Stat(s.binaryPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return AgentRelease{}, ErrReleaseNotFound
	}
	if err != nil {
		return AgentRelease{}, err
	}
	if !info.Mode().IsRegular() || info.Size() != release.Size {
		return AgentRelease{}, ErrInvalidRelease
	}
	return release, nil
}

func (s *ReleaseStore) Latest(goos, goarch string) (*AgentRelease, error) {
	items, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.OS == strings.ToLower(strings.TrimSpace(goos)) && item.Arch == strings.ToLower(strings.TrimSpace(goarch)) {
			copy := item
			return &copy, nil
		}
	}
	return nil, nil
}

func (s *ReleaseStore) Delete(id string) error {
	if _, err := s.Get(id); err != nil {
		return err
	}
	s.mu.Lock()
	s.pruneGrantsLocked()
	for _, grant := range s.grants {
		if grant.ReleaseID == id {
			s.mu.Unlock()
			return ErrReleaseInUse
		}
	}
	s.mu.Unlock()
	if err := os.Remove(s.metadataPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(s.binaryPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *ReleaseStore) IssueDownloadToken(agentID, releaseID string, ttl time.Duration) (string, error) {
	if _, err := s.Get(releaseID); err != nil {
		return "", err
	}
	if !agentIDPattern.MatchString(agentID) || ttl < time.Minute || ttl > 30*time.Minute {
		return "", ErrInvalidInput
	}
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneGrantsLocked()
	s.grants[token] = releaseDownloadGrant{AgentID: agentID, ReleaseID: releaseID, ExpiresAt: s.now().UTC().Add(ttl)}
	return token, nil
}

func (s *ReleaseStore) OpenDownload(releaseID, token string) (AgentRelease, *os.File, error) {
	token = strings.TrimSpace(token)
	s.mu.Lock()
	s.pruneGrantsLocked()
	grant, exists := s.grants[token]
	s.mu.Unlock()
	if !exists || grant.ReleaseID != releaseID {
		return AgentRelease{}, nil, ErrDownloadToken
	}
	release, err := s.Get(releaseID)
	if err != nil {
		return AgentRelease{}, nil, err
	}
	file, err := os.Open(s.binaryPath(releaseID))
	if err != nil {
		return AgentRelease{}, nil, err
	}
	return release, file, nil
}

func (s *ReleaseStore) RevokeDownloadToken(token string) {
	s.mu.Lock()
	delete(s.grants, strings.TrimSpace(token))
	s.mu.Unlock()
}

func (s *ReleaseStore) writeMetadata(release AgentRelease) error {
	data, err := json.MarshalIndent(release, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.root, ".metadata-*")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer os.Remove(path)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(path, s.metadataPath(release.ID))
}

func (s *ReleaseStore) pruneGrantsLocked() {
	now := s.now().UTC()
	for token, grant := range s.grants {
		if !grant.ExpiresAt.After(now) {
			delete(s.grants, token)
		}
	}
}

func (s *ReleaseStore) binaryPath(id string) string   { return filepath.Join(s.root, id+".bin") }
func (s *ReleaseStore) metadataPath(id string) string { return filepath.Join(s.root, id+".json") }

func buildSetting(info *buildinfo.BuildInfo, key string) string {
	for _, setting := range info.Settings {
		if setting.Key == key {
			return strings.ToLower(strings.TrimSpace(setting.Value))
		}
	}
	return ""
}

func supportedAgentReleasePlatform(goos, goarch string) bool {
	if goos != "linux" && goos != "darwin" && goos != "windows" {
		return false
	}
	switch goarch {
	case "amd64", "arm64":
		return true
	default:
		return false
	}
}

func validReleaseID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil
}

func validReleaseMetadata(value AgentRelease) bool {
	return validReleaseID(value.ID) && agentReleaseVersionPattern.MatchString(value.Version) &&
		supportedAgentReleasePlatform(value.OS, value.Arch) && value.FileName != "" && filepath.Base(value.FileName) == value.FileName &&
		value.Size > 0 && value.Size <= MaxAgentReleaseBytes && runtimePerformanceSHA256Pattern.MatchString(value.SHA256) && !value.UploadedAt.IsZero()
}

func compareAgentVersions(left, right string) int {
	leftMain, leftPrerelease := splitAgentVersion(left)
	rightMain, rightPrerelease := splitAgentVersion(right)
	count := len(leftMain)
	if len(rightMain) > count {
		count = len(rightMain)
	}
	for index := 0; index < count; index++ {
		leftValue, rightValue := 0, 0
		if index < len(leftMain) {
			leftValue, _ = strconv.Atoi(leftMain[index])
		}
		if index < len(rightMain) {
			rightValue, _ = strconv.Atoi(rightMain[index])
		}
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	if leftPrerelease == rightPrerelease {
		return 0
	}
	if leftPrerelease == "" {
		return 1
	}
	if rightPrerelease == "" {
		return -1
	}
	return strings.Compare(leftPrerelease, rightPrerelease)
}

func splitAgentVersion(value string) ([]string, string) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	main, prerelease, _ := strings.Cut(value, "-")
	parts := strings.Split(main, ".")
	for index, part := range parts {
		if _, err := strconv.Atoi(part); err != nil {
			parts[index] = "0"
		}
	}
	return parts, prerelease
}

func (s *Service) UploadDirectory() string {
	if s.releases == nil {
		return ""
	}
	return s.releases.root
}
