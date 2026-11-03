package aicontrol

import "context"

type modelChannelKey struct{}

// WithModelChannel is set by authenticated model transports, never by a header
// or body field. In-process API dispatch preserves it through nested recipes.
func WithModelChannel(ctx context.Context) context.Context {
	return context.WithValue(ctx, modelChannelKey{}, true)
}
func IsModelChannel(ctx context.Context) bool { v, _ := ctx.Value(modelChannelKey{}).(bool); return v }
