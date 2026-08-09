package announcements

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const maxContentRunes = 10000

type Service struct {
	store *Store
	now   func() time.Time
}

func NewService(store *Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("announcement store is required")
	}
	return &Service{store: store, now: time.Now}, nil
}

func (s *Service) List() ([]Announcement, error) {
	items, err := s.store.List()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	for index := range items {
		items[index] = withStatus(items[index], now)
	}
	return items, nil
}

func (s *Service) Get(id uint64) (Announcement, error) {
	value, err := s.store.Get(id)
	if err != nil {
		return Announcement{}, err
	}
	return withStatus(value, s.now().UTC()), nil
}

func (s *Service) Create(input Input) (Announcement, error) {
	now := s.now().UTC()
	input, err := normalizeInput(input, now)
	if err != nil {
		return Announcement{}, err
	}
	created, err := s.store.Create(Announcement{
		Title: input.Title, Content: input.Content, PublishTime: now, ExpireTime: input.ExpireTime,
		Target: input.Target, Important: input.Important, UpdatedAt: now,
	})
	if err != nil {
		return Announcement{}, err
	}
	return withStatus(created, now), nil
}

func (s *Service) Update(id uint64, input Input) (Announcement, error) {
	now := s.now().UTC()
	input, err := normalizeInput(input, now)
	if err != nil {
		return Announcement{}, err
	}
	current, err := s.store.Get(id)
	if err != nil {
		return Announcement{}, err
	}
	current.Title, current.Content, current.ExpireTime = input.Title, input.Content, input.ExpireTime
	current.Target, current.Important, current.UpdatedAt = input.Target, input.Important, now
	updated, err := s.store.Update(current)
	if err != nil {
		return Announcement{}, err
	}
	return withStatus(updated, now), nil
}

func (s *Service) Delete(id uint64) error {
	return s.store.Delete(id)
}

func normalizeInput(input Input, now time.Time) (Input, error) {
	input.Title = strings.TrimSpace(input.Title)
	input.Content = strings.TrimSpace(input.Content)
	input.ExpireTime = input.ExpireTime.UTC()
	fields := make(map[string]string)
	if length := utf8.RuneCountInString(input.Title); !utf8.ValidString(input.Title) || length < 2 || length > 50 {
		fields["title"] = "标题长度必须在 2 到 50 个字符之间"
	}
	if input.Content == "" || !utf8.ValidString(input.Content) || utf8.RuneCountInString(input.Content) > maxContentRunes || strings.ContainsRune(input.Content, '\x00') {
		fields["content"] = "内容必须是 1 到 10000 个字符的有效文本"
	}
	if input.Target != TargetAll && input.Target != TargetOnline && input.Target != TargetAdmins {
		fields["target"] = "发送对象必须是 all、online 或 admins"
	}
	if input.ExpireTime.IsZero() || !input.ExpireTime.After(now) {
		fields["expireTime"] = "过期时间必须晚于当前时间"
	}
	if len(fields) > 0 {
		return Input{}, &FieldError{Fields: fields}
	}
	return input, nil
}

func withStatus(value Announcement, now time.Time) Announcement {
	value.Status = StatusExpired
	if value.ExpireTime.After(now) {
		value.Status = StatusActive
	}
	return value
}
