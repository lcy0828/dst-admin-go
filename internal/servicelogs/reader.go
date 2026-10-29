package servicelogs

import (
	"bufio"
	"bytes"
	"context"
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
	"strconv"
	"strings"
	"time"

	"dont/shared"
)

const scanBudget = int64(8 * 1024 * 1024)

var logName = regexp.MustCompile(`^service-[0-9]{20}-[0-9a-f]{32}\.jsonl$`)

type fileInfo struct {
	Name     string
	Size     int64
	Modified time.Time
}

func logFiles(root string) ([]fileInfo, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return []fileInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []fileInfo{}
	for _, e := range entries {
		if e.Type().IsRegular() && logName.MatchString(e.Name()) {
			info, err := e.Info()
			if err != nil {
				return nil, err
			}
			out = append(out, fileInfo{e.Name(), info.Size(), info.ModTime().UTC()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}
func Health(root string) shared.ServiceLogHealth {
	defaultMu.RLock()
	active := defaultStore
	defaultMu.RUnlock()
	var v shared.ServiceLogHealth
	if active != nil && active.config.Root == root {
		v = active.Health()
	} else {
		data, err := os.ReadFile(filepath.Join(root, "health.json"))
		if err != nil || json.Unmarshal(data, &v) != nil {
			v.Persistence = "not_initialized"
		}
	}
	if !ReadSettings(root).Enabled {
		v.Persistence = "disabled"
	}
	return v
}
func Source(root string) (shared.ServiceLogSource, error) {
	v := shared.ServiceLogSource{ID: "controller", Name: "管理服务", NodeID: "local", Online: true, Supported: true, Health: Health(root), Settings: ReadSettings(root)}
	files, err := logFiles(root)
	if err != nil {
		v.Health.Persistence = "degraded"
		v.Health.LastError = Redact(err.Error())
		return v, err
	}
	cutoff := time.Now().Add(-time.Duration(v.Settings.RetentionDays) * 24 * time.Hour)
	for _, f := range files {
		if f.Modified.Before(cutoff) {
			continue
		}
		v.Files++
		v.Bytes += f.Size
		if v.OldestAt == nil || f.Modified.Before(*v.OldestAt) {
			stamp := f.Modified
			v.OldestAt = &stamp
		}
		if v.NewestAt == nil || f.Modified.After(*v.NewestAt) {
			stamp := f.Modified
			v.NewestAt = &stamp
		}
	}
	// Segment creation is a conservative coverage boundary; it is not proof of
	// continuous logging. The page reports truncation and missing cursors itself.
	if len(files) > 0 {
		for i := len(files) - 1; i >= 0; i-- {
			if files[i].Modified.Before(cutoff) {
				continue
			}
			if file, err := os.Open(filepath.Join(root, files[i].Name)); err == nil {
				raw, _ := bufio.NewReader(io.LimitReader(file, maxEntryBytes+1)).ReadBytes('\n')
				file.Close()
				if idx := bytes.IndexByte(raw, '\n'); idx >= 0 {
					var e shared.ServiceLogEntry
					if json.Unmarshal(raw[:idx], &e) == nil {
						if e.Time.Before(cutoff) {
							e.Time = cutoff
						}
						v.OldestAt = &e.Time
					}
				}
			}
			break
		}
	}
	return v, nil
}

type queryCursor struct {
	File   string    `json:"f"`
	Offset int64     `json:"o"`
	From   time.Time `json:"a"`
	To     time.Time `json:"b"`
	Filter string    `json:"q"`
}

func filterHash(q shared.ServiceLogQuery) string {
	q.Cursor = ""
	q.AfterID = ""
	q.Limit = 0
	data, _ := json.Marshal(q)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func encodeCursor(c queryCursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}
func parseCursor(raw string) (queryCursor, error) {
	var c queryCursor
	if len(raw) > 2048 {
		return c, ErrInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(data, &c) != nil || !logName.MatchString(c.File) || c.Offset < 0 {
		return c, ErrInvalid
	}
	return c, nil
}
func NormalizeQuery(q shared.ServiceLogQuery) (shared.ServiceLogQuery, error) {
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Limit < 1 || q.Limit > 500 || len(q.Keyword) > 256 || len(q.RequestID) > 128 || len(q.JobID) > 128 || len(q.OperationID) > 128 || len(q.Component) > 64 || len(q.AfterID) > 256 {
		return q, ErrInvalid
	}
	switch q.Level {
	case "", "debug", "info", "warn", "error", "fatal":
	default:
		return q, ErrInvalid
	}
	if q.Cursor != "" {
		c, err := parseCursor(q.Cursor)
		if err != nil {
			return q, err
		}
		if q.From.IsZero() {
			q.From = c.From
		}
		if q.To.IsZero() {
			q.To = c.To
		}
	}
	if q.To.IsZero() {
		q.To = time.Now().UTC()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-time.Hour)
	}
	if !q.From.Before(q.To) || q.To.Sub(q.From) > 31*24*time.Hour {
		return q, ErrInvalid
	}
	if strings.ContainsAny(q.RequestID+q.JobID+q.OperationID, "\r\n\x00") {
		return q, ErrInvalid
	}
	return q, nil
}
func Query(ctx context.Context, root string, q shared.ServiceLogQuery) (shared.ServiceLogPage, error) {
	q, err := NormalizeQuery(q)
	page := shared.ServiceLogPage{Items: []shared.ServiceLogEntry{}, From: q.From, To: q.To, Health: Health(root)}
	if err != nil {
		return page, err
	}
	files, err := logFiles(root)
	if err != nil {
		return page, err
	}
	start, offset := 0, int64(-1)
	if q.Cursor != "" {
		c, err := parseCursor(q.Cursor)
		if err != nil {
			return page, err
		}
		if c.Filter != filterHash(q) {
			return page, ErrInvalid
		}
		found := false
		for i, f := range files {
			if f.Name == c.File {
				start, offset, found = i, c.Offset, true
				break
			}
		}
		if !found {
			return page, ErrCursorExpired
		}
	}
	foundAfter := q.AfterID == ""
	cutoff := time.Now().Add(-time.Duration(ReadSettings(root).RetentionDays) * 24 * time.Hour)
	if q.From.Before(cutoff) {
		page.CoverageLost = true
	}
	responseBytes := 0
	for index := start; index < len(files); index++ {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		f := files[index]
		if f.Modified.Before(cutoff) {
			continue
		}
		file, err := os.Open(filepath.Join(root, f.Name))
		if os.IsNotExist(err) {
			page.CoverageLost = true
			continue
		}
		if err != nil {
			return page, err
		}
		end := f.Size
		if index == start && offset >= 0 {
			if offset > f.Size {
				file.Close()
				return page, ErrCursorExpired
			}
			end = offset
		}
		for end > 0 {
			if err := ctx.Err(); err != nil {
				file.Close()
				return page, err
			}
			if page.ScannedBytes >= scanBudget {
				page.Limited = true
				page.HasMore = true
				page.NextCursor = encodeCursor(queryCursor{f.Name, end, q.From, q.To, filterHash(q)})
				file.Close()
				return page, nil
			}
			begin := end - 64*1024
			if begin < 0 {
				begin = 0
			}
			raw := make([]byte, end-begin)
			n, readErr := file.ReadAt(raw, begin)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				file.Close()
				return page, readErr
			}
			raw = raw[:n]
			page.ScannedBytes += int64(n)
			first := int64(0)
			if begin > 0 {
				cut := bytes.IndexByte(raw, '\n')
				if cut < 0 {
					file.Close()
					return page, fmt.Errorf("%w: oversized log record", ErrInvalid)
				}
				first = int64(cut + 1)
			}
			consumed := false
			for last := len(raw); last > int(first); {
				if raw[last-1] == '\n' {
					last--
					if last <= int(first) {
						break
					}
				}
				prev := bytes.LastIndexByte(raw[:last], '\n')
				lineStart := prev + 1
				if lineStart < int(first) {
					break
				}
				line := raw[lineStart:last]
				recordOffset := begin + int64(lineStart)
				last = prev + 1
				consumed = true
				var entry shared.ServiceLogEntry
				if len(line) > maxEntryBytes || json.Unmarshal(line, &entry) != nil {
					page.CoverageLost = true
					continue
				}
				if entry.ID == q.AfterID {
					foundAfter = true
					file.Close()
					page.CoverageLost = page.CoverageLost || !foundAfter
					return page, nil
				}
				if !matches(entry, q) {
					continue
				}
				if entry.Time.Before(cutoff) {
					continue
				}
				entry = normalize(entry, Config{})
				responseBytes += len(line)
				if responseBytes > 512*1024 {
					page.Limited = true
					page.HasMore = true
					page.NextCursor = encodeCursor(queryCursor{f.Name, recordOffset + int64(len(line)) + 1, q.From, q.To, filterHash(q)})
					file.Close()
					return page, nil
				}
				page.Items = append(page.Items, entry)
				if len(page.Items) >= q.Limit {
					page.HasMore = recordOffset > 0 || index < len(files)-1
					page.NextCursor = encodeCursor(queryCursor{f.Name, recordOffset, q.From, q.To, filterHash(q)})
					page.CoverageLost = page.CoverageLost || q.AfterID != ""
					file.Close()
					return page, nil
				}
			}
			next := begin + first
			if !consumed && next >= end {
				file.Close()
				return page, ErrInvalid
			}
			end = next
		}
		file.Close()
	}
	page.CoverageLost = page.CoverageLost || !foundAfter
	return page, nil
}
func matches(e shared.ServiceLogEntry, q shared.ServiceLogQuery) bool {
	keyword := strings.ToLower(q.Keyword)
	return !e.Time.Before(q.From) && !e.Time.After(q.To) && (q.Level == "" || e.Level == q.Level) && (q.Component == "" || e.Component == q.Component) && (q.RequestID == "" || e.RequestID == q.RequestID) && (q.JobID == "" || e.JobID == q.JobID) && (q.OperationID == "" || e.OperationID == q.OperationID) && (keyword == "" || strings.Contains(strings.ToLower(e.Message), keyword) || strings.Contains(strings.ToLower(e.Stack), keyword) || strings.Contains(strings.ToLower(e.Code), keyword))
}
func Context(ctx context.Context, root, id string, radius int) (shared.ServiceLogPage, error) {
	page := shared.ServiceLogPage{Items: []shared.ServiceLogEntry{}, Health: Health(root)}
	if radius == 0 {
		radius = 20
	}
	if radius < 1 || radius > 50 || len(id) > 256 {
		return page, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return page, ErrInvalid
	}
	name, offsetRaw, ok := strings.Cut(string(raw), "@")
	offset, err := strconv.ParseInt(offsetRaw, 10, 64)
	if !ok || err != nil || offset < 0 || !logName.MatchString(name) {
		return page, ErrInvalid
	}
	file, err := os.Open(filepath.Join(root, name))
	if os.IsNotExist(err) {
		return page, ErrCursorExpired
	}
	if err != nil {
		return page, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return page, err
	}
	start := offset - 512*1024
	if start < 0 {
		start = 0
	}
	end := offset + 512*1024
	if end > info.Size() {
		end = info.Size()
	}
	if end <= start {
		return page, ErrCursorExpired
	}
	data := make([]byte, end-start)
	n, err := file.ReadAt(data, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return page, err
	}
	data = data[:n]
	page.ScannedBytes = int64(n)
	entries := []shared.ServiceLogEntry{}
	anchor := -1
	cutoff := time.Now().Add(-time.Duration(ReadSettings(root).RetentionDays) * 24 * time.Hour)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		var e shared.ServiceLogEntry
		if len(line) <= maxEntryBytes && json.Unmarshal(line, &e) == nil && !e.Time.Before(cutoff) {
			if e.ID == id {
				anchor = len(entries)
			}
			entries = append(entries, normalize(e, Config{}))
		}
	}
	if anchor < 0 {
		return page, ErrCursorExpired
	}
	left, right := anchor-radius, anchor+radius+1
	if left < 0 {
		left = 0
	}
	if right > len(entries) {
		right = len(entries)
	}
	page.Items = entries[left:right]
	page.Limited = (start > 0 && left == 0) || (end < info.Size() && right == len(entries))
	if len(page.Items) > 0 {
		page.From = page.Items[0].Time
		page.To = page.Items[len(page.Items)-1].Time
	}
	return page, nil
}
func Execute(ctx context.Context, root string, request shared.ServiceLogRequest) (shared.ServiceLogResult, error) {
	var result shared.ServiceLogResult
	switch request.Action {
	case "query":
		v, e := Query(ctx, root, request.Query)
		result.Page = &v
		return result, e
	case "context":
		v, e := Context(ctx, root, request.EntryID, request.Radius)
		result.Page = &v
		return result, e
	case "status":
		v, e := Source(root)
		result.Source = &v
		return result, e
	case "settings.read":
		v := ReadSettings(root)
		result.Settings = &v
		return result, nil
	case "settings.update":
		if request.Settings == nil {
			return result, ErrInvalid
		}
		v, e := SaveSettings(root, *request.Settings)
		result.Settings = &v
		return result, e
	default:
		return result, ErrInvalid
	}
}
