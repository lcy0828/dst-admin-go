package aicontrol

import "context"

type dispatchedPrincipalKey struct{}
type genericResultKey struct{}

// Internal dispatch context cannot be forged by HTTP headers or model arguments.
// The authenticating service credential is still verified by the real router.
func WithDispatchedPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, dispatchedPrincipalKey{}, p)
}
func DispatchedPrincipal(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(dispatchedPrincipalKey{}).(Principal)
	return p, ok
}
