package traffictransaction

import "time"

// EventCode is the closed, secret-free vocabulary emitted for user-visible
// Traffic Analysis lifecycle facts. Event values intentionally carry no
// transaction, backup, configuration, or error details.
type EventCode string

const (
	EventBackupCreated      EventCode = "traffic_backup_created"
	EventRouteApplied       EventCode = "traffic_route_applied"
	EventAnalysisStarted    EventCode = "traffic_analysis_started"
	EventBackupRemoved      EventCode = "traffic_backup_removed"
	EventRouteRestored      EventCode = "traffic_route_restored"
	EventAnalysisStopped    EventCode = "traffic_analysis_stopped"
	EventBackupCreateFailed EventCode = "traffic_backup_create_failed"
	EventRestoreFailed      EventCode = "traffic_restore_failed"
	EventCleanupPending     EventCode = "traffic_cleanup_pending"
	EventRecoveryRequired   EventCode = "traffic_recovery_required"
)

type EventSeverity string

const (
	EventSeverityInfo    EventSeverity = "info"
	EventSeveritySuccess EventSeverity = "success"
	EventSeverityWarning EventSeverity = "warning"
	EventSeverityError   EventSeverity = "error"
)

// Event is the complete wire contract for a Traffic Analysis runtime-log
// event. Keep this DTO closed: adding a free-form field can expose internal
// transaction state to the frontend.
type Event struct {
	Timestamp string        `json:"timestamp"`
	Code      EventCode     `json:"code"`
	Severity  EventSeverity `json:"severity"`
}

type EventSink func(Event)

func (s *Service) emitEvent(code EventCode, severity EventSeverity) {
	if s.deps.Events == nil {
		return
	}
	event := Event{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Code:      code,
		Severity:  severity,
	}
	defer func() { _ = recover() }()
	s.deps.Events(event)
}
