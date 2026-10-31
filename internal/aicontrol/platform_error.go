package aicontrol

import (
	"encoding/json"
	"fmt"
)

// PlatformError retains the API's structured rejection for fixed recipes.
// It does not imply that an arbitrary failed mutation had no side effects.
type PlatformError struct {
	Status  int
	Code    string
	Message string
	Payload json.RawMessage
}

func (e *PlatformError) Error() string {
	if len(e.Payload) > 0 {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Payload)
	}
	return fmt.Sprintf("HTTP %d: %s: %s", e.Status, e.Code, e.Message)
}
