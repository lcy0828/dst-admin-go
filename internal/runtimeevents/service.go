package runtimeevents

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"dont/internal/dstruntime"
)

var ErrInvalidCursor = errors.New("runtime event cursor is invalid")

type Source interface {
	ReadEvents(context.Context, string, string) (dstruntime.EventBatch, error)
}

type Cursor struct {
	ProducerInstanceID string `json:"producerInstanceId"`
	Sequence           int64  `json:"sequence"`
}

type Window struct {
	Cursor        Cursor                    `json:"cursor"`
	EncodedCursor string                    `json:"encodedCursor"`
	FirstSequence int64                     `json:"firstSequence"`
	LastSequence  int64                     `json:"lastSequence"`
	Reset         bool                      `json:"reset"`
	Gap           bool                      `json:"gap"`
	Events        []dstruntime.RuntimeEvent `json:"events"`
}

type Service struct{ source Source }

func New(source Source) (*Service, error) {
	if source == nil {
		return nil, errors.New("runtime event source is required")
	}
	return &Service{source: source}, nil
}

func (s *Service) Window(ctx context.Context, roomID, worldID string, cursor Cursor, resume bool) (Window, error) {
	batch, err := s.source.ReadEvents(ctx, roomID, worldID)
	if err != nil {
		return Window{}, err
	}
	window := Window{
		Cursor:        Cursor{ProducerInstanceID: batch.ProducerInstanceID, Sequence: batch.LastSequence},
		FirstSequence: batch.FirstSequence, LastSequence: batch.LastSequence,
	}
	window.EncodedCursor = Encode(window.Cursor)
	if !resume {
		window.Events = []dstruntime.RuntimeEvent{}
		return window, nil
	}
	if cursor.ProducerInstanceID != batch.ProducerInstanceID || cursor.Sequence > batch.LastSequence {
		window.Reset = true
		window.Events = append([]dstruntime.RuntimeEvent(nil), batch.Events...)
		return window, nil
	}
	if cursor.Sequence < batch.FirstSequence-1 {
		window.Gap = true
		window.Events = append([]dstruntime.RuntimeEvent(nil), batch.Events...)
		return window, nil
	}
	window.Events = make([]dstruntime.RuntimeEvent, 0, len(batch.Events))
	for _, event := range batch.Events {
		if event.Sequence > cursor.Sequence {
			window.Events = append(window.Events, event)
		}
	}
	return window, nil
}

func Encode(cursor Cursor) string {
	if strings.TrimSpace(cursor.ProducerInstanceID) == "" || cursor.Sequence < 0 {
		return ""
	}
	producer := base64.RawURLEncoding.EncodeToString([]byte(cursor.ProducerInstanceID))
	return producer + "." + strconv.FormatInt(cursor.Sequence, 10)
}

func Parse(value string) (Cursor, error) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ".")
	if value == "" || len(parts) != 2 || len(parts[0]) > 256 || len(parts[1]) > 20 {
		return Cursor{}, ErrInvalidCursor
	}
	producer, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(producer) < 1 || len(producer) > 128 || strings.ContainsRune(string(producer), '\x00') {
		return Cursor{}, ErrInvalidCursor
	}
	sequence, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || sequence < 0 {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{ProducerInstanceID: string(producer), Sequence: sequence}, nil
}

func EventCursor(producerInstanceID string, sequence int64) (string, error) {
	value := Encode(Cursor{ProducerInstanceID: producerInstanceID, Sequence: sequence})
	if value == "" {
		return "", fmt.Errorf("%w: event identity is empty", ErrInvalidCursor)
	}
	return value, nil
}
