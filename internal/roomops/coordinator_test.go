package roomops

import (
	"context"
	"testing"
	"time"
)

func TestAcquireSerializesRoomOperationsAndAllowsNestedLease(t *testing.T) {
	ctx, release, err := Acquire(context.Background(), "room-one")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, nestedRelease, err := Acquire(ctx, "room-one")
	if err != nil {
		t.Fatal(err)
	}
	nestedRelease()

	acquired := make(chan struct{})
	go func() {
		_, nextRelease, acquireErr := Acquire(context.Background(), "room-one")
		if acquireErr == nil {
			close(acquired)
			nextRelease()
		}
	}()
	select {
	case <-acquired:
		t.Fatal("second operation acquired the same room before release")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second operation did not acquire the room after release")
	}
}

func TestAcquireHonorsCancellation(t *testing.T) {
	_, release, err := Acquire(context.Background(), "room-cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Acquire(ctx, "room-cancel"); err == nil {
		t.Fatal("canceled acquisition unexpectedly succeeded")
	}
}

func TestAcquireManyAllowsNestedAccessToEveryLeasedRoom(t *testing.T) {
	ctx, release, err := AcquireMany(context.Background(), []string{"room-b", "room-a", "room-b"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, roomID := range []string{"room-a", "room-b"} {
		_, nestedRelease, err := Acquire(ctx, roomID)
		if err != nil {
			t.Fatal(err)
		}
		nestedRelease()
	}
}
