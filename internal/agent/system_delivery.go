package agent

import "context"

// systemDeliveryKey tags a context as a system delivery turn.
type systemDeliveryKey struct{}

// WithSystemDelivery marks the context as a dispatch-result delivery
// turn (#388). The run started from it carries the systemDelivery mark
// on its SessionAgentCall, which keeps the queued call alive across
// ClearQueue, Cancel, and the cancel-covered queue partitions, and
// hides it from the queued-prompt surfaces the UI reads. The marker is
// set only on the delivery path; unexported, so it cannot be forged
// from outside the package.
func WithSystemDelivery(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemDeliveryKey{}, true)
}

// SystemDeliveryFromContext reports whether ctx carries the
// WithSystemDelivery marker.
func SystemDeliveryFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(systemDeliveryKey{}).(bool)
	return v
}
