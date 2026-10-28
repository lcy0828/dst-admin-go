package notifications

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dont/internal/softwareupdate"
	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

type channelRecord struct {
	ID       string `gorm:"primary_key;type:varchar(64)"`
	Data     string `gorm:"type:text;not null"`
	Secret   string `gorm:"type:text"`
	Revision string `gorm:"type:char(36);not null"`
}

type deliveryRecord struct {
	ID            string `gorm:"primary_key;type:varchar(64)"`
	ChannelID     string `gorm:"type:varchar(64);index;not null"`
	ChannelName   string `gorm:"type:varchar(100);not null"`
	EventData     string `gorm:"type:text;not null"`
	Status        string `gorm:"type:varchar(24);index;not null"`
	Attempts      int    `gorm:"not null"`
	ErrorCode     string `gorm:"type:varchar(64)"`
	HTTPStatus    int
	CreatedAt     time.Time `gorm:"index"`
	NextAttemptAt time.Time `gorm:"index"`
	CompletedAt   *time.Time
}

type releaseNoticeRecord struct {
	ID      string `gorm:"primary_key;type:varchar(64)"`
	Version string `gorm:"type:varchar(64);not null"`
}

type Store struct {
	db                                   *gorm.DB
	channelsTable, deliveriesTable, root string
	mu                                   sync.Mutex
}

func NewStore(db *gorm.DB, prefix, root string) *Store {
	return &Store{db: db, channelsTable: strings.TrimSpace(prefix) + "notification_channel", deliveriesTable: strings.TrimSpace(prefix) + "notification_delivery", root: root}
}

func (s *Store) Migrate() error {
	if s.db == nil {
		return errors.New("notification database is required")
	}
	if err := s.db.Table(s.channelsTable).AutoMigrate(&channelRecord{}).Error; err != nil {
		return err
	}
	if err := s.db.Table(s.deliveriesTable).AutoMigrate(&deliveryRecord{}).Error; err != nil {
		return err
	}
	if err := s.db.Table(s.channelsTable + "_event").AutoMigrate(&feedRecord{}).Error; err != nil {
		return err
	}
	return s.db.Table(s.channelsTable + "_release").AutoMigrate(&releaseNoticeRecord{}).Error
}

