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

// deliveryConsumedKey carries a delivery turn's queue-verdict hook.
type deliveryConsumedKey struct{}

// withDeliveryConsumed attaches fn as the OnConsumed hook of the
// delivery turn run from ctx (#355). A delivery that finds the parent
// busy is queued as a system-delivery call, and only the queue knows
// when it reaches the parent: fn receives the queue's verdict through
// the call's OnConsumed (#351). Unexported, so only the delivery path
// in this package can set it.
func withDeliveryConsumed(ctx context.Context, fn func(consumed bool)) context.Context {
	return context.WithValue(ctx, deliveryConsumedKey{}, fn)
}

// deliveryConsumedFromContext returns the hook withDeliveryConsumed
// attached to ctx, or nil.
func deliveryConsumedFromContext(ctx context.Context) func(consumed bool) {
	fn, _ := ctx.Value(deliveryConsumedKey{}).(func(consumed bool))
	return fn
}
