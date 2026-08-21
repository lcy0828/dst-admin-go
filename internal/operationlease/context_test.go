package operationlease

import (
	"context"
	"testing"
	"time"
)

func TestBorrowedLeaseProviderReturnsCurrentLease(t *testing.T) {
	current := Lease{RoomID: "room-a", LeaseID: "lease-1", ExpiresAt: time.Now().UTC().Add(time.Minute)}
	ctx := WithBorrowedLeaseProvider(context.Background(), func(roomID string) (Lease, bool) {
		return current, roomID == current.RoomID
	})
	if lease, ok := BorrowedLease(ctx, "room-a"); !ok || lease.LeaseID != "lease-1" {
		t.Fatalf("initial lease=%#v found=%v", lease, ok)
	}
	current.LeaseID = "lease-2"
	if lease, ok := BorrowedLease(ctx, "room-a"); !ok || lease.LeaseID != "lease-2" {
		t.Fatalf("updated lease=%#v found=%v", lease, ok)
	}
}

func TestBorrowedLeaseStaticContextIsCopied(t *testing.T) {
	original := Lease{RoomID: "room-a", LeaseID: "lease-1"}
	ctx := WithBorrowedLeases(context.Background(), original)
	original.LeaseID = "changed"
	if lease, ok := BorrowedLease(ctx, "room-a"); !ok || lease.LeaseID != "lease-1" {
		t.Fatalf("borrowed lease=%#v found=%v", lease, ok)
	}
}