func (s *Store) Channels() ([]Channel, error) {
	var rows []channelRecord
	if err := s.db.Table(s.channelsTable).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]Channel, 0, len(rows))
	for _, row := range rows {
		var item Channel
		if err := json.Unmarshal([]byte(row.Data), &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) record(id string) (channelRecord, error) {
	var row channelRecord
	err := s.db.Table(s.channelsTable).Where("id = ?", id).First(&row).Error
	if gorm.IsRecordNotFoundError(err) {
		err = ErrNotFound
	}
	return row, err
}

func (s *Store) cipher(create bool) (cipher.AEAD, error) {
	path := filepath.Join(s.root, "webhook.key")
	key, err := os.ReadFile(path)
	if os.IsNotExist(err) && create {
		if err = os.MkdirAll(s.root, 0o700); err != nil {
			return nil, ErrSecrets
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, ErrSecrets
		}
		file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(openErr) {
			key, err = os.ReadFile(path)
		} else if openErr != nil {
			return nil, ErrSecrets
		} else {
			_, err = file.Write(key)
			if err == nil {
				err = file.Sync()
			}
			err = errors.Join(err, file.Close())
		}
	}
	if err != nil || len(key) != 32 {
		return nil, ErrSecrets
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrSecrets
	}
	return cipher.NewGCM(block)
}

func (s *Store) secret(row channelRecord) (credentials, error) {
	if row.Secret == "" {
		return credentials{}, nil
	}
	data, err := base64.StdEncoding.DecodeString(row.Secret)
	if err != nil {
		return credentials{}, ErrSecrets
	}
	aead, err := s.cipher(false)
	if err != nil || len(data) < aead.NonceSize() {
		return credentials{}, ErrSecrets
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(row.ID))
	if err != nil {
		return credentials{}, ErrSecrets
	}
	var value credentials
	if err = json.Unmarshal(plain, &value); err != nil {
		return value, ErrSecrets
	}
	return value, nil
}

func (s *Store) Secret(id string) (credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.record(id)
	if err != nil {
		return credentials{}, err
	}
	return s.secret(row)
}

func (s *Store) Save(id string, input ChannelInput) (Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var row channelRecord
	var secret credentials
	if id != "" {
		var err error
		row, err = s.record(id)
		if err != nil {
			return Channel{}, err
		}
		if input.Revision != row.Revision {
			return Channel{}, ErrConflict
		}
		if input.Type == "webhook" {
			secret, err = s.secret(row)
			if err != nil && !(input.URL != "" && input.Headers != nil) {
				return Channel{}, err
			}
		}
	} else {
		var count int
		if err := s.db.Table(s.channelsTable).Count(&count).Error; err != nil {
			return Channel{}, err
		}
		if count >= 32 {
			return Channel{}, ErrLimit
		}
		id = uuid.NewString()
	}
	if input.URL != "" {
		secret.URL = strings.TrimSpace(input.URL)
	}
	if input.Headers != nil {
		secret.Headers = input.Headers
	}
	if err := validate(&input, secret); err != nil {
		return Channel{}, err
	}
	channel := Channel{ID: id, Revision: uuid.NewString(), Name: input.Name, Type: input.Type, Enabled: input.Enabled, Events: input.Events, FailuresOnly: input.FailuresOnly, Recipients: input.Recipients, Method: input.Method, Format: input.Format, Template: input.Template, ResponseMode: input.ResponseMode, URLConfigured: input.Type == "webhook" && secret.URL != "", HeadersConfigured: input.Type == "webhook" && len(secret.Headers) > 0, UpdatedAt: time.Now().UTC()}
	data, err := json.Marshal(channel)
	if err != nil {
		return Channel{}, err
	}
	encoded := ""
	if input.Type == "webhook" {
		aead, err := s.cipher(true)
		if err != nil {
			return Channel{}, err
		}
		plain, _ := json.Marshal(secret)
		nonce := make([]byte, aead.NonceSize())
		if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
			return Channel{}, err
		}
		encoded = base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, []byte(id)))
	}
	next := channelRecord{ID: id, Data: string(data), Secret: encoded, Revision: channel.Revision}
	if row.ID == "" {
		err = s.db.Table(s.channelsTable).Create(&next).Error
	} else {
		result := s.db.Table(s.channelsTable).Where("id = ? AND revision = ?", id, input.Revision).Updates(map[string]any{"data": next.Data, "secret": next.Secret, "revision": next.Revision})
		err = result.Error
		if err == nil && result.RowsAffected != 1 {
			err = ErrConflict
		}
	}
	return channel, err
}

