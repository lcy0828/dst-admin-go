package authn

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
	"golang.org/x/crypto/bcrypt"
)

const (
	SessionCookieName = "dst_admin_session"
	ContextAdminKey   = "dst_admin.admin"
	ContextSessionKey = "dst_admin.session"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrSetupComplete      = errors.New("setup is already complete")
	ErrSetupRequired      = errors.New("initial setup is required")
	ErrInvalidSession     = errors.New("invalid session")
	ErrWeakPassword       = errors.New("password must contain at least 12 characters")
	ErrPasswordTooLong    = errors.New("password must not exceed 72 bytes")
	ErrPasswordUnchanged  = errors.New("new password must differ from current password")
)

type Admin struct {
	ID       int    `gorm:"primary_key" json:"id"`
	Username string `gorm:"type:varchar(128);unique_index;not null" json:"username"`
	Password string `gorm:"column:password;type:varchar(255);not null" json:"-"`
}

func (Admin) TableName() string { return "auth" }

type Session struct {
	ID        int       `gorm:"primary_key" json:"-"`
	TokenHash string    `gorm:"type:char(64);unique_index;not null" json:"-"`
	CSRFToken string    `gorm:"type:varchar(64);not null" json:"-"`
	AdminID   int       `gorm:"index;not null" json:"-"`
	ExpiresAt time.Time `gorm:"index;not null" json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
	IPAddress string    `gorm:"type:varchar(64)" json:"-"`
	UserAgent string    `gorm:"type:varchar(512)" json:"-"`
}

func (Session) TableName() string { return "admin_session" }

type AuthenticatedSession struct {
	Admin     Admin
	Session   Session
	CSRFToken string
}

type Service struct {
	db           *gorm.DB
	now          func() time.Time
	sessionTTL   time.Duration
	bcryptCost   int
	adminTable   string
	sessionTable string
}

func NewService(db *gorm.DB) *Service {
	return &Service{
		db:           db,
		now:          time.Now,
		sessionTTL:   24 * time.Hour,
		bcryptCost:   bcrypt.DefaultCost,
		adminTable:   "auth",
		sessionTable: "admin_session",
	}
}

var tablePrefixPattern = regexp.MustCompile(`^[A-Za-z0-9_]*$`)

func NewServiceWithTablePrefix(db *gorm.DB, prefix string) (*Service, error) {
	if !tablePrefixPattern.MatchString(prefix) {
		return nil, fmt.Errorf("invalid database table prefix %q", prefix)
	}
	service := NewService(db)
	service.adminTable = prefix + "auth"
	service.sessionTable = prefix + "admin_session"
	return service, nil
}

func (s *Service) Migrate() error {
	if s.db == nil {
		return errors.New("auth database is nil")
	}
	if err := s.admins().AutoMigrate(&Admin{}).Error; err != nil {
		return fmt.Errorf("migrate admin table: %w", err)
	}
	if err := s.sessions().AutoMigrate(&Session{}).Error; err != nil {
		return fmt.Errorf("migrate auth tables: %w", err)
	}

	var admins []Admin
	if err := s.admins().Find(&admins).Error; err != nil {
		return fmt.Errorf("load admins for password migration: %w", err)
	}
	if len(admins) > 1 {
		return fmt.Errorf("single-admin mode requires at most one auth record, found %d", len(admins))
	}
	for _, admin := range admins {
		if _, err := bcrypt.Cost([]byte(admin.Password)); err == nil {
			continue
		}
		if admin.Password == "" {
			return fmt.Errorf("admin %q has an empty legacy password", admin.Username)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(admin.Password), s.bcryptCost)
		if err != nil {
			return fmt.Errorf("hash legacy password for %q: %w", admin.Username, err)
		}
		if err := s.admins().Where("id = ?", admin.ID).
			UpdateColumn("password", string(hash)).Error; err != nil {
			return fmt.Errorf("store migrated password for %q: %w", admin.Username, err)
		}
	}
	return s.deleteExpiredSessions()
}

func (s *Service) SetupRequired() (bool, error) {
	var count int
	if err := s.admins().Count(&count).Error; err != nil {
		return false, err
	}
	return count == 0, nil
}

func (s *Service) Setup(username, password, ipAddress, userAgent string) (*AuthenticatedSession, string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, "", ErrInvalidCredentials
	}
	if err := checkNewPassword(password); err != nil {
		return nil, "", err
	}

	tx := s.db.Begin()
	if tx.Error != nil {
		return nil, "", tx.Error
	}
	defer func() {
		if recoverValue := recover(); recoverValue != nil {
			tx.Rollback()
			panic(recoverValue)
		}
	}()

	var count int
	if err := tx.Table(s.adminTable).Count(&count).Error; err != nil {
		tx.Rollback()
		return nil, "", err
	}
	if count != 0 {
		tx.Rollback()
		return nil, "", ErrSetupComplete
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.bcryptCost)
	if err != nil {
		tx.Rollback()
		return nil, "", err
	}
	// A fixed primary key makes concurrent first-run setup attempts converge on
	// one database-enforced winner, even when they use different usernames.
	admin := Admin{ID: 1, Username: username, Password: string(hash)}
	if err := tx.Table(s.adminTable).Create(&admin).Error; err != nil {
		tx.Rollback()
		return nil, "", err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, "", err
	}
	return s.createSession(admin, ipAddress, userAgent)
}

func (s *Service) Login(username, password, ipAddress, userAgent string) (*AuthenticatedSession, string, error) {
	required, err := s.SetupRequired()
	if err != nil {
		return nil, "", err
	}
	if required {
		return nil, "", ErrSetupRequired
	}

	var admin Admin
	if err := s.admins().Where("username = ?", strings.TrimSpace(username)).First(&admin).Error; err != nil {
		if gorm.IsRecordNotFoundError(err) {
			return nil, "", ErrInvalidCredentials
		}
		return nil, "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(admin.Password), []byte(password)) != nil {
		return nil, "", ErrInvalidCredentials
	}
	return s.createSession(admin, ipAddress, userAgent)
}

func (s *Service) Authenticate(rawToken string) (*AuthenticatedSession, error) {
	if rawToken == "" {
		return nil, ErrInvalidSession
	}
	var session Session
	if err := s.sessions().Where("token_hash = ? AND expires_at > ?", digest(rawToken), s.now()).First(&session).Error; err != nil {
		if gorm.IsRecordNotFoundError(err) {
			return nil, ErrInvalidSession
		}
		return nil, err
	}
	var admin Admin
	if err := s.admins().First(&admin, session.AdminID).Error; err != nil {
		return nil, ErrInvalidSession
	}
	return &AuthenticatedSession{Admin: admin, Session: session}, nil
}

func (s *Service) CSRFMatches(session Session, csrfToken string) bool {
	return subtle.ConstantTimeCompare([]byte(session.CSRFToken), []byte(csrfToken)) == 1
}

func (s *Service) Logout(rawToken string) error {
	if rawToken == "" {
		return nil
	}
	return s.sessions().Where("token_hash = ?", digest(rawToken)).Delete(&Session{}).Error
}

func (s *Service) ChangePassword(adminID int, currentPassword, newPassword string) error {
	if err := checkNewPassword(newPassword); err != nil {
		return err
	}
	var admin Admin
	if err := s.admins().First(&admin, adminID).Error; err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(admin.Password), []byte(currentPassword)) != nil {
		return ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(admin.Password), []byte(newPassword)) == nil {
		return ErrPasswordUnchanged
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), s.bcryptCost)
	if err != nil {
		return err
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	if err := tx.Table(s.adminTable).Where("id = ?", adminID).UpdateColumn("password", string(hash)).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Table(s.sessionTable).Where("admin_id = ?", adminID).Delete(&Session{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit().Error
}

func (s *Service) createSession(admin Admin, ipAddress, userAgent string) (*AuthenticatedSession, string, error) {
	rawToken, err := secureToken()
	if err != nil {
		return nil, "", err
	}
	csrfToken, err := secureToken()
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	session := Session{
		TokenHash: digest(rawToken),
		CSRFToken: csrfToken,
		AdminID:   admin.ID,
		ExpiresAt: now.Add(s.sessionTTL),
		CreatedAt: now,
		IPAddress: truncate(ipAddress, 64),
		UserAgent: truncate(userAgent, 512),
	}
	if err := s.sessions().Create(&session).Error; err != nil {
		return nil, "", err
	}
	return &AuthenticatedSession{Admin: admin, Session: session, CSRFToken: csrfToken}, rawToken, nil
}

func (s *Service) deleteExpiredSessions() error {
	return s.sessions().Where("expires_at <= ?", s.now()).Delete(&Session{}).Error
}

func (s *Service) admins() *gorm.DB {
	return s.db.Table(s.adminTable)
}

func (s *Service) sessions() *gorm.DB {
	return s.db.Table(s.sessionTable)
}

func checkNewPassword(password string) error {
	if len([]rune(password)) < 12 {
		return ErrWeakPassword
	}
	if len([]byte(password)) > 72 {
		return ErrPasswordTooLong
	}
	return nil
}

func secureToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
