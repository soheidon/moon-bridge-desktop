package publishrecovery

// FaultPoint identifies a fault-injection seam. Step 3A defines the type only;
// concrete points are added alongside the operations that can be faulted.
type FaultPoint string

// FaultInjector lets tests inject deterministic failures at named seams. The
// production binary uses NoopFaultInjector — injection is a test-only seam.
type FaultInjector interface {
	Hit(FaultPoint) error
}

// NoopFaultInjector never faults.
type NoopFaultInjector struct{}

// Hit implements FaultInjector by returning nil.
func (NoopFaultInjector) Hit(FaultPoint) error { return nil }
