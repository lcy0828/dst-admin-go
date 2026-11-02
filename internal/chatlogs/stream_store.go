package chatlogs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

const (
	streamMaxRecords = 2048
	streamMaxBytes   = 4 << 20
	streamMaxPayload = 64 << 10
	streamPageSize   = 16
)

var (
	ErrInvalidStreamCursor = errors.New("invalid room chat stream cursor")
	ErrStreamUnavailable   = errors.New("persistent chat streaming is unavailable")
	ErrStreamBusy          = errors.New("chat stream subscription limit reached")
)

// The journal and its checkpoint commit in the same transaction as chat data.
// Event IDs stay stable; a second shard adds an update, never another creation.
type changeRecord struct {
	RoomID   string `gorm:"primary_key;type:varchar(255)"`
	Sequence int64  `gorm:"primary_key;AUTO_INCREMENT:false"`
	Payload  string `gorm:"type:text"`
	Size     int64
}
type streamRecord struct {
	RoomID string `gorm:"primary_key;type:varchar(255)"`
	Epoch  string
	Head   int64
	Floor  int64
	Bytes  int64
}

type ChatChange struct {
	ID         string    `json:"id"`
	Operation  string    `json:"operation"`
	Kind       Kind      `json:"kind"`
	RecordedAt time.Time `json:"recordedAt"`
	Historical bool      `json:"historical"`
	Truncated  bool      `json:"truncated,omitempty"`
	Entry      *Entry    `json:"entry,omitempty"`
}
type StreamItem struct {
	Cursor string
	Change ChatChange
}
type StreamWindow struct {
	Cursor string
	Signal string
	Items  []StreamItem
	More   bool
}
type chatCursor struct {
	epoch, scope string
	sequence     int64
}

func cursorScope(roomID string, kind Kind) string {
	sum := sha256.Sum256([]byte(roomID + "\x00" + string(kind)))
	return hex.EncodeToString(sum[:12])
}
func encodeChatCursor(state streamRecord, scope string, seq int64) string {
	return "c1." + scope + "." + state.Epoch + "." + strconv.FormatInt(seq, 10)
}
func parseChatCursor(value, scope string) (chatCursor, error) {
	parts := strings.Split(value, ".")
	if len(value) > 160 || len(parts) != 4 || parts[0] != "c1" || parts[1] != scope {
		return chatCursor{}, ErrInvalidStreamCursor
	}
	if _, err := uuid.Parse(parts[2]); err != nil {
		return chatCursor{}, ErrInvalidStreamCursor
	}
	n, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != parts[3] {
		return chatCursor{}, ErrInvalidStreamCursor
	}
	return chatCursor{epoch: parts[2], scope: scope, sequence: n}, nil
}

func (s *Store) changed(roomID string) {
	if s.notify != nil {
		s.notify(roomID)
	}
}

