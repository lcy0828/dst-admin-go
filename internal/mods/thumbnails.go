package mods

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp"
)

const thumbnailLimit = 2 << 20

type Thumbnail struct {
	Data        []byte
	ContentType string
	ETag        string
}

func thumbnail(data []byte) (Thumbnail, error) {
	if len(data) == 0 || len(data) > thumbnailLimit {
		return Thumbnail{}, &NetworkError{Code: "WORKSHOP_IMAGE_INVALID"}
	}
	kind := http.DetectContentType(data)
	if kind != "image/jpeg" && kind != "image/png" && kind != "image/gif" && kind != "image/webp" {
		return Thumbnail{}, &NetworkError{Code: "WORKSHOP_IMAGE_INVALID"}
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 16_000_000 {
		return Thumbnail{}, &NetworkError{Code: "WORKSHOP_IMAGE_INVALID"}
	}
	sum := sha256.Sum256(data)
	return Thumbnail{Data: data, ContentType: kind, ETag: `"` + hex.EncodeToString(sum[:]) + `"`}, nil
}

func allowedImageURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return nil, &NetworkError{Code: "WORKSHOP_IMAGE_SOURCE_REJECTED"}
	}
	host := strings.ToLower(u.Hostname())
	allowed := false
	for _, domain := range []string{"steamusercontent.com", "steamuserimages-a.akamaihd.net", "steamstatic.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			allowed = true
		}
	}
	if !allowed {
		return nil, &NetworkError{Code: "WORKSHOP_IMAGE_SOURCE_REJECTED"}
	}
	return u, nil
}

func fetchThumbnail(ctx context.Context, client *http.Client, raw string, size int) (Thumbnail, error) {
	u, err := allowedImageURL(raw)
	if err != nil {
		return Thumbnail{}, err
	}
	q := u.Query()
	q.Set("imw", strconv.Itoa(size))
	q.Set("imh", strconv.Itoa(size))
	q.Set("ima", "fit")
	q.Set("impolicy", "Letterbox")
	u.RawQuery = q.Encode()
	ctx = context.WithValue(ctx, networkKindKey{}, 2)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Thumbnail{}, &NetworkError{Code: "WORKSHOP_IMAGE_SOURCE_REJECTED"}
	}
	response, err := client.Do(req)
	if err != nil {
		return Thumbnail{}, safeNetworkError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Thumbnail{}, &NetworkError{Code: "WORKSHOP_UPSTREAM_REJECTED", Status: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, thumbnailLimit+1))
	if err != nil {
		return Thumbnail{}, safeNetworkError(err)
	}
	return thumbnail(data)
}

func (w *Workshop) Thumbnail(ctx context.Context, id string, size int) (Thumbnail, error) {
	if !validModID(id) || (size != 80 && size != 184 && size != 384) {
		return Thumbnail{}, ErrInvalidRequest
	}
	s := w.snapshot()
	sum := sha256.Sum256([]byte(s.config.scope() + ":" + id + ":" + strconv.Itoa(size)))
	name := hex.EncodeToString(sum[:]) + ".thumb"
	value, err := w.coalesce(ctx, s.version+":image:"+name, 8*time.Second, func(ctx context.Context) (any, error) {
		defer w.finish(s)
		if data, ok := w.readImage(name); ok {
			return data, nil
		}
		w.mu.Lock()
		preview := w.cards[s.version+":"+id].PreviewURL
		w.mu.Unlock()
		if preview == "" {
			items, err := s.provider.BasicDetails(ctx, []string{id})
			if err != nil && len(items) == 0 {
				return nil, err
			}
			preview = items[id].PreviewURL
		}
		if preview == "" {
			return nil, &NetworkError{Code: "WORKSHOP_IMAGE_UNAVAILABLE"}
		}
		data, err := fetchThumbnail(ctx, s.provider.HTTPClient, preview, size)
		if err != nil {
			return nil, err
		}
		w.writeImage(name, data.Data)
		return data, nil
	})
	if err != nil {
		return Thumbnail{}, err
	}
	return value.(Thumbnail), nil
}

func (w *Workshop) readImage(name string) (Thumbnail, bool) {
	if w.imageRoot == "" {
		return Thumbnail{}, false
	}
	w.imageMu.Lock()
	defer w.imageMu.Unlock()
	path := filepath.Join(w.imageRoot, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > thumbnailLimit || time.Since(info.ModTime()) > 24*time.Hour {
		return Thumbnail{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return Thumbnail{}, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, thumbnailLimit+1))
	if err != nil {
		return Thumbnail{}, false
	}
	result, err := thumbnail(data)
	if err != nil {
		return Thumbnail{}, false
	}
	// Track recency separately; reading a file must not extend its freshness.
	if len(w.imageUsed) >= 1024 {
		for key := range w.imageUsed {
			delete(w.imageUsed, key)
			break
		}
	}
	w.imageUsed[name] = time.Now()
	return result, true
}

func (w *Workshop) writeImage(name string, data []byte) {
	if w.imageRoot == "" {
		return
	}
	w.imageMu.Lock()
	defer w.imageMu.Unlock()
	if err := os.MkdirAll(w.imageRoot, 0700); err != nil {
		return
	}
	f, err := os.CreateTemp(w.imageRoot, ".thumbnail-")
	if err != nil {
		return
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return
	}
	if err = f.Close(); err != nil {
		return
	}
	if err = os.Rename(path, filepath.Join(w.imageRoot, name)); err != nil {
		return
	}
	entries, err := os.ReadDir(w.imageRoot)
	if err != nil {
		return
	}
	type entry struct {
		name string
		info os.FileInfo
	}
	files := []entry{}
	var total int64
	for _, item := range entries {
		if !strings.HasSuffix(item.Name(), ".thumb") {
			continue
		}
		info, err := item.Info()
		if err == nil && info.Mode().IsRegular() {
			files = append(files, entry{item.Name(), info})
			total += info.Size()
		}
	}
	// Work is bounded by the dedicated cache (at most 512 thumbnail files).
	sort.Slice(files, func(i, j int) bool {
		a, b := w.imageUsed[files[i].name], w.imageUsed[files[j].name]
		if a.IsZero() {
			a = files[i].info.ModTime()
		}
		if b.IsZero() {
			b = files[j].info.ModTime()
		}
		return a.Before(b)
	})
	remaining := len(files)
	for _, item := range files {
		if total <= 64<<20 && remaining <= 512 {
			break
		}
		if err := os.Remove(filepath.Join(w.imageRoot, item.name)); err == nil {
			total -= item.info.Size()
			delete(w.imageUsed, item.name)
			remaining--
		}
	}
}
