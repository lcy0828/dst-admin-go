package savehealth

import (
	"strings"
	"testing"
)

func TestInspectLogTailReportsOnlyUnresolvedSaveFailure(t *testing.T) {
	incident := InspectLogTail([]byte("Serializing world: 55\n[CRITICAL] Failed to save file /save/session\n"))
	if incident == nil || incident.Code != SaveWriteFailedCode || !strings.Contains(incident.Message, "可能回档") {
		t.Fatalf("incident=%#v", incident)
	}
	if incident := InspectLogTail([]byte("[CRITICAL] Failed to save file /save/session\nSerializing world: 56\n")); incident != nil {
		t.Fatalf("later serialization did not clear incident: %#v", incident)
	}
	if incident := InspectLogTail([]byte("Serializing world: 56\n")); incident != nil {
		t.Fatalf("healthy log classified as incident: %#v", incident)
	}
}
