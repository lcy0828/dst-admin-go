package luajit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/shared"
)

type grant struct {
	ID      string
	Expires time.Time
}
type Store struct {
	catalogMu sync.Mutex
	seedMu    sync.Mutex
	root      string
	mu        sync.Mutex
	grants    map[string]grant
}

func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("LuaJIT 发布目录不能为空")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &Store{root: root, grants: map[string]grant{}}, nil
}
func (s *Store) Save(ctx context.Context, source io.Reader, expected string) (shared.LuaJITRelease, error) {
	var release shared.LuaJITRelease
	temp, err := os.CreateTemp(s.root, ".download-*")
	if err != nil {
		return release, err
	}
	defer os.Remove(temp.Name())
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(source, MaxPackageBytes+1))
	closeErr := temp.Close()
	if err != nil {
		return release, err
	}
	if closeErr != nil {
		return release, closeErr
	}
	if n <= 0 || n > MaxPackageBytes {
		return release, ErrInvalidPackage
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if expected != "" && !strings.EqualFold(digest, expected) {
		return release, errors.New("LuaJIT 安装包 SHA-256 校验失败")
	}
	stage, err := os.MkdirTemp(s.root, ".validate-*")
	if err != nil {
		return release, err
	}
	defer os.RemoveAll(stage)
	version, err := unpack(ctx, temp.Name(), stage)
	if err != nil {
		return release, err
	}
	release = shared.LuaJITRelease{ID: digest, Version: version, OS: "linux", Arch: "amd64", SHA256: digest, Size: n}
	if data, e := os.ReadFile(filepath.Join(stage, "dst_admin_build.json")); e == nil && len(data) < 4096 {
		var provenance struct {
			Revision     string `json:"revision"`
			Distribution string `json:"distribution"`
		}
		if json.Unmarshal(data, &provenance) == nil && len(provenance.Revision) <= 64 {
			release.Revision = provenance.Revision
			if provenance.Distribution == "compatibility" {
				release.Channel = "compatibility"
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = os.Rename(temp.Name(), filepath.Join(s.root, digest+".zip")); err != nil {
		return release, err
	}
	data, _ := json.MarshalIndent(release, "", "  ")
	if err = os.WriteFile(filepath.Join(s.root, digest+".json.tmp"), data, 0600); err != nil {
		return release, err
	}
	err = os.Rename(filepath.Join(s.root, digest+".json.tmp"), filepath.Join(s.root, digest+".json"))
	return release, err
}
func ValidateSource(rawURL, expected string) error {
	u, err := url.Parse(rawURL)
	if len(rawURL) > 4096 || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || !packageID.MatchString(strings.ToLower(expected)) {
		return errors.New("请输入 HTTPS 安装包地址及发布者提供的 SHA-256")
	}
	return nil
}
func (s *Store) ImportURL(ctx context.Context, rawURL, expected string) (shared.LuaJITRelease, error) {
	if err := ValidateSource(rawURL, expected); err != nil {
		return shared.LuaJITRelease{}, err
	}
	emit(ctx, "download", 5, "运行节点正在下载 LuaJIT 安装包")
	response, err := fetchPublicSource(ctx, rawURL, publicSourceClient(10*time.Minute))
	if err != nil {
		return shared.LuaJITRelease{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return shared.LuaJITRelease{}, fmt.Errorf("下载安装包失败: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > MaxPackageBytes {
		return shared.LuaJITRelease{}, ErrInvalidPackage
	}
	total := response.ContentLength
	if total <= 0 {
		total = MaxPackageBytes
	}
	return s.Save(ctx, io.TeeReader(response.Body, &downloadCounter{ctx: ctx, total: total}), expected)
}
func (s *Store) Get(id string) (shared.LuaJITRelease, error) {
	var release shared.LuaJITRelease
	if !packageID.MatchString(id) {
		return release, os.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(s.root, id+".json"))
	if err != nil {
		return release, err
	}
	if err = json.Unmarshal(data, &release); err != nil {
		return release, err
	}
	if release.ID != id || release.SHA256 != id || release.OS != "linux" || release.Arch != "amd64" || release.Size <= 0 || release.Size > MaxPackageBytes {
		return release, ErrInvalidPackage
	}
	return release, nil
}
func (s *Store) List() ([]shared.LuaJITRelease, error) {
	paths, err := filepath.Glob(filepath.Join(s.root, "*.json"))
	if err != nil {
		return nil, err
	}
	items := []shared.LuaJITRelease{}
	for _, p := range paths {
		r, e := s.Get(strings.TrimSuffix(filepath.Base(p), ".json"))
		if e != nil {
			return nil, e
		}
		items = append(items, r)
	}
	sortReleases(items)
	return items, nil
}
func sortReleases(items []shared.LuaJITRelease) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Channel != items[j].Channel {
			if items[i].Channel == "upstream" {
				return true
			}
			if items[j].Channel == "upstream" {
				return false
			}
		}
		if items[i].Version != items[j].Version {
			left, right := strings.Split(items[i].Version, "."), strings.Split(items[j].Version, ".")
			for n := 0; n < len(left) && n < len(right); n++ {
				a, _ := strconv.Atoi(left[n])
				b, _ := strconv.Atoi(right[n])
				if a != b {
					return a > b
				}
			}
		}
		return items[i].ID < items[j].ID
	})
}
func (s *Store) Path(id string) (string, error) {
	_, err := s.Get(id)
	return filepath.Join(s.root, id+".zip"), err
}
func (s *Store) Grant(id string) (string, error) {
	if _, err := s.Get(id); err != nil {
		return "", err
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, g := range s.grants {
		if time.Now().After(g.Expires) {
			delete(s.grants, key)
		}
	}
	if len(s.grants) >= 128 {
		return "", errors.New("LuaJIT 下载任务过多")
	}
	s.grants[token] = grant{ID: id, Expires: time.Now().Add(15 * time.Minute)}
	return token, nil
}
func (s *Store) Revoke(token string) { s.mu.Lock(); delete(s.grants, token); s.mu.Unlock() }
func (s *Store) Open(id, token string) (*os.File, error) {
	s.mu.Lock()
	g, ok := s.grants[token]
	s.mu.Unlock()
	if !ok || g.ID != id || time.Now().After(g.Expires) {
		return nil, os.ErrPermission
	}
	p, err := s.Path(id)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}