func (s *Store) Delete(id, revision string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.record(id)
	if err != nil {
		return err
	}
	if revision != row.Revision {
		return ErrConflict
	}
	result := s.db.Table(s.channelsTable).Where("id = ? AND revision = ?", id, revision).Delete(&channelRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) Enqueue(event Event, channels []Channel) ([]Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	if err := s.db.Table(s.deliveriesTable).Where("status IN (?)", []string{"queued", "sending"}).Count(&count).Error; err != nil {
		return nil, err
	}
	if count+len(channels) > 1000 {
		return nil, ErrLimit
	}
	data, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	items := make([]Delivery, 0, len(channels))
	err = s.db.Transaction(func(tx *gorm.DB) error {
		for _, channel := range channels {
			if (event.Kind == "software.available" || event.Kind == "agent.software.available") && event.Version != "" {
				if _, ok := softwareupdate.NormalizeVersion(event.Version); !ok {
					return ErrInvalid
				}
				key := sha256.Sum256([]byte(channel.ID + "\x00" + event.Kind + "\x00" + event.TargetID))
				id := hex.EncodeToString(key[:])
				var notice releaseNoticeRecord
				err := tx.Table(s.channelsTable+"_release").Where("id = ?", id).First(&notice).Error
				if err != nil && !gorm.IsRecordNotFoundError(err) {
					return err
				}
				if err == nil && softwareupdate.CompareVersions(notice.Version, event.Version) >= 0 {
					continue
				}
				notice.ID = id
				notice.Version = event.Version
				if err := tx.Table(s.channelsTable + "_release").Save(&notice).Error; err != nil {
					return err
				}
			}
			hash := sha256.Sum256([]byte(channel.ID + "\x00" + event.ID))
			row := deliveryRecord{ID: hex.EncodeToString(hash[:]), ChannelID: channel.ID, ChannelName: channel.Name, EventData: string(data), Status: "queued", CreatedAt: time.Now().UTC(), NextAttemptAt: time.Now().UTC()}
			if err := tx.Table(s.deliveriesTable).Where("id = ?", row.ID).FirstOrCreate(&row).Error; err != nil {
				return err
			}
			item, err := decodeDelivery(row)
			if err != nil {
				return err
			}
			items = append(items, item)
		}
		return nil
	})
	return items, err
}

func decodeDelivery(row deliveryRecord) (Delivery, error) {
	var event Event
	err := json.Unmarshal([]byte(row.EventData), &event)
	return Delivery{ID: row.ID, ChannelID: row.ChannelID, ChannelName: row.ChannelName, Event: event, Status: row.Status, Attempts: row.Attempts, ErrorCode: row.ErrorCode, HTTPStatus: row.HTTPStatus, CreatedAt: row.CreatedAt, NextAttemptAt: row.NextAttemptAt, CompletedAt: row.CompletedAt}, err
}

func (s *Store) History(limit, offset int) ([]Delivery, int, error) {
	if limit < 1 || limit > 100 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	var count int
	if err := s.db.Table(s.deliveriesTable).Count(&count).Error; err != nil {
		return nil, 0, err
	}
	var rows []deliveryRecord
	if err := s.db.Table(s.deliveriesTable).Order("created_at DESC, id DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	items := make([]Delivery, 0, len(rows))
	for _, row := range rows {
		item, err := decodeDelivery(row)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, count, nil
}

func (s *Store) Next() (Delivery, error) {
	var row deliveryRecord
	err := s.db.Table(s.deliveriesTable).Where("status = ?", "queued").Order("next_attempt_at ASC, created_at ASC").First(&row).Error
	if gorm.IsRecordNotFoundError(err) {
		err = ErrNotFound
	}
	if err != nil {
		return Delivery{}, err
	}
	return decodeDelivery(row)
}

func (s *Store) Begin(id string) (bool, error) {
	result := s.db.Table(s.deliveriesTable).Where("id = ? AND status = ?", id, "queued").Updates(map[string]any{"status": "sending", "attempts": gorm.Expr("attempts + 1")})
	return result.RowsAffected == 1, result.Error
}

func (s *Store) Finish(item Delivery, result SendResult, canceled bool) error {
	status := "sent"
	var completed *time.Time
	now, next := time.Now().UTC(), time.Now().UTC()
	if canceled {
		status = "canceled"
	} else if result.ErrorCode != "" {
		status = "failed"
		if result.Retryable && item.Attempts < 3 {
			status = "queued"
			next = now.Add(time.Duration(item.Attempts*item.Attempts) * 15 * time.Second)
		}
	}
	if status != "queued" {
		completed = &now
	}
	return s.db.Table(s.deliveriesTable).Where("id = ? AND status = ?", item.ID, "sending").Updates(map[string]any{"status": status, "error_code": result.ErrorCode, "http_status": result.HTTPStatus, "next_attempt_at": next, "completed_at": completed}).Error
}

func (s *Store) Recover() error {
	// The recipient may already have accepted an interrupted send. Keep that
	// uncertainty visible; never automatically replay a possibly delivered mail.
	now := time.Now().UTC()
	return s.db.Table(s.deliveriesTable).Where("status = ?", "sending").Updates(map[string]any{"status": "failed", "error_code": "DELIVERY_INTERRUPTED", "completed_at": &now}).Error
}

func (s *Store) Retry(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	if err := s.db.Table(s.deliveriesTable).Where("status IN (?)", []string{"queued", "sending"}).Count(&count).Error; err != nil {
		return err
	}
	if count >= 1000 {
		return ErrLimit
	}
	result := s.db.Table(s.deliveriesTable).Where("id = ? AND status = ?", id, "failed").Updates(map[string]any{"status": "queued", "attempts": 0, "error_code": "", "next_attempt_at": time.Now().UTC(), "completed_at": nil})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) Prune() error {
	var expired []string
	if err := s.db.Table(s.deliveriesTable).Where("status NOT IN (?)", []string{"queued", "sending"}).Order("created_at DESC, id DESC").Offset(2000).Limit(100).Pluck("id", &expired).Error; err != nil {
		return err
	}
	if len(expired) == 0 {
		return nil
	}
	return s.db.Table(s.deliveriesTable).Where("status NOT IN (?) AND id IN (?)", []string{"queued", "sending"}, expired).Delete(&deliveryRecord{}).Error
}
