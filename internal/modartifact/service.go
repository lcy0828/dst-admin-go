package modartifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"strings"
	"sync"
	"time"

	"dont/internal/moddistribution"
	"dont/shared"

	"github.com/shirou/gopsutil/v3/disk"
)

var (
	ErrInvalidInput = errors.New("mod artifact request is invalid")
	ErrUnauthorized = errors.New("mod artifact download grant is invalid")
)

var (
	workshopIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	sha256Pattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

const (
	MaxGrantTTL               = 30 * time.Minute
	DefaultBundleRetention    = 7 * 24 * time.Hour
	DefaultBundleCacheBytes   = int64(4 << 30)
	MinimumArtifactHeadroom   = uint64(256 << 20)
	temporaryArtifactLifetime = time.Hour
)

type Descriptor struct {
	WorkshopID string    `json:"workshopId"`
	TreeSHA256 string    `json:"treeSha256"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	CreatedAt  time.Time `json:"createdAt"`
}

type grant struct {
	Subject    string
	WorkshopID string
	TreeSHA256 string
	ExpiresAt  time.Time
}

type Service struct {
	manager    *moddistribution.Manager
	root       string
	now        func() time.Time
	buildMu    sync.Mutex
	mu         sync.Mutex
	grants     map[string]grant
	diskUsage  func(string) (*disk.UsageStat, error)
	retention  time.Duration
	cacheLimit int64
}

func NewService(manager *moddistribution.Manager, root string) (*Service, error) {
	root = strings.TrimSpace(root)
	if manager == nil || root == "" {
		return nil, ErrInvalidInput
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.Join(ErrInvalidInput, err)
	}
	if err := os.Chmod(resolved, 0o700); err != nil {
		return nil, err
	}
	return &Service{
		manager: manager, root: filepath.Clean(resolved), now: time.Now, grants: make(map[string]grant),
		diskUsage: disk.Usage, retention: DefaultBundleRetention, cacheLimit: DefaultBundleCacheBytes,
	}, nil
}

func (s *Service) Issue(ctx context.Context, subject, workshopID, treeSHA string, ttl time.Duration) (shared.RuntimeModFetchLocation, string, error) {
	if s == nil || strings.TrimSpace(subject) == "" || !validArtifact(workshopID, treeSHA) || ttl < time.Minute || ttl > MaxGrantTTL {
		return shared.RuntimeModFetchLocation{}, "", ErrInvalidInput
	}
	descriptor, err := s.materialize(ctx, workshopID, strings.ToLower(treeSHA))
	if err != nil {
		return shared.RuntimeModFetchLocation{}, "", err
	}
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return shared.RuntimeModFetchLocation{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(data)
	s.mu.Lock()
	s.pruneGrantsLocked()
	s.grants[token] = grant{
		Subject: strings.TrimSpace(subject), WorkshopID: workshopID, TreeSHA256: descriptor.TreeSHA256,
		ExpiresAt: s.now().UTC().Add(ttl),
	}
	s.mu.Unlock()
	return shared.RuntimeModFetchLocation{
		Source:        shared.RuntimeModFetchSourceController,
		DownloadPath:  "/mod-artifacts/" + workshopID + "/" + descriptor.TreeSHA256,
		DownloadToken: token, Size: descriptor.Size, SHA256: descriptor.SHA256,
	}, token, nil
}

func (s *Service) Revoke(token string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.grants, strings.TrimSpace(token))
	s.mu.Unlock()
}

func (s *Service) Open(workshopID, treeSHA, token string) (Descriptor, *os.File, error) {
	if s == nil || !validArtifact(workshopID, treeSHA) {
		return Descriptor{}, nil, ErrInvalidInput
	}
	treeSHA, token = strings.ToLower(treeSHA), strings.TrimSpace(token)
	s.mu.Lock()
	s.pruneGrantsLocked()
	permission, exists := s.grants[token]
	s.mu.Unlock()
	if !exists || permission.WorkshopID != workshopID || permission.TreeSHA256 != treeSHA {
		return Descriptor{}, nil, ErrUnauthorized
	}
	descriptor, err := s.readDescriptor(workshopID, treeSHA)
	if err != nil {
		return Descriptor{}, nil, err
	}
	file, err := os.Open(s.bundlePath(workshopID, treeSHA))
	if err != nil {
		return Descriptor{}, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != descriptor.Size {
		file.Close()
		return Descriptor{}, nil, errors.Join(moddistribution.ErrIntegrity, err)
	}
	return descriptor, file, nil
}

func (s *Service) materialize(ctx context.Context, workshopID, treeSHA string) (Descriptor, error) {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	if descriptor, err := s.readDescriptor(workshopID, treeSHA); err == nil {
		if info, statErr := os.Stat(s.bundlePath(workshopID, treeSHA)); statErr == nil && info.Mode().IsRegular() && info.Size() == descriptor.Size {
			return descriptor, nil
		}
	}
	manifest, err := s.manager.Verify(ctx, workshopID, treeSHA)
	if err != nil {
		return Descriptor{}, err
	}
	_ = os.Remove(s.bundlePath(workshopID, treeSHA))
	_ = os.Remove(s.descriptorPath(workshopID, treeSHA))
	if err := s.pruneBundlesLocked(bundleSpaceEstimate(manifest), workshopID+"\x00"+treeSHA); err != nil {
		return Descriptor{}, err
	}
	directory := filepath.Join(s.root, workshopID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Descriptor{}, err
	}
	temporary, err := os.CreateTemp(directory, ".bundle-*.tar")
	if err != nil {
		return Descriptor{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	digest := sha256.New()
	writeErr := s.manager.WriteBundle(ctx, workshopID, treeSHA, io.MultiWriter(temporary, digest))
	if writeErr == nil {
		writeErr = temporary.Sync()
	}
	info, statErr := temporary.Stat()
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, statErr, closeErr); err != nil {
		return Descriptor{}, err
	}
	if info.Size() < 1 {
		return Descriptor{}, moddistribution.ErrIntegrity
	}
	if err := os.Chmod(temporaryPath, 0o400); err != nil {
		return Descriptor{}, err
	}
	descriptor := Descriptor{
		WorkshopID: workshopID, TreeSHA256: treeSHA, Size: info.Size(),
		SHA256: hex.EncodeToString(digest.Sum(nil)), CreatedAt: s.now().UTC(),
	}
	if err := os.Rename(temporaryPath, s.bundlePath(workshopID, treeSHA)); err != nil {
		return Descriptor{}, err
	}
	if err := syncDirectory(directory); err != nil {
		_ = os.Remove(s.bundlePath(workshopID, treeSHA))
		return Descriptor{}, err
	}
	if err := writeJSONAtomic(s.descriptorPath(workshopID, treeSHA), descriptor); err != nil {
		_ = os.Remove(s.bundlePath(workshopID, treeSHA))
		return Descriptor{}, err
	}
	return descriptor, nil
}

type bundleCandidate struct {
	workshopID string
	treeSHA    string
	path       string
	size       int64
	modifiedAt time.Time
}

func (s *Service) pruneBundlesLocked(required int64, preserve string) error {
	if required < 0 || s.diskUsage == nil {
		return ErrInvalidInput
	}
	active := s.activeArtifacts()
	now := s.now().UTC()
	candidates := make([]bundleCandidate, 0)
	total := int64(0)
	workshops, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, workshop := range workshops {
		if !workshop.IsDir() || !workshopIDPattern.MatchString(workshop.Name()) {
			continue
		}
		directory := filepath.Join(s.root, workshop.Name())
		entries, readErr := os.ReadDir(directory)
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			name := entry.Name()
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			if strings.HasPrefix(name, ".bundle-") {
				if !info.IsDir() && now.Sub(info.ModTime()) >= temporaryArtifactLifetime {
					_ = os.Remove(filepath.Join(directory, name))
				}
				continue
			}
			if entry.IsDir() || filepath.Ext(name) != ".tar" {
				continue
			}
			treeSHA := strings.TrimSuffix(name, ".tar")
			if !sha256Pattern.MatchString(treeSHA) || info.Size() < 1 {
				continue
			}
			candidate := bundleCandidate{
				workshopID: workshop.Name(), treeSHA: treeSHA, path: filepath.Join(directory, name),
				size: info.Size(), modifiedAt: info.ModTime(),
			}
			candidates = append(candidates, candidate)
			total += info.Size()
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].modifiedAt.Before(candidates[j].modifiedAt) })
	usage, err := s.diskUsage(s.root)
	if err != nil {
		return fmt.Errorf("inspect Mod artifact disk space: %w", err)
	}
	effectiveFree := usage.Free
	for _, candidate := range candidates {
		key := candidate.workshopID + "\x00" + candidate.treeSHA
		expired := s.retention > 0 && now.Sub(candidate.modifiedAt) >= s.retention
		overLimit := s.cacheLimit > 0 && total+required > s.cacheLimit
		insufficient := uint64(required) > effectiveFree || MinimumArtifactHeadroom > effectiveFree-uint64(required)
		if key == preserve || active[key] || !expired && !overLimit && !insufficient {
			continue
		}
		if err := removeBundlePair(candidate); err != nil {
			continue
		}
		total -= candidate.size
		effectiveFree += uint64(candidate.size)
	}
	if s.cacheLimit > 0 && total+required > s.cacheLimit {
		return fmt.Errorf("%w: Controller Mod artifact cache limit would be exceeded", moddistribution.ErrInsufficientSpace)
	}
	if uint64(required) > effectiveFree || MinimumArtifactHeadroom > effectiveFree-uint64(required) {
		return fmt.Errorf("%w: Controller needs %d bytes plus %d bytes headroom", moddistribution.ErrInsufficientSpace, required, MinimumArtifactHeadroom)
	}
	return nil
}

func (s *Service) activeArtifacts() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneGrantsLocked()
	result := make(map[string]bool, len(s.grants))
	for _, permission := range s.grants {
		result[permission.WorkshopID+"\x00"+permission.TreeSHA256] = true
	}
	return result
}

func bundleSpaceEstimate(manifest moddistribution.Manifest) int64 {
	estimate := manifest.Size + int64(manifest.FileCount+1)*1024 + 1<<20
	if estimate < 1<<20 {
		return 1 << 20
	}
	return estimate
}

func removeBundlePair(candidate bundleCandidate) error {
	if err := os.Remove(candidate.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	descriptor := filepath.Join(filepath.Dir(candidate.path), candidate.treeSHA+".json")
	if err := os.Remove(descriptor); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(filepath.Dir(candidate.path))
}

func (s *Service) readDescriptor(workshopID, treeSHA string) (Descriptor, error) {
	file, err := os.Open(s.descriptorPath(workshopID, treeSHA))
	if err != nil {
		return Descriptor{}, err
	}
	defer file.Close()
	var descriptor Descriptor
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024))
	if err := decoder.Decode(&descriptor); err != nil {
		return Descriptor{}, err
	}
	if descriptor.WorkshopID != workshopID || descriptor.TreeSHA256 != treeSHA || descriptor.Size < 1 || !sha256Pattern.MatchString(descriptor.SHA256) {
		return Descriptor{}, moddistribution.ErrIntegrity
	}
	return descriptor, nil
}

func (s *Service) pruneGrantsLocked() {
	now := s.now().UTC()
	for token, permission := range s.grants {
		if !permission.ExpiresAt.After(now) {
			delete(s.grants, token)
		}
	}
}

func (s *Service) bundlePath(workshopID, treeSHA string) string {
	return filepath.Join(s.root, workshopID, treeSHA+".tar")
}

func (s *Service) descriptorPath(workshopID, treeSHA string) string {
	return filepath.Join(s.root, workshopID, treeSHA+".json")
}

func validArtifact(workshopID, treeSHA string) bool {
	return workshopIDPattern.MatchString(workshopID) && sha256Pattern.MatchString(strings.ToLower(strings.TrimSpace(treeSHA)))
}

func writeJSONAtomic(path string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".descriptor-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish Mod artifact descriptor: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
