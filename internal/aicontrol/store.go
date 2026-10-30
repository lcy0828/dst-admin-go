package aicontrol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

type configRecord struct {
	ID     int    `gorm:"primary_key"`
	Data   string `gorm:"type:text"`
	Secret string `gorm:"type:text"`
}
type tokenRecord struct {
	ID      string `gorm:"primary_key;type:varchar(64)"`
	Digest  string `gorm:"unique_index;type:varchar(64)"`
	Data    string `gorm:"type:text"`
	AdminID int    `gorm:"not null;default:0"`
}
type Store struct {
	db                                       *gorm.DB
	configTable, tokenTable, callTable, root string
	mu                                       sync.Mutex
	filesMu                                  sync.Mutex
	uploadReserved                           int64
}

func NewStore(db *gorm.DB, prefix, root string) *Store {
	return &Store{db: db, configTable: prefix + "ai_config", tokenTable: prefix + "ai_token", callTable: prefix + "ai_call", root: root}
}
func (s *Store) Migrate() error {
	for _, t := range []struct {
		name  string
		model any
	}{{s.configTable, &configRecord{}}, {s.tokenTable, &tokenRecord{}}, {s.callTable, &Call{}}} {
		if err := s.db.Table(t.name).AutoMigrate(t.model).Error; err != nil {
			return err
		}
	}
	// An interrupted mutation may already have reached the game. Never replay it.
	return s.db.Table(s.callTable).Where("status = ?", "executing").Updates(map[string]any{"status": "uncertain", "result": `{"error":"AI_INTERRUPTED_CHECK_JOB_BEFORE_RETRY"}`}).Error
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func (s *Store) config() (configRecord, Config, error) {
	var row configRecord
	err := s.db.Table(s.configTable).Where("id = ?", 1).First(&row).Error
	if gorm.IsRecordNotFoundError(err) {
		limits := defaultChatLimits()
		return row, Config{Revision: "initial", Limits: &limits}, nil
	}
	var value Config
	if err == nil {
		err = json.Unmarshal([]byte(row.Data), &value)
	}
	if value.Limits == nil {
		limits := defaultChatLimits()
		value.Limits = &limits
	}
	return row, value, err
}
func (s *Store) Config() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, v, err := s.config()
	return v, err
}
func (s *Store) aead(create bool) (cipher.AEAD, error) {
	file := filepath.Join(s.root, "provider.key")
	key, err := os.ReadFile(file)
	if os.IsNotExist(err) && create {
		if err = os.MkdirAll(s.root, 0700); err != nil {
			return nil, err
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if os.IsExist(e) {
			key, err = os.ReadFile(file)
		} else if e != nil {
			return nil, e
		} else {
			_, err = f.Write(key)
			if err == nil {
				err = f.Sync()
			}
			err = errors.Join(err, f.Close())
		}
	}
	if err != nil || len(key) != 32 {
		return nil, ErrUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (s *Store) Provider() (Config, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, v, err := s.config()
	if err != nil || r.Secret == "" {
		return v, "", err
	}
	a, err := s.aead(false)
	if err != nil {
		return v, "", err
	}
	data, err := base64.StdEncoding.DecodeString(r.Secret)
	if err != nil || len(data) < a.NonceSize() {
		return v, "", ErrUnavailable
	}
	plain, err := a.Open(nil, data[:a.NonceSize()], data[a.NonceSize():], []byte("dst-ai-provider"))
	if err != nil {
		return v, "", ErrUnavailable
	}
	return v, string(plain), nil
}
func (s *Store) SaveConfig(input ConfigInput) (Config, error) {
	if err := validateConfig(input); err != nil {
		return Config{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, v, err := s.config()
	if err != nil {
		return v, err
	}
	if v.Revision != input.Revision {
		return v, ErrConflict
	}
	if input.ClearKey {
		r.Secret = ""
	}
	if input.APIKey != "" {
		a, err := s.aead(true)
		if err != nil {
			return v, err
		}
		nonce := make([]byte, a.NonceSize())
		if _, err = rand.Read(nonce); err != nil {
			return v, err
		}
		r.Secret = base64.StdEncoding.EncodeToString(a.Seal(nonce, nonce, []byte(input.APIKey), []byte("dst-ai-provider")))
	}
	limits := input.Limits
	if limits == nil {
		limits = v.Limits
	}
	v = input.Config
	v.Limits = limits
	v.Revision = uuid.NewString()
	v.KeyConfigured = r.Secret != ""
	raw, _ := json.Marshal(v)
	r.ID = 1
	r.Data = string(raw)
	if err = s.db.Table(s.configTable).Save(&r).Error; err != nil {
		return Config{}, err
	}
	return v, nil
}
func (s *Store) Tokens() ([]Token, error) {
	var rows []tokenRecord
	if err := s.db.Table(s.tokenTable).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]Token, 0, len(rows))
	for _, r := range rows {
		var t Token
		if err := json.Unmarshal([]byte(r.Data), &t); err != nil {
			return nil, err
		}
		values = append(values, t)
	}
	return values, nil
}
func (s *Store) CreateToken(input TokenInput) (Token, string, error) {
	if err := normalizeTokenScope(&input); err != nil {
		return Token{}, "", err
	}
	if strings.TrimSpace(input.Name) == "" || len(input.Name) > 100 || input.Days < 1 || input.Days > 365 {
		return Token{}, "", ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	if err := s.db.Table(s.tokenTable).Count(&count).Error; err != nil {
		return Token{}, "", err
	}
	if count >= 32 {
		return Token{}, "", ErrBusy
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Token{}, "", err
	}
	secret := "dstai_" + base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	t := Token{Profile: input.Profile, RoomIDs: input.RoomIDs, ID: uuid.NewString(), Name: strings.TrimSpace(input.Name), CreatedAt: now, ExpiresAt: now.Add(time.Duration(input.Days) * 24 * time.Hour)}
	data, _ := json.Marshal(t)
	err := s.db.Table(s.tokenTable).Create(&tokenRecord{ID: t.ID, Digest: digest(secret), Data: string(data), AdminID: input.AdminID}).Error
	return t, secret, err
}
func (s *Store) token(where string, value any) (Principal, error) {
	var row tokenRecord
	if err := s.db.Table(s.tokenTable).Where(where, value).First(&row).Error; err != nil {
		return Principal{}, ErrDenied
	}
	var t Token
	if json.Unmarshal([]byte(row.Data), &t) != nil || !t.ExpiresAt.After(time.Now()) {
		return Principal{}, ErrDenied
	}
	scope := TokenInput{Profile: t.Profile, RoomIDs: t.RoomIDs}
	if normalizeTokenScope(&scope) != nil {
		return Principal{}, ErrDenied
	}
	return Principal{ID: t.ID, AdminID: row.AdminID, Profile: scope.Profile, RoomIDs: scope.RoomIDs}, nil
}
func (s *Store) Authenticate(secret string) (Principal, error) {
	if !strings.HasPrefix(secret, "dstai_") || len(secret) > 100 {
		return Principal{}, ErrDenied
	}
	return s.token("digest = ?", digest(secret))
}
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Table(s.tokenTable).Where("id = ?", id).Delete(&tokenRecord{}).Error
}
func (s *Store) History() ([]Call, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []Call
	err := s.db.Table(s.callTable).Select("id, source, tool, room_id, arguments, status, created_at, CASE WHEN result IS NOT NULL AND result <> '' THEN 1 ELSE 0 END AS result_available").Order("created_at DESC").Limit(100).Scan(&items).Error
	return items, err
}
func (s *Store) GetCall(id string) (Call, error) {
	if len(id) > 64 || id == "" {
		return Call{}, ErrInvalid
	}
	return s.call(id)
}
func (s *Store) call(id string) (Call, error) {
	var c Call
	err := s.db.Table(s.callTable).Where("id = ?", id).First(&c).Error
	return c, err
}
func (s *Store) prune() error {
	var count int
	if err := s.db.Table(s.callTable).Where("status <> ? AND tool NOT IN (?)", "executing", []string{"bot.read", "bot.write"}).Count(&count).Error; err != nil {
		return err
	}
	if count <= 2000 {
		return nil
	}
	var rows []struct{ ID string }
	// Trim a batch so normal requests use an indexed count rather than sorting
	// the entire retained history on every tool call.
	if err := s.db.Table(s.callTable).Select("id").Where("status <> ? AND tool NOT IN (?)", "executing", []string{"bot.read", "bot.write"}).Order("created_at ASC").Limit(100).Scan(&rows).Error; err != nil {
		return err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	return s.db.Table(s.callTable).Where("id IN (?)", ids).Delete(&Call{}).Error
}

// Demand-driven compaction preserves stable identity/fingerprint and any active
// or uncertain receipt. Capacity exhaustion refuses new work; it never makes an
// old write executable again. No background database sweep is introduced.
func (s *Store) compactBotRecords() error {
	var calls []Call
	if e := s.db.Table(s.callTable).Where("tool IN (?) AND status = ? AND created_at < ?", []string{"bot.read", "bot.write"}, "succeeded", time.Now().UTC().Add(-7*24*time.Hour)).Order("created_at ASC").Limit(32).Find(&calls).Error; e != nil {
		return e
	}
	for _, call := range calls {
		var result BotResult
		if json.Unmarshal([]byte(call.Result), &result) != nil || result.NextAction != "none" || (result.Status != "succeeded" && result.Status != "partial" && result.Status != "failed" && result.Status != "rejected") {
			continue
		}
		result.Data = nil
		result.Complete = false
		result.Status = "unavailable"
		result.NextAction = "none"
		result.Error = map[string]string{"code": "BOT_RESULT_EXPIRED", "message": "结果正文已超过保留期；请求身份仍保留，不会重新执行"}
		raw, _ := json.Marshal(result)
		if e := s.db.Table(s.callTable).Where("id = ?", call.ID).Updates(map[string]any{"result": string(raw), "status": "archived"}).Error; e != nil {
			return e
		}
	}
	return nil
}
