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
		"Starting Up\n",
		"[DST-ADMIN-RUNTIME] snapshot barrier recovered id=123\n",
	} {
		if cleared := InspectLogTail([]byte(failedLog + recoveryLine)); cleared != nil {
			t.Fatalf("recovery line %q did not clear barrier incident: %#v", recoveryLine, cleared)
		}
	}
}

func TestBarrierRecoveryRequiresActualOutputFromTheSameAttempt(t *testing.T) {
	failure := "[00:10:00]: [DST-ADMIN-RUNTIME ERROR] code=SNAPSHOT_BARRIER_Release_FAILED id=hot-current\n"
	for _, line := range []string{
		`[00:10:01]: RemoteCommandInput: "print(\"[DST-ADMIN-RUNTIME] snapshot barrier recovered id=hot-current\")"`,
		`[00:10:01]: local r=rawget(_G,"DSTAdmin"); print("[DST-ADMIN-RUNTIME] snapshot barrier recovered id=hot-current")`,
		`[00:10:01]: RemoteCommandInput: "print(\"Sim unpaused\")"`,
		`[00:10:01]: local s="Starting Up"`,
		"[00:10:01]: Serializing world: 57",
		"[00:10:01]: [DST-ADMIN-RUNTIME] snapshot barrier recovered id=hot-stale",
		"[00:10:01]: [DST-ADMIN-RUNTIME] snapshot barrier recovered",
		"[00:10:01]: [DST-ADMIN-RUNTIME ERROR] code=SNAPSHOT_BARRIER_RECOVERY_UNCONFIRMED id=hot-current",
	} {
		t.Run(line, func(t *testing.T) {
			if incident := InspectLogTail([]byte(failure + line + "\n")); incident == nil || incident.Code != SnapshotBarrierFailedCode {
				t.Fatalf("unresolved barrier failure was cleared by %q: %#v", line, incident)
			}
		})
	}
	for _, line := range []string{
		"[00:10:01]: Sim unpaused",
		"[00:10:01]: Starting Up",
		"[00:10:01]: [DST-ADMIN-RUNTIME] snapshot barrier recovered id=hot-current",
	} {
		if incident := InspectLogTail([]byte(failure + line + "\n")); incident != nil {
			t.Fatalf("verified recovery %q did not clear incident: %#v", line, incident)
		}
	}
}
