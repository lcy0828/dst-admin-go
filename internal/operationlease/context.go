package operationlease

import "context"

type borrowedLeasesContextKey struct{}
type borrowedLeaseProviderContextKey struct{}

type BorrowedLeaseProvider func(string) (Lease, bool)

// WithBorrowedLeases lets nested runtime operations reuse leases already held
// by the parent operation instead of deadlocking on a second acquisition.
func WithBorrowedLeases(ctx context.Context, leases ...Lease) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	values := make(map[string]Lease, len(leases))
	if existing, ok := ctx.Value(borrowedLeasesContextKey{}).(map[string]Lease); ok {
		for roomID, lease := range existing {
			values[roomID] = lease
		}
	}
	for _, lease := range leases {
		if lease.RoomID != "" {
			values[lease.RoomID] = lease
		}
	}
	return context.WithValue(ctx, borrowedLeasesContextKey{}, values)
}

func WithBorrowedLeaseProvider(ctx context.Context, provider BorrowedLeaseProvider) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if provider == nil {
		return ctx
	}
	return context.WithValue(ctx, borrowedLeaseProviderContextKey{}, provider)
}

func BorrowedLease(ctx context.Context, roomID string) (Lease, bool) {
	if ctx == nil {
		return Lease{}, false
	}
	if provider, ok := ctx.Value(borrowedLeaseProviderContextKey{}).(BorrowedLeaseProvider); ok {
		if lease, found := provider(roomID); found {
			return lease, true
		}
	}
	values, ok := ctx.Value(borrowedLeasesContextKey{}).(map[string]Lease)
	if !ok {
		return Lease{}, false
	}
	lease, ok := values[roomID]
	return lease, ok
}
