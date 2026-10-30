package softwareupdate

import (
	"context"
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
)

type DownloadProgress struct {
	Bytes, Total, Speed int64
	Source              string
	Resumed             bool
	Attempt             int
}

type ResumableClient interface {
	DownloadFile(context.Context, Asset, string, *os.File, func(DownloadProgress)) error
}

type route struct {
	url    string
	client *http.Client
	name   string
}

func (g *GitHubClient) routes(urls []string, source string) []route {
	var result []route
	for _, raw := range urls {
		mirror := strings.HasPrefix(raw, g.mirrorPrefix)
		if mirror && strings.Contains(raw, "https://api.github.com/") {
			continue
		}
		name := "direct"
		if mirror {
			name = "mirror"
		}
		client := g.client
		if mirror && source == "auto" && g.direct != nil {
			client = g.direct
		}
		result = append(result, route{raw, client, name})
		if !mirror && source == "auto" && g.direct != nil {
			result[len(result)-1].name = "configured-proxy"
			result = append(result, route{raw, g.direct, "direct"})
		}
	}
	return result
}

func transportError(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return fmt.Errorf("release connection failed: %w", u.Err)
	}
	return err
}

func (g *GitHubClient) DownloadFile(ctx context.Context, asset Asset, source string, file *os.File, progress func(DownloadProgress)) error {
	urls, err := g.sourceURLs(asset.URL, source)
	if err != nil {
		return err
	}
	return downloadRoutes(ctx, asset, g.routes(urls, source), file, progress, nil)
}

// DownloadHTTPFile is shared by the authenticated controller relay. The caller
// owns URL/redirect trust and authentication; the range protocol is identical.
func DownloadHTTPFile(ctx context.Context, asset Asset, client *http.Client, address string, headers http.Header, file *os.File, progress func(DownloadProgress)) error {
	return downloadRoutes(ctx, asset, []route{{address, client, "controller"}}, file, progress, headers)
}

type rangeIdentity struct{ URL, ETag string }

func downloadRoutes(ctx context.Context, asset Asset, routes []route, file *os.File, progress func(DownloadProgress), headers http.Header) error {
	if asset.Size < 1 || asset.Size > MaxArchiveBytes {
		return ErrInvalid
	}
	var last error
	attempt := 0
	for _, route := range routes {
		for retry := 0; retry < 3; retry++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			attempt++
			if retry > 0 {
				timer := time.NewTimer(time.Duration(retry) * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
			info, err := file.Stat()
			if err != nil {
				return err
			}
			offset := info.Size()
			if offset == asset.Size {
				return nil
			}
			if offset > asset.Size {
				if err := file.Truncate(0); err != nil {
					return err
				}
				offset = 0
			}
			attemptCtx, cancel := context.WithCancel(ctx)
			req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, route.url, nil)
			if err != nil {
				cancel()
				return err
			}
			for key, values := range headers {
				req.Header[key] = append([]string(nil), values...)
			}
			req.Header.Set("User-Agent", "dst-admin-go/software-update")
			req.Header.Set("Accept-Encoding", "identity")
			var identity rangeIdentity
			if data, err := regularRead(file.Name()+".range.json", 4096); err == nil {
				_ = json.Unmarshal(data, &identity)
			}
			if offset > 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
				if identity.URL == route.url && identity.ETag != "" {
					req.Header.Set("If-Range", identity.ETag)
				}
			}
			timer := time.AfterFunc(20*time.Second, cancel)
			response, err := route.client.Do(req)
			timer.Stop()
			if err != nil {
				cancel()
				last = transportError(err)
				continue
			}
			start := offset
			switch response.StatusCode {
			case http.StatusOK:
				start = 0
				err = file.Truncate(0)
			case http.StatusPartialContent:
				var begin, end, total int64
				if _, parseErr := fmt.Sscanf(response.Header.Get("Content-Range"), "bytes %d-%d/%d", &begin, &end, &total); parseErr != nil || begin != offset || total != asset.Size || end != total-1 || strings.TrimSpace(response.Header.Get("Content-Range")) != fmt.Sprintf("bytes %d-%d/%d", begin, end, total) {
					err = fmt.Errorf("%w: invalid download range", ErrInvalid)
				}
			case http.StatusRequestedRangeNotSatisfiable:
				err = file.Truncate(0)
				if err == nil {
					err = fmt.Errorf("release range expired; restarting download")
				}
			default:
				err = fmt.Errorf("release server returned HTTP %d", response.StatusCode)
			}
			if err == nil && response.ContentLength >= 0 && response.ContentLength != asset.Size-start {
				err = fmt.Errorf("%w: release length changed", ErrInvalid)
			}
			if err == nil && response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" {
				err = ErrInvalid
			}
			if err == nil {
				_, err = file.Seek(start, io.SeekStart)
			}
			if err == nil {
				etag := response.Header.Get("ETag")
				if !strings.HasPrefix(etag, "\"") || !strings.HasSuffix(etag, "\"") || len(etag) > 1024 {
					etag = ""
				}
				err = writeJSON(file.Name()+".range.json", rangeIdentity{route.url, etag})
			}
			if err == nil {
				stall := time.AfterFunc(45*time.Second, cancel)
				p := &downloadWriter{file: file, start: start, count: start, began: time.Now(), stall: stall, progress: progress, state: DownloadProgress{Total: asset.Size, Source: route.name, Resumed: start > 0, Attempt: attempt}}
				if progress != nil {
					state := p.state
					state.Bytes = start
					progress(state)
				}
				_, err = io.Copy(p, io.LimitReader(response.Body, asset.Size-start+1))
				stall.Stop()
				if err == nil && p.count != asset.Size {
					err = io.ErrUnexpectedEOF
				}
				if p.count > asset.Size {
					_ = file.Truncate(0)
					err = ErrInvalid
				}
				if syncErr := file.Sync(); err == nil {
					err = syncErr
				}
			}
			response.Body.Close()
			cancel()
			if err == nil {
				return nil
			}
			last = err
			if response.StatusCode == 429 || response.StatusCode == 503 {
				delay := retryAfter(response.Header.Get("Retry-After"))
				if delay > 0 {
					timer := time.NewTimer(delay)
					select {
					case <-ctx.Done():
						timer.Stop()
						return ctx.Err()
					case <-timer.C:
					}
				}
			}
		}
	}
	if last == nil {
		last = ErrInvalid
	}
	return last
}

func retryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 && seconds <= 86400 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && time.Until(at) > 0 {
		return time.Until(at)
	}
	return 0
}

type downloadWriter struct {
	file         *os.File
	start, count int64
	began, last  time.Time
	stall        *time.Timer
	progress     func(DownloadProgress)
	state        DownloadProgress
}

func (w *downloadWriter) Write(data []byte) (int, error) {
	n, err := w.file.Write(data)
	w.count += int64(n)
	if n > 0 {
		w.stall.Reset(45 * time.Second)
	}
	if w.progress != nil && (time.Since(w.last) >= 250*time.Millisecond || w.count == w.state.Total) {
		w.last = time.Now()
		p := w.state
		p.Bytes = w.count
		p.Speed = int64(float64(w.count-w.start) / time.Since(w.began).Seconds())
		w.progress(p)
	}
	return n, err
}

// A cache has a single writer. Relay users open the verified file before
// releasing this lock, so eviction cannot invalidate an active transfer.
var cacheLocks sync.Map

func cacheLock(ctx context.Context, root string) (func(), error) {
	value, _ := cacheLocks.LoadOrStore(root, make(chan struct{}, 1))
	slot := value.(chan struct{})
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// VerifiedArchive retains interrupted bytes, but always hashes the entire
// resulting file before exposing it. Cache names bind size, identity and SHA.
func VerifiedArchive(ctx context.Context, root string, client ReleaseClient, asset Asset, source, digest string, progress func(DownloadProgress)) (*os.File, error) {
	if !digestPattern.MatchString(digest) || asset.Size < 1 || asset.Size > MaxArchiveBytes {
		return nil, ErrInvalid
	}
	if err := ensureRoot(root); err != nil {
		return nil, err
	}
	directory := filepath.Join(root, "downloads")
	unlock, err := cacheLock(ctx, directory)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalid
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", asset.URL, asset.Name, asset.Size, digest)))
	path := filepath.Join(directory, hex.EncodeToString(key[:])+".part")
	if err := pruneDownloads(directory, path, asset.Size); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, ErrInvalid
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			file.Close()
		}
	}()
	if err := writeJSON(path+".json", struct {
		Asset  Asset
		SHA256 string
	}{asset, digest}); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if resume, yes := client.(ResumableClient); yes {
			err = resume.DownloadFile(ctx, asset, source, file, progress)
		} else {
			if err = file.Truncate(0); err == nil {
				_, err = file.Seek(0, io.SeekStart)
			}
			if err == nil {
				stall := time.NewTimer(time.Hour)
				err = client.Download(ctx, asset, source, &downloadWriter{file: file, began: time.Now(), stall: stall, progress: progress, state: DownloadProgress{Total: asset.Size, Source: source}})
				stall.Stop()
			}
		}
		if err != nil {
			return nil, err
		}
		if err = file.Sync(); err != nil {
			return nil, err
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		hash := sha256.New()
		n, copyErr := io.Copy(hash, io.LimitReader(file, asset.Size+1))
		if copyErr != nil {
			return nil, copyErr
		}
		if n == asset.Size && hex.EncodeToString(hash.Sum(nil)) == digest {
			_, err = file.Seek(0, io.SeekStart)
			if err != nil {
				return nil, err
			}
			ok = true
			return file, nil
		}
		if err = file.Truncate(0); err != nil {
			return nil, err
		}
		_ = os.Remove(path + ".range.json")
	}
	return nil, fmt.Errorf("%w: downloaded archive checksum mismatch", ErrInvalid)
}

func pruneDownloads(directory, keep string, required int64) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	var files []os.FileInfo
	var total int64
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".part") || !digestPattern.MatchString(strings.TrimSuffix(entry.Name(), ".part")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrInvalid
		}
		if filepath.Join(directory, entry.Name()) != keep {
			files = append(files, info)
			total += info.Size()
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime().Before(files[j].ModTime()) })
	for _, info := range files {
		if time.Since(info.ModTime()) < 72*time.Hour && total+required <= 1<<30 {
			continue
		}
		path := filepath.Join(directory, info.Name())
		if err := os.Remove(path); err != nil {
			return err
		}
		_ = os.Remove(path + ".json")
		_ = os.Remove(path + ".range.json")
		total -= info.Size()
	}
	return nil
}
