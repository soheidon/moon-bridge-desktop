package publishrecovery

// FaultPoint identifies a fault-injection seam. Step 3A defines the type only;
// concrete points are added alongside the operations that can be faulted.
type FaultPoint string

// Publish fault seams: each fires right after the named durable mutation or
// journal write, so a hit aborts Publish with the journal at its last durable
// phase — exactly the crash windows §11 of the plan maps to recovery actions.
const (
	FaultAfterPreparedJournal FaultPoint = "after_prepared_journal"
	FaultAfterBackoutCopy     FaultPoint = "after_backout_copy"
	FaultAfterCatalogWrite    FaultPoint = "after_catalog_write"
	FaultAfterCatalogJournal  FaultPoint = "after_catalog_journal"
	FaultAfterAuthWrite       FaultPoint = "after_auth_write"
	FaultAfterAuthJournal     FaultPoint = "after_auth_journal"
	FaultAfterConfigWrite     FaultPoint = "after_config_write"
	FaultAfterConfigJournal   FaultPoint = "after_config_journal"
	FaultAfterVerified        FaultPoint = "after_verified"
)

// FaultInjector lets tests inject deterministic failures at named seams. The
// production binary uses NoopFaultInjector — injection is a test-only seam.
type FaultInjector interface {
	Hit(FaultPoint) error
}

// NoopFaultInjector never faults.
type NoopFaultInjector struct{}

// Hit implements FaultInjector by returning nil.
func (NoopFaultInjector) Hit(FaultPoint) error { return nil }
