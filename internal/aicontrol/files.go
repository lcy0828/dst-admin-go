package aicontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dont/internal/apicontract"
	"github.com/google/uuid"
)

const MaxAttachmentBytes int64 = 2 << 30
const attachmentQuota int64 = 4 << 30
const fileLifetime = 24 * time.Hour

// Files are streamed to private, bounded temporary storage. Neither model
// prompts nor operation receipts contain file contents or host filesystem paths.
type Attachment struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"createdAt"`
	Owner     string    `json:"-"`
}
type attachmentRecord struct {
	Attachment
	Source string `json:"source"`
}
type Resource struct {
	ID          string            `json:"id"`
	Owner       string            `json:"owner"`
	OperationID string            `json:"operationId"`
	Input       apicontract.Input `json:"input"`
	CreatedAt   time.Time         `json:"createdAt"`
}

func validFileID(id string) bool { v, err := uuid.Parse(id); return err == nil && v.String() == id }
func (s *Store) filePath(kind, id, suffix string) string {
	return filepath.Join(s.root, kind, id+suffix)
}
func (s *Store) fileRecords(kind string) ([]os.DirEntry, error) {
	dir := filepath.Join(s.root, kind)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var records []os.DirEntry
	for _, item := range items {
		info, e := item.Info()
		if e != nil {
			continue
		}
		if now.Sub(info.ModTime()) > fileLifetime {
			if e = os.Remove(filepath.Join(dir, item.Name())); e != nil && !os.IsNotExist(e) {
				return nil, e
			}
			continue
		}
		if strings.HasSuffix(item.Name(), ".json") {
			records = append(records, item)
		}
	}
	return records, nil
}
func (s *Store) writeFileRecord(kind, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.root, kind)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".record-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = joinClose(err, f)
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.filePath(kind, id, ".json"))
}
func joinClose(err error, f *os.File) error {
	e := f.Close()
	if err != nil {
		return err
	}
	return e
}

type contextFileReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextFileReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (s *Store) SaveAttachment(ctx context.Context, p Principal, name string, r io.Reader, sizeBound ...int64) (Attachment, error) {
	if p.ID == "" {
		return Attachment{}, ErrDenied
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || len(name) > 255 || strings.ContainsAny(name, "\x00\r\n") {
		return Attachment{}, ErrInvalid
	}
	reserve := MaxAttachmentBytes
	if len(sizeBound) > 0 && sizeBound[0] >= 0 && sizeBound[0] < reserve {
		reserve = sizeBound[0]
	}
	s.filesMu.Lock()
	items, err := s.fileRecords("attachments")
	var used int64
	// Include interrupted uploads left by a process crash, even if no metadata
	// was committed. Active writes are conservatively covered by reservations.
	if err == nil {
		entries, e := os.ReadDir(filepath.Join(s.root, "attachments"))
		err = e
		for _, item := range entries {
			if strings.HasSuffix(item.Name(), ".data") || strings.HasPrefix(item.Name(), ".upload-") {
				info, e := item.Info()
				if e != nil {
					err = e
					break
				}
				used += info.Size()
			}
		}
	}
	if err == nil && (len(items) >= 64 || used+s.uploadReserved+reserve > attachmentQuota) {
		err = ErrBusy
	}
	if err == nil {
		s.uploadReserved += reserve
	}
	s.filesMu.Unlock()
	if err != nil {
		return Attachment{}, err
	}
	defer func() { s.filesMu.Lock(); s.uploadReserved -= reserve; s.filesMu.Unlock() }()
	id := uuid.NewString()
	f, err := os.CreateTemp(filepath.Join(s.root, "attachments"), ".upload-")
	if err != nil {
		return Attachment{}, err
	}
	defer os.Remove(f.Name())
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(contextFileReader{ctx, r}, reserve+1))
	if err == nil && n > reserve {
		err = fmt.Errorf("%w: file exceeds its declared size or 2 GiB limit", ErrInvalid)
	}
	if err == nil {
		err = f.Sync()
	}
	err = joinClose(err, f)
	if err != nil {
		return Attachment{}, err
	}
	value := Attachment{ID: id, Name: name, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil)), CreatedAt: time.Now().UTC(), Owner: p.ID}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if err = os.Rename(f.Name(), s.filePath("attachments", id, ".data")); err != nil {
		return Attachment{}, err
	}
	if err = s.writeFileRecord("attachments", id, attachmentRecord{value, p.ID}); err != nil {
		os.Remove(s.filePath("attachments", id, ".data"))
		return Attachment{}, err
	}
	return value, nil
}
func (s *Store) Attachment(p Principal, id string) (Attachment, error) {
	if !validFileID(id) || p.ID == "" {
		return Attachment{}, ErrDenied
	}
	raw, err := os.ReadFile(s.filePath("attachments", id, ".json"))
	if err != nil {
		return Attachment{}, ErrUnavailable
	}
	var record attachmentRecord
	if json.Unmarshal(raw, &record) != nil || record.Source != p.ID {
		return Attachment{}, ErrDenied
	}
	if time.Since(record.CreatedAt) > fileLifetime {
		return Attachment{}, ErrUnavailable
	}
	record.Owner = record.Source
	return record.Attachment, nil
}
func (s *Store) OpenAttachment(p Principal, id string) (Attachment, *os.File, error) {
	v, err := s.Attachment(p, id)
	if err != nil {
		return v, nil, err
	}
	f, err := os.Open(s.filePath("attachments", id, ".data"))
	return v, f, err
}
func (s *Store) DeleteAttachment(p Principal, id string) error {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if _, err := s.Attachment(p, id); err != nil {
		return err
	}
	if err := os.Remove(s.filePath("attachments", id, ".data")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Remove(s.filePath("attachments", id, ".json"))
}
func (s *Store) CreateResource(p Principal, op *apicontract.Operation, input apicontract.Input) (Resource, error) {
	if p.ID == "" || op.Method != "GET" || !op.Callable() || !p.AllowsAPI(op, input) {
		return Resource{}, ErrDenied
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	items, err := s.fileRecords("resources")
	if err != nil {
		return Resource{}, err
	}
	if len(items) >= 256 {
		return Resource{}, ErrBusy
	}
	v := Resource{ID: uuid.NewString(), Owner: p.ID, OperationID: op.ID, Input: input, CreatedAt: time.Now().UTC()}
	err = s.writeFileRecord("resources", v.ID, v)
	return v, err
}
func (s *Store) Resource(p Principal, id string) (Resource, error) {
	if !validFileID(id) || p.ID == "" {
		return Resource{}, ErrDenied
	}
	raw, err := os.ReadFile(s.filePath("resources", id, ".json"))
	if err != nil {
		return Resource{}, ErrUnavailable
	}
	var v Resource
	if json.Unmarshal(raw, &v) != nil || v.Owner != p.ID {
		return Resource{}, ErrDenied
	}
	if time.Since(v.CreatedAt) > fileLifetime {
		return Resource{}, ErrUnavailable
	}
	return v, nil
}
