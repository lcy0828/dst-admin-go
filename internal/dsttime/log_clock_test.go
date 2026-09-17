package dsttime

import (
	"testing"
	"time"
)

func TestLogClockTracksDailyWrapAndNewBoot(t *testing.T) {
	var clock LogClock
	if at, _ := clock.ReadLine("[18:56:15]: Client authenticated: (KU_ONE) name"); !at.IsZero() {
		t.Fatal("unanchored tail acquired a timestamp")
	}
	clock.ReadLine("[00:00:00]: Current time: Sun Sep  6 15:51:17 2026")
	for i := 0; i < 4; i++ {
		clock.ReadLine("[23:59:59]: heartbeat")
		clock.ReadLine("[00:00:01]: heartbeat")
	}
	at, payload := clock.ReadLine("[18:56:15]: Client authenticated: (KU_ONE) 名字有毒")
	want := time.Date(2026, 9, 11, 10, 47, 32, 0, time.Local)
	if !at.Equal(want) || payload != "Client authenticated: (KU_ONE) 名字有毒" {
		t.Fatalf("got %s %q, want %s", at, payload, want)
	}
	clock.ReadLine("[00:00:00]: Starting Up")
	if at, _ := clock.ReadLine("[00:00:01]: player"); !at.IsZero() {
		t.Fatal("new boot retained old wall clock")
	}
}
