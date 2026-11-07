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

func TestInspectLogTailDetectsAndClearsSnapshotBarrierFailure(t *testing.T) {
	echoedOnly := `[00:25:15]: RemoteCommandInput: "print(\"[DST-ADMIN-RUNTIME ERROR] code=SNAPSHOT_BARRIER_INVALID_STATE barrier=123 message=barrier_not_holding\")"` + "\n"
	if incident := InspectLogTail([]byte(echoedOnly)); incident != nil {
		t.Fatalf("echoed RemoteCommandInput classified as incident: %#v", incident)
	}

	failedLog := echoedOnly + `[00:25:15]: [DST-ADMIN-RUNTIME ERROR] code=SNAPSHOT_BARRIER_INVALID_STATE barrier=123 message=barrier_not_holding` + "\n"
	incident := InspectLogTail([]byte(failedLog))
	if incident == nil || incident.Code != SnapshotBarrierFailedCode || !strings.Contains(incident.Message, "一键重启") {
		t.Fatalf("expected snapshot barrier incident, got %#v", incident)
	}

	for _, recoveryLine := range []string{
		"Sim unpaused\n",
		"Serializing world: 57\n",
		"Starting Up\n",
		"[DST-ADMIN-RUNTIME] snapshot barrier recovered\n",
	} {
		if cleared := InspectLogTail([]byte(failedLog + recoveryLine)); cleared != nil {
			t.Fatalf("recovery line %q did not clear barrier incident: %#v", recoveryLine, cleared)
		}
	}
}

