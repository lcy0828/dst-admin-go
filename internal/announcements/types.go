package announcements

import (
	"errors"
	"time"
)

var (
	ErrNotFound     = errors.New("announcement not found")
	ErrInvalidInput = errors.New("announcement input is invalid")
)

type Target string

const (
	TargetAll     Target = "all"
	TargetOnline  Target = "online"
	TargetAdmins  Target = "admins"
	StatusActive         = "active"
	StatusExpired        = "expired"
)

type FieldError struct {
	Fields map[string]string
}

func (e *FieldError) Error() string { return ErrInvalidInput.Error() }
func (e *FieldError) Unwrap() error { return ErrInvalidInput }

type Announcement struct {
	ID          uint64    `json:"id"`
	Title       string    `json:"title"`
	Content     string    `json:"content"`
	PublishTime time.Time `json:"publishTime"`
	ExpireTime  time.Time `json:"expireTime"`
	Target      Target    `json:"target"`
	Important   bool      `json:"important"`
	Status      string    `json:"status"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type Input struct {
	Title      string    `json:"title"`
	Content    string    `json:"content"`
	ExpireTime time.Time `json:"expireTime"`
	Target     Target    `json:"target"`
	Important  bool      `json:"important"`
}