func (s *Store) appendChange(tx *gorm.DB, eventID, operation string, historical bool) error {
	var event eventRecord
	if err := tx.Table(s.eventsTable).Where("id = ?", eventID).First(&event).Error; err != nil {
		return err
	}
	change := ChatChange{ID: event.ID, Operation: operation, Kind: Kind(event.Kind), RecordedAt: time.Now().UTC(), Historical: historical}
	if operation != "deleted" {
		entries, err := s.entriesFrom(tx, []eventRecord{event})
		if err != nil {
			return err
		}
		change.Entry = &entries[0]
	}
	data, err := json.Marshal(change)
	if err != nil {
		return err
	}
	if len(data) > streamMaxPayload && change.Entry != nil {
		// Extremely large input must not abort ordinary history ingestion or create
		// unbounded SSE frames. The stable ID remains available in normal history.
		entry := change.Entry
		if len(entry.Content) > 8192 {
			entry.Content = validPrefix(entry.Content, 8192)
		}
		if len(entry.PlayerName) > 512 {
			entry.PlayerName = validPrefix(entry.PlayerName, 512)
		}
		if len(entry.PlayerID) > 512 {
			entry.PlayerID = validPrefix(entry.PlayerID, 512)
		}
		if len(entry.Sources) > 16 {
			entry.Sources = entry.Sources[:16]
		}
		change.Truncated = true
		data, err = json.Marshal(change)
		if err != nil {
			return err
		}
		if len(data) > streamMaxPayload {
			change.Entry = nil
			data, err = json.Marshal(change)
			if err != nil {
				return err
			}
		}
	}
	var state streamRecord
	err = tx.Table(s.streamsTable).Where("room_id = ?", event.RoomID).First(&state).Error
	if gorm.IsRecordNotFoundError(err) {
		state = streamRecord{RoomID: event.RoomID, Epoch: uuid.NewString()}
		if err = tx.Table(s.streamsTable).Create(&state).Error; err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	previousHead := state.Head
	state.Head++
	row := changeRecord{RoomID: event.RoomID, Sequence: state.Head, Payload: string(data), Size: int64(len(data)) + 128}
	if err = tx.Table(s.changesTable).Create(&row).Error; err != nil {
		return err
	}
	state.Bytes += row.Size
	for state.Head-state.Floor > streamMaxRecords || state.Bytes > streamMaxBytes {
		var oldest []changeRecord
		if err = tx.Table(s.changesTable).Where("room_id = ? AND sequence > ?", event.RoomID, state.Floor).Order("sequence ASC").Limit(128).Find(&oldest).Error; err != nil {
			return err
		}
		if len(oldest) == 0 {
			return errors.New("chat stream retention checkpoint is inconsistent")
		}
		for _, old := range oldest {
			if state.Head-state.Floor <= streamMaxRecords && state.Bytes <= streamMaxBytes {
				break
			}
			state.Floor = old.Sequence
			state.Bytes -= old.Size
		}
		if err = tx.Table(s.changesTable).Where("room_id = ? AND sequence <= ?", event.RoomID, state.Floor).Delete(&changeRecord{}).Error; err != nil {
			return err
		}
	}
	result := tx.Table(s.streamsTable).Where("room_id = ? AND head = ?", state.RoomID, previousHead).Updates(map[string]interface{}{"head": state.Head, "floor": state.Floor, "bytes": state.Bytes})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("concurrent chat stream checkpoint changed")
	}
	return nil
}
func validPrefix(value string, limit int) string {
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

// Opening a stream never backfills old history into the journal. Its epoch is
// persisted so reconnects survive controller restarts without replaying a tail.
func (s *Store) streamState(roomID string) (streamRecord, error) {
	var state streamRecord
	err := s.db.Table(s.streamsTable).Where("room_id = ?", roomID).First(&state).Error
	if !gorm.IsRecordNotFoundError(err) {
		return state, err
	}
	state = streamRecord{RoomID: roomID, Epoch: uuid.NewString()}
	if err = s.db.Table(s.streamsTable).Create(&state).Error; err == nil {
		return state, nil
	}
	// A simultaneous importer or first subscriber may have created it.
	err = s.db.Table(s.streamsTable).Where("room_id = ?", roomID).First(&state).Error
	return state, err
}

func (s *Store) StreamWindow(roomID string, kind Kind, value string) (StreamWindow, error) {
	scope := cursorScope(roomID, kind)
	var cursor chatCursor
	var err error
	if value != "" {
		cursor, err = parseChatCursor(value, scope)
		if err != nil {
			return StreamWindow{}, err
		}
	}
	if _, err = s.streamState(roomID); err != nil {
		return StreamWindow{}, err
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return StreamWindow{}, tx.Error
	}
	defer tx.Rollback()
	var state streamRecord
	if err = tx.Table(s.streamsTable).Where("room_id = ?", roomID).First(&state).Error; err != nil {
		return StreamWindow{}, err
	}
	window := StreamWindow{Cursor: encodeChatCursor(state, scope, state.Head), Items: []StreamItem{}}
	if value == "" {
		window.Signal = "chat.cursor"
		return window, nil
	}
	if cursor.epoch != state.Epoch || cursor.sequence > state.Head {
		window.Signal = "chat.reset"
		return window, nil
	}
	if cursor.sequence < state.Floor {
		window.Signal = "chat.gap"
		window.Cursor = encodeChatCursor(state, scope, state.Floor)
		window.More = true
		return window, nil
	}
	var records []changeRecord
	if err = tx.Table(s.changesTable).Where("room_id = ? AND sequence > ? AND sequence <= ?", roomID, cursor.sequence, state.Head).Order("sequence ASC").Limit(streamPageSize).Find(&records).Error; err != nil {
		return StreamWindow{}, err
	}
	last := cursor.sequence
	for _, record := range records {
		if record.Sequence != last+1 {
			return StreamWindow{}, errors.New("chat stream journal is incomplete")
		}
		var change ChatChange
		if err = json.Unmarshal([]byte(record.Payload), &change); err != nil {
			return StreamWindow{}, err
		}
		last = record.Sequence
		next := encodeChatCursor(state, scope, last)
		if kind == "" || change.Kind == kind {
			window.Items = append(window.Items, StreamItem{Cursor: next, Change: change})
		}
	}
	window.Cursor = encodeChatCursor(state, scope, last)
	window.More = last < state.Head
	if window.More && len(records) == 0 {
		return StreamWindow{}, errors.New("chat stream journal is incomplete")
	}
	return window, nil
}
