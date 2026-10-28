package playeractivity

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type segmentCheck struct {
	Segment          Segment
	Events           []Event
	Code, Field      string
	TrustedEnvelope  bool
	Fatal            bool
	InvalidSequences []int64
}

func inspectSegment(data []byte, slot Slot, now time.Time) segmentCheck {
	r := segmentCheck{}
	if len(data) == 0 {
		r.Code = "ACTIVITY_SEGMENT_MISSING"
		return r
	}
	if len(data) > 65536+256 {
		r.Code = "ACTIVITY_SEGMENT_SIZE"
		return r
	}
	// Decode a complete JSON document first. Maps classify future versions
	// without admitting their records; raw events allow independent field errors.
	var head map[string]json.RawMessage
	if err := decode(data, &head); err != nil {
		r.Code = "ACTIVITY_SEGMENT_JSON"
		return r
	}
	var schema int
	if json.Unmarshal(head["schemaVersion"], &schema) != nil {
		r.Code = "ACTIVITY_SEGMENT_JSON"
		r.Field = "schemaVersion"
		return r
	}
	if schema != 1 {
		r.Code = "ACTIVITY_SCHEMA_UNSUPPORTED"
		r.Field = "schemaVersion"
		r.Fatal = true
		return r
	}
	var wire struct {
		SchemaVersion      int               `json:"schemaVersion"`
		ProducerVersion    string            `json:"producerVersion"`
		ProducerInstanceID string            `json:"producerInstanceId"`
		SessionID          string            `json:"sessionId"`
		ShardID            string            `json:"shardId"`
		Generation         int64             `json:"generation"`
		FirstSequence      int64             `json:"firstSequence"`
		LastSequence       int64             `json:"lastSequence"`
		Events             []json.RawMessage `json:"events"`
	}
	if err := decode(data, &wire); err != nil {
		r.Code = "ACTIVITY_SEGMENT_JSON"
		return r
	}
	r.Segment = Segment{SchemaVersion: wire.SchemaVersion, ProducerVersion: wire.ProducerVersion, ProducerInstanceID: wire.ProducerInstanceID, SessionID: wire.SessionID, ShardID: wire.ShardID, Generation: wire.Generation, FirstSequence: wire.FirstSequence, LastSequence: wire.LastSequence}
	// Trust sequence structure only after checking every event, including those
	// whose other fields cannot be decoded. No recovery from truncated fragments.
	for i, raw := range wire.Events {
		var sequence struct {
			Sequence int64 `json:"sequence"`
		}
		if json.Unmarshal(raw, &sequence) != nil {
			r.Code = "ACTIVITY_SEGMENT_SEQUENCE"
			r.Field = fmt.Sprintf("events[%d].sequence", i)
			return r
		}
		r.Segment.Events = append(r.Segment.Events, Event{Sequence: sequence.Sequence})
	}
	v := r.Segment
	if v.SchemaVersion != 1 {
		r.Code = "ACTIVITY_SCHEMA_UNSUPPORTED"
		r.Field = "schemaVersion"
		r.Fatal = true
		return r
	}
	if v.Generation != slot.Generation || v.ProducerInstanceID != slot.ProducerInstanceID || v.SessionID != slot.SessionID || v.ShardID != slot.ShardID || v.FirstSequence != slot.FirstSequence || v.LastSequence != slot.LastSequence {
		r.Code = "ACTIVITY_SEGMENT_METADATA"
		r.Field = "envelope"
		return r
	}
	if len(v.Events) < 1 || len(v.Events) > 128 {
		r.Code = "ACTIVITY_SEGMENT_SEQUENCE"
		r.Field = "events"
		return r
	}
	previous := int64(0)
	for i, e := range v.Events {
		if e.Sequence <= previous || e.Sequence < v.FirstSequence || e.Sequence > v.LastSequence {
			r.Code = "ACTIVITY_SEGMENT_SEQUENCE"
			r.Field = fmt.Sprintf("events[%d].sequence", i)
			return r
		}
		previous = e.Sequence
	}
	if v.Events[0].Sequence != v.FirstSequence || previous != v.LastSequence {
		r.Code = "ACTIVITY_SEGMENT_SEQUENCE"
		r.Field = "sequenceRange"
		return r
	}
	r.TrustedEnvelope = true
	for i, raw := range wire.Events {
		var e Event
		if decode(raw, &e) != nil {
			r.Code = "ACTIVITY_EVENT_INVALID"
			r.InvalidSequences = append(r.InvalidSequences, v.Events[i].Sequence)
			r.Field = fmt.Sprintf("events[%d]", i)
			continue
		}
		// Reuse strict event validation without conflating envelope and field errors.
		single := v
		single.Events = []Event{e}
		single.FirstSequence = e.Sequence
		single.LastSequence = e.Sequence
		expected := slot
		expected.FirstSequence = e.Sequence
		expected.LastSequence = e.Sequence
		if !validSegment(single, expected, now) {
			r.Code = "ACTIVITY_EVENT_INVALID"
			r.InvalidSequences = append(r.InvalidSequences, v.Events[i].Sequence)
			r.Field = fmt.Sprintf("events[%d]", i)
			if e.OccurredAtUnix > now.Add(5*time.Minute).Unix() || e.OccurredAtUnix < 946684800 {
				r.Code = "ACTIVITY_CLOCK_INVALID"
				r.Field += ".occurredAtUnix"
				r.Fatal = true
				return r
			}
			continue
		}
		r.Events = append(r.Events, e)
	}
	return r
}

func contentFingerprint(data []byte) string { return identity(string(data)) }

type activityReadError struct{ code string }

func (e *activityReadError) Error() string { return e.code }
func manifestFromBundleData(data []byte) (Manifest, error) {
	var m Manifest
	var head map[string]json.RawMessage
	if err := decode(data, &head); err != nil {
		return m, &activityReadError{code: "ACTIVITY_MANIFEST_JSON"}
	}
	var version int
	if json.Unmarshal(head["schemaVersion"], &version) == nil && version != 1 {
		return m, &activityReadError{code: "ACTIVITY_SCHEMA_UNSUPPORTED"}
	}
	if err := decode(data, &m); err != nil {
		return m, &activityReadError{code: "ACTIVITY_MANIFEST_JSON"}
	}
	if !validManifest(m) {
		return m, &activityReadError{code: "ACTIVITY_MANIFEST_METADATA"}
	}
	return m, nil
}
func manifestErrorCode(err error) string {
	var classified *activityReadError
	if errors.As(err, &classified) {
		return classified.code
	}
	return "ACTIVITY_READ_FAILED"
}
