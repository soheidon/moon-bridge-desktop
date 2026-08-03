use crate::{
    management_connection, snapshot, start_gateway_inner, stop_gateway_inner, GatewaySnapshot,
    GatewayStateStore,
};
use chrono::{Local, Utc};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use tauri::Emitter;
use toml_edit::{value, DocumentMut};
use uuid::Uuid;

#[cfg(test)]
use std::sync::{Mutex, OnceLock};

#[cfg(test)]
static INJECTED_PUBLISH_FAILURES: OnceLock<Mutex<Vec<PathBuf>>> = OnceLock::new();

pub const CAPTURE_BASE_URL: &str = "http://127.0.0.1:38441";

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TrafficCommandError {
    pub operation: String,
    pub operation_id: String,
    pub stage: String,
    pub code: String,
    pub message: String,
    pub retryable: bool,
    pub config_changed: bool,
    pub capture_running: bool,
    pub restart_codex_required: bool,
    pub gateway_snapshot: Option<GatewaySnapshot>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct CaptureStatus {
    pub instance_id: Option<String>,
    pub state: String,
    pub session_id: Option<String>,
    pub capture_address: String,
    pub upstream_host: String,
    pub started_at: Option<String>,
    pub http_requests: u64,
    pub sse_streams: u64,
    pub websocket_connections: u64,
    pub observation_count: u64,
    pub dropped_observations: u64,
    pub dropped_backpressure: u64,
    pub active_http_requests: u64,
    pub active_websocket_connections: u64,
    pub last_sequence: u64,
    pub last_safe_error: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TrafficAnalysisStatus {
    pub capture: CaptureStatus,
    pub config_path: String,
    pub config_exists: bool,
    pub integration_active: bool,
    pub relay_active: bool,
    pub recovery_available: bool,
    pub applied_openai_base_url: Option<String>,
    pub recovery_phase: Option<String>,
    pub reconciliation_status: Option<String>,
    pub reconciled_at: Option<String>,
    pub auto_save: TrafficAutoSaveStatus,
}

#[derive(Debug, Clone, Serialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct TrafficAutoSaveStatus {
    pub enabled: bool,
    pub active: bool,
    pub destination: Option<String>,
    pub last_persisted_sequence: u64,
    pub observations_written: u64,
    pub sequence_gaps: u64,
    pub last_synced_at: Option<String>,
    pub finalized: bool,
    pub last_safe_error: Option<AutoSaveError>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AutoSaveError {
    pub code: String,
    pub message: String,
    pub retryable: bool,
}

#[derive(Debug)]
pub struct TrafficAutoSaveSession {
    pub session_id: String,
    pub destination: PathBuf,
    pub started_at: String,
    pub last_persisted_sequence: u64,
    pub observations_written: u64,
    pub sequence_gaps: u64,
    pub last_synced_at: Option<String>,
    pub finalized: bool,
    pub last_safe_error: Option<AutoSaveError>,
    pub dropped_observations: u64,
    pub last_checkpoint_sequence: u64,
    pub last_checkpoint_at: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TrafficAnalysisResult {
    pub operation_id: String,
    pub status: TrafficAnalysisStatus,
    pub config_path: String,
    pub restart_codex_required: bool,
    pub gateway_snapshot: GatewaySnapshot,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TrafficObservationPage {
    pub observations: Vec<serde_json::Value>,
    pub dropped: u64,
    pub last_sequence: u64,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TrafficExportResult {
    pub operation_id: String,
    pub destination: String,
    pub observation_count: usize,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TrafficRevealResult {
    pub operation_id: String,
    pub destination: String,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RestoreInput {
    pub operation_id: String,
    pub confirm_conflict: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RecoveryState {
    schema_version: u32,
    integration_active: bool,
    #[serde(default = "default_recovery_phase")]
    phase: String,
    operation_id: String,
    config_path: String,
    previous_openai_base_url_present: bool,
    previous_openai_base_url: Option<String>,
    applied_openai_base_url: String,
    config_hash_before_apply: String,
    config_hash_after_apply: String,
    backup_path: Option<String>,
    started_at: String,
    #[serde(default)]
    updated_at: Option<String>,
    #[serde(default)]
    auto_log: Option<AutoLogRecoveryState>,
    #[serde(default)]
    auto_log_status: Option<String>,
    #[serde(default)]
    unsaved_observations_may_remain: bool,
    #[serde(default)]
    unsaved_discard_confirmed: bool,
    #[serde(default)]
    migration: Option<RecoveryMigrationState>,
    #[serde(default)]
    capture_state_last_known: Option<String>,
    #[serde(default)]
    relay_active_last_known: bool,
    #[serde(default)]
    reconciliation_status: Option<String>,
    #[serde(default)]
    reconciled_at: Option<String>,
    #[serde(default)]
    reconciliation_detail: Option<String>,
    #[serde(default)]
    restart_attempted: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct AutoLogRecoveryState {
    session_id: String,
    path: String,
    last_checkpoint_sequence: u64,
    finalized: bool,
    last_checkpoint_at: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RecoveryMigrationState {
    source_path: String,
    source_schema_version: u32,
    migrated_at: String,
}

fn default_recovery_phase() -> String {
    "integration_applied".to_string()
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct TrafficProgress {
    operation_id: String,
    operation: String,
    stage: String,
    message: String,
}

pub async fn status(state: &GatewayStateStore) -> Result<TrafficAnalysisStatus, String> {
    let config_path = resolve_config_path()?;
    let recovery = read_recovery_state(state).ok();
    let capture = if let Ok((address, token)) = management_connection(state) {
        crate::moonbridge_api::ApiClient::new(state.client.clone(), address, token)
            .get::<CaptureStatus>("/system/traffic-analysis/status")
            .await
            .unwrap_or_else(|_| stopped_capture_status())
    } else {
        stopped_capture_status()
    };
    let auto_save = state
        .traffic_auto_save
        .lock()
        .map_err(|error| error.to_string())?
        .as_ref()
        .map(auto_save_status)
        .unwrap_or_default();
    let recovery_phase = recovery.as_ref().map(|value| value.phase.clone());
    let reconciliation_status = recovery
        .as_ref()
        .and_then(|value| value.reconciliation_status.clone());
    let reconciled_at = recovery
        .as_ref()
        .and_then(|value| value.reconciled_at.clone());
    let applied_openai_base_url = recovery.as_ref().and_then(|value| {
        if value.integration_active {
            Some(value.applied_openai_base_url.clone())
        } else {
            None
        }
    });
    Ok(TrafficAnalysisStatus {
        relay_active: capture_relay_active(&capture),
        capture,
        config_exists: config_path.is_file(),
        config_path: config_path.display().to_string(),
        integration_active: recovery
            .as_ref()
            .is_some_and(|value| value.integration_active),
        recovery_available: recovery.as_ref().is_some_and(|value| {
            value.integration_active
                && !matches!(
                    value.reconciliation_status.as_deref(),
                    Some("already_restored" | "inactive")
                )
        }),
        applied_openai_base_url,
        recovery_phase,
        reconciliation_status,
        reconciled_at,
        auto_save,
    })
}

fn auto_save_status(session: &TrafficAutoSaveSession) -> TrafficAutoSaveStatus {
    TrafficAutoSaveStatus {
        enabled: true,
        active: !session.finalized,
        destination: Some(session.destination.display().to_string()),
        last_persisted_sequence: session.last_persisted_sequence,
        observations_written: session.observations_written,
        sequence_gaps: session.sequence_gaps,
        last_synced_at: session.last_synced_at.clone(),
        finalized: session.finalized,
        last_safe_error: session.last_safe_error.clone(),
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct ReconciliationDecision {
    status: &'static str,
    phase: &'static str,
    integration_active: bool,
    detail: &'static str,
}

fn classify_recovery_after_startup(
    recovery: &RecoveryState,
    current: &ReadConfig,
) -> ReconciliationDecision {
    let current_hash = hash_bytes(&current.bytes);
    let current_url = current.previous_url.as_deref();
    let applied_url = Some(recovery.applied_openai_base_url.as_str());
    let previous_url = recovery.previous_openai_base_url.as_deref();
    let matches_applied =
        current_hash == recovery.config_hash_after_apply && current_url == applied_url;
    let matches_previous_url = current_url == previous_url;

    // A prepared/capture-started record whose exact candidate is on disk
    // means the process may have crashed between publication and the final
    // recovery write. Recognize ownership, but never publish the candidate.
    if matches_applied
        && (recovery.integration_active
            || matches!(recovery.phase.as_str(), "prepared" | "capture_started"))
    {
        return ReconciliationDecision {
            status: "pending_restore",
            phase: "reconciliation_required",
            integration_active: true,
            detail: "Codex設定はCapture用の適用値です。自動再適用は行わず、復元確認が必要です",
        };
    }

    if recovery.integration_active && matches_previous_url {
        return ReconciliationDecision {
            status: "already_restored",
            phase: "reconciled_restored",
            integration_active: false,
            detail: "Codex設定は既に元の接続先へ戻っています",
        };
    }

    if recovery.integration_active {
        return ReconciliationDecision {
            status: "config_conflict",
            phase: "reconciliation_conflict",
            integration_active: true,
            detail: "Codex設定が適用後に変更されています。自動復元を行いません",
        };
    }

    ReconciliationDecision {
        status: "inactive",
        phase: "inactive",
        integration_active: false,
        detail: "復旧対象の統合は有効ではありません",
    }
}

/// Reconciles persisted state with the current Codex file without changing
/// that file. This is intentionally synchronous and safe to call during
/// Tauri setup, before Gateway or Capture is started.
pub fn reconcile_startup(state: &GatewayStateStore) -> Result<(), String> {
    let Ok(mut recovery) = read_recovery_state(state) else {
        return Ok(());
    };
    let config_path = PathBuf::from(&recovery.config_path);
    if !config_path.is_absolute() || reject_reparse(&config_path).is_err() {
        recovery.reconciliation_status = Some("config_path_invalid".to_string());
        recovery.phase = "reconciliation_conflict".to_string();
        recovery.reconciled_at = Some(Utc::now().to_rfc3339());
        recovery.reconciliation_detail =
            Some("Codex設定の対象パスを安全に検証できません".to_string());
        return write_recovery_state(state, &recovery);
    }

    let current = match read_config(&config_path) {
        Ok(value) => value,
        Err(_) => {
            recovery.reconciliation_status = Some("config_unreadable".to_string());
            recovery.phase = "reconciliation_conflict".to_string();
            recovery.reconciled_at = Some(Utc::now().to_rfc3339());
            recovery.reconciliation_detail =
                Some("Codex設定を読み込めないため、自動復元を行いません".to_string());
            return write_recovery_state(state, &recovery);
        }
    };
    let decision = classify_recovery_after_startup(&recovery, &current);
    let unchanged = recovery.reconciliation_status.as_deref() == Some(decision.status)
        && recovery.phase == decision.phase
        && recovery.integration_active == decision.integration_active
        && recovery.reconciliation_detail.as_deref() == Some(decision.detail);
    if unchanged {
        return Ok(());
    }
    recovery.reconciliation_status = Some(decision.status.to_string());
    recovery.phase = decision.phase.to_string();
    recovery.integration_active = decision.integration_active;
    recovery.reconciled_at = Some(Utc::now().to_rfc3339());
    recovery.reconciliation_detail = Some(decision.detail.to_string());
    write_recovery_state(state, &recovery)
}

pub async fn start(
    app: &tauri::AppHandle,
    state: &GatewayStateStore,
    operation_id: &str,
) -> Result<TrafficAnalysisResult, TrafficCommandError> {
    if operation_id.trim().is_empty() {
        return Err(command_error(
            operation_id,
            "validating",
            "invalid_operation_id",
            false,
            false,
            state,
        ));
    }
    if read_recovery_state(state).is_ok_and(|value| value.integration_active) {
        return Err(command_error(
            operation_id,
            "validating",
            "analysis_already_active",
            false,
            false,
            state,
        ));
    }
    let config_path = resolve_config_path().map_err(|_| {
        command_error(
            operation_id,
            "validating",
            "codex_target_invalid",
            false,
            false,
            state,
        )
    })?;
    emit_progress(
        app,
        operation_id,
        "validating",
        "Codex設定の対象を確認しています",
    );

    // The recovery record and config backup must exist before any gateway or
    // Capture startup can mutate the user's effective integration state.
    emit_progress(
        app,
        operation_id,
        "reading_config",
        "Codex設定を読み込んでいます",
    );
    let original = match read_config(&config_path) {
        Ok(value) => value,
        Err(_) => {
            return Err(command_error(
                operation_id,
                "reading_config",
                "config_parse_failed",
                false,
                false,
                state,
            ))
        }
    };
    emit_progress(
        app,
        operation_id,
        "backing_up_config",
        "設定のバックアップを作成しています",
    );
    let backup_path = match create_backup(state, &config_path, &original.bytes, original.existed) {
        Ok(value) => value,
        Err(_) => {
            return Err(command_error(
                operation_id,
                "backing_up_config",
                "backup_failed",
                false,
                false,
                state,
            ))
        }
    };
    emit_progress(
        app,
        operation_id,
        "initializing_log",
        "分析ログの保存先を準備しています",
    );
    if initialize_autosave(state).is_err() {
        return Err(command_error(
            operation_id,
            "initializing_log",
            "autosave_init_failed",
            false,
            false,
            state,
        ));
    }
    let started_at = Utc::now().to_rfc3339();
    let hash_before = hash_bytes(&original.bytes);
    let candidate = render_with_base_url(&original.document, CAPTURE_BASE_URL);
    let hash_after = hash_bytes(&candidate);
    let mut recovery = RecoveryState {
        schema_version: 2,
        integration_active: false,
        phase: "prepared".to_string(),
        operation_id: operation_id.to_string(),
        config_path: config_path.display().to_string(),
        previous_openai_base_url_present: original.previous_url.is_some(),
        previous_openai_base_url: original.previous_url.clone(),
        applied_openai_base_url: CAPTURE_BASE_URL.to_string(),
        config_hash_before_apply: hash_before,
        config_hash_after_apply: hash_after,
        backup_path: backup_path.map(|path| path.display().to_string()),
        started_at,
        updated_at: Some(Utc::now().to_rfc3339()),
        auto_log: autosave_recovery_state(state),
        auto_log_status: Some("active".to_string()),
        unsaved_observations_may_remain: false,
        unsaved_discard_confirmed: false,
        migration: None,
        capture_state_last_known: Some("stopped".to_string()),
        relay_active_last_known: false,
        reconciliation_status: Some("unreconciled".to_string()),
        reconciled_at: None,
        reconciliation_detail: None,
        restart_attempted: false,
    };
    if write_recovery_state(state, &recovery).is_err() {
        abort_autosave(state);
        return Err(command_error(
            operation_id,
            "preparing_recovery",
            "recovery_state_failed",
            false,
            false,
            state,
        ));
    }

    let before_gateway = snapshot(state).map_err(|_| {
        command_error(
            operation_id,
            "starting_capture",
            "state_unavailable",
            false,
            false,
            state,
        )
    })?;
    let gateway_started_by_operation =
        !matches!(before_gateway.state, crate::GatewayState::Running);
    if gateway_started_by_operation {
        emit_progress(
            app,
            operation_id,
            "starting_capture",
            "Gatewayを起動しています",
        );
        if start_gateway_inner(app, state).await.is_err() {
            return Err(command_error(
                operation_id,
                "starting_capture",
                "gateway_start_failed",
                false,
                false,
                state,
            ));
        }
    }
    let (address, token) = match management_connection(state) {
        Ok(value) => value,
        Err(_) => {
            return Err(cleanup_start_failure(
                app,
                state,
                operation_id,
                gateway_started_by_operation,
                "waiting_for_readiness",
                "capture_not_ready",
            )
            .await)
        }
    };
    if crate::validate_running_sidecar(state).await.is_err() {
        return Err(cleanup_start_failure(
            app,
            state,
            operation_id,
            gateway_started_by_operation,
            "validating",
            "sidecar_incompatible",
        )
        .await);
    }
    let api = crate::moonbridge_api::ApiClient::new(state.client.clone(), address, token);
    if let Ok(existing_capture) = api
        .get::<CaptureStatus>("/system/traffic-analysis/status")
        .await
    {
        if existing_capture.state == "passthrough" {
            return Err(command_error(
                operation_id,
                "validating",
                "capture_relay_active",
                false,
                true,
                state,
            ));
        }
    }
    emit_progress(
        app,
        operation_id,
        "waiting_for_readiness",
        "Capture Proxyを開始しています",
    );
    if api
        .post::<CaptureStatus>("/system/traffic-analysis/start", serde_json::json!({}))
        .await
        .is_err()
    {
        return Err(cleanup_start_failure(
            app,
            state,
            operation_id,
            gateway_started_by_operation,
            "waiting_for_readiness",
            "capture_start_failed",
        )
        .await);
    }
    let capture = match api
        .get::<CaptureStatus>("/system/traffic-analysis/status")
        .await
    {
        Ok(value) if value.state == "capturing" => value,
        _ => {
            return Err(cleanup_start_failure(
                app,
                state,
                operation_id,
                gateway_started_by_operation,
                "waiting_for_readiness",
                "capture_not_ready",
            )
            .await)
        }
    };
    if capture.instance_id != snapshot(state).ok().and_then(|value| value.instance_id) {
        return Err(cleanup_start_failure(
            app,
            state,
            operation_id,
            gateway_started_by_operation,
            "validating",
            "capture_instance_mismatch",
        )
        .await);
    }
    recovery.phase = "capture_started".to_string();
    recovery.capture_state_last_known = Some(capture.state.clone());
    recovery.relay_active_last_known = capture_relay_active(&capture);
    recovery.auto_log = autosave_recovery_state(state);
    if write_recovery_state(state, &recovery).is_err() {
        return Err(cleanup_start_failure(
            app,
            state,
            operation_id,
            gateway_started_by_operation,
            "preparing_recovery",
            "recovery_state_failed",
        )
        .await);
    }
    emit_progress(
        app,
        operation_id,
        "publishing_config",
        "openai_base_urlだけを更新しています",
    );
    if publish_config(&config_path, &candidate).is_err() {
        return Err(cleanup_start_failure(
            app,
            state,
            operation_id,
            gateway_started_by_operation,
            "publishing_config",
            "config_publish_failed",
        )
        .await);
    }
    if verify_config(&config_path, Some(CAPTURE_BASE_URL)).is_err() {
        let _ = restore_original_config(&config_path, &original);
        return Err(cleanup_start_failure(
            app,
            state,
            operation_id,
            gateway_started_by_operation,
            "verifying_config",
            "config_verification_failed",
        )
        .await);
    }
    recovery.integration_active = true;
    recovery.phase = "integration_applied".to_string();
    recovery.capture_state_last_known = Some(capture.state.clone());
    recovery.relay_active_last_known = capture_relay_active(&capture);
    recovery.auto_log = autosave_recovery_state(state);
    if write_recovery_state(state, &recovery).is_err() {
        let _ = restore_original_config(&config_path, &original);
        return Err(cleanup_start_failure(
            app,
            state,
            operation_id,
            gateway_started_by_operation,
            "verifying_config",
            "recovery_state_failed",
        )
        .await);
    }
    emit_progress(
        app,
        operation_id,
        "complete",
        "CaptureとCodex設定を準備しました。Codexの再起動が必要です",
    );
    let gateway_snapshot = snapshot(state).map_err(|_| {
        command_error(
            operation_id,
            "complete",
            "state_unavailable",
            true,
            true,
            state,
        )
    })?;
    let _ = capture;
    Ok(TrafficAnalysisResult {
        operation_id: operation_id.to_string(),
        status: status(state).await.map_err(|_| {
            command_error(
                operation_id,
                "complete",
                "status_unavailable",
                true,
                true,
                state,
            )
        })?,
        config_path: config_path.display().to_string(),
        restart_codex_required: true,
        gateway_snapshot,
    })
}

/// Restarts only the Capture path for a previously recognized recovery
/// incident. It never writes Codex config and is intentionally limited to one
/// user-confirmed attempt per persisted incident.
pub async fn restart_capture(
    app: &tauri::AppHandle,
    state: &GatewayStateStore,
    operation_id: &str,
) -> Result<TrafficAnalysisResult, TrafficCommandError> {
    let mut recovery = read_recovery_state(state).map_err(|_| {
        command_error(
            operation_id,
            "validating_recovery",
            "recovery_state_missing",
            false,
            false,
            state,
        )
    })?;
    if !recovery.integration_active {
        return Err(command_error(
            operation_id,
            "validating_recovery",
            "recovery_not_active",
            false,
            false,
            state,
        ));
    }
    if recovery.restart_attempted {
        return Err(command_error(
            operation_id,
            "validating_recovery",
            "recovery_restart_already_attempted",
            false,
            false,
            state,
        ));
    }
    if recovery.reconciliation_status.as_deref() == Some("config_conflict") {
        return Err(command_error(
            operation_id,
            "validating_recovery",
            "recovery_config_conflict",
            false,
            false,
            state,
        ));
    }
    let config = read_config(Path::new(&recovery.config_path)).map_err(|_| {
        command_error(
            operation_id,
            "validating_recovery",
            "config_parse_failed",
            false,
            false,
            state,
        )
    })?;
    if hash_bytes(&config.bytes) != recovery.config_hash_after_apply
        || config.previous_url.as_deref() != Some(recovery.applied_openai_base_url.as_str())
    {
        return Err(command_error(
            operation_id,
            "validating_recovery",
            "recovery_config_conflict",
            false,
            false,
            state,
        ));
    }

    recovery.restart_attempted = true;
    recovery.phase = "restart_prepared".to_string();
    recovery.reconciliation_detail =
        Some("ユーザー確認済みのCapture再起動を準備しています".to_string());
    write_recovery_state(state, &recovery).map_err(|_| {
        command_error(
            operation_id,
            "preparing_recovery",
            "recovery_state_failed",
            false,
            false,
            state,
        )
    })?;

    let before = snapshot(state).map_err(|_| {
        command_error(
            operation_id,
            "starting_capture",
            "state_unavailable",
            false,
            false,
            state,
        )
    })?;
    if !matches!(before.state, crate::GatewayState::Running) {
        emit_progress(
            app,
            operation_id,
            "starting_capture",
            "Gatewayを確認しています",
        );
        if start_gateway_inner(app, state).await.is_err() {
            recovery.phase = "restart_failed".to_string();
            recovery.reconciliation_detail = Some("Gateway起動に失敗しました".to_string());
            let _ = write_recovery_state(state, &recovery);
            return Err(command_error(
                operation_id,
                "starting_capture",
                "gateway_start_failed",
                false,
                false,
                state,
            ));
        }
    }
    let (address, token) = management_connection(state).map_err(|_| {
        command_error(
            operation_id,
            "starting_capture",
            "capture_not_ready",
            false,
            false,
            state,
        )
    })?;
    let api = crate::moonbridge_api::ApiClient::new(state.client.clone(), address, token);
    let current_capture = api
        .get::<CaptureStatus>("/system/traffic-analysis/status")
        .await
        .map_err(|_| {
            command_error(
                operation_id,
                "starting_capture",
                "capture_not_ready",
                false,
                false,
                state,
            )
        })?;
    if capture_relay_active(&current_capture) {
        return Err(command_error(
            operation_id,
            "starting_capture",
            "capture_relay_active",
            false,
            true,
            state,
        ));
    }
    emit_progress(
        app,
        operation_id,
        "starting_capture",
        "Capture Proxyを再起動しています",
    );
    api.post::<CaptureStatus>("/system/traffic-analysis/start", serde_json::json!({}))
        .await
        .map_err(|_| {
            command_error(
                operation_id,
                "starting_capture",
                "capture_start_failed",
                false,
                false,
                state,
            )
        })?;
    let capture = api
        .get::<CaptureStatus>("/system/traffic-analysis/status")
        .await
        .map_err(|_| {
            command_error(
                operation_id,
                "starting_capture",
                "capture_not_ready",
                false,
                false,
                state,
            )
        })?;
    if capture.state != "capturing" {
        return Err(command_error(
            operation_id,
            "starting_capture",
            "capture_not_ready",
            false,
            capture_relay_active(&capture),
            state,
        ));
    }
    recovery.phase = "capture_restarted".to_string();
    recovery.capture_state_last_known = Some(capture.state.clone());
    recovery.relay_active_last_known = capture_relay_active(&capture);
    recovery.reconciliation_status = Some("capture_restarted".to_string());
    recovery.reconciliation_detail = Some("Capture Proxyを再起動しました".to_string());
    write_recovery_state(state, &recovery).map_err(|_| {
        command_error(
            operation_id,
            "preparing_recovery",
            "recovery_state_failed",
            false,
            true,
            state,
        )
    })?;
    let status = status(state).await.map_err(|_| {
        command_error(
            operation_id,
            "complete",
            "status_unavailable",
            false,
            true,
            state,
        )
    })?;
    Ok(TrafficAnalysisResult {
        operation_id: operation_id.to_string(),
        config_path: status.config_path.clone(),
        restart_codex_required: false,
        gateway_snapshot: snapshot(state).unwrap_or_else(|_| empty_gateway_snapshot()),
        status,
    })
}

pub async fn stop(
    app: &tauri::AppHandle,
    state: &GatewayStateStore,
    operation_id: &str,
) -> Result<TrafficAnalysisResult, TrafficCommandError> {
    emit_progress(
        app,
        operation_id,
        "restoring_config",
        "Codex設定を確認しています",
    );
    let recovery = read_recovery_state(state).map_err(|_| {
        command_error(
            operation_id,
            "restoring_config",
            "recovery_state_missing",
            false,
            false,
            state,
        )
    })?;
    if !recovery.integration_active {
        return status(state)
            .await
            .map(|status| TrafficAnalysisResult {
                operation_id: operation_id.to_string(),
                status,
                config_path: recovery.config_path,
                restart_codex_required: false,
                gateway_snapshot: snapshot(state).unwrap_or_else(|_| empty_gateway_snapshot()),
            })
            .map_err(|_| {
                command_error(
                    operation_id,
                    "complete",
                    "status_unavailable",
                    false,
                    false,
                    state,
                )
            });
    }
    emit_progress(
        app,
        operation_id,
        "draining_capture",
        "新しい観測の記録を停止しています",
    );
    pause_capture(app, state, operation_id).await?;
    let page = match fetch_observations(state, 0).await {
        Ok(page) => Some(page),
        Err(_) => {
            set_autosave_error(state, "autosave_read_failed");
            None
        }
    };
    if let Some(page) = page.as_ref() {
        let _ = sync_autosave(state, &page.observations, page.dropped);
    }
    let result = restore_active_config(app, state, operation_id, &recovery, false).await?;
    if let Some(page) = page.as_ref() {
        if finalize_autosave(state, &page.observations, page.dropped).is_err() {
            mark_recovery_autosave_failure(state);
        } else {
            mark_recovery_autosave_finalized(state);
        }
    } else {
        mark_recovery_autosave_failure(state);
    }
    let mut result = result;
    result.status = status(state).await.map_err(|_| {
        command_error(
            operation_id,
            "complete",
            "status_unavailable",
            true,
            true,
            state,
        )
    })?;
    Ok(result)
}

pub async fn finish_relay(
    app: &tauri::AppHandle,
    state: &GatewayStateStore,
    operation_id: &str,
    discard_unsaved: bool,
) -> Result<TrafficAnalysisResult, TrafficCommandError> {
    if operation_id.trim().is_empty() {
        return Err(command_error(
            operation_id,
            "validating",
            "invalid_operation_id",
            false,
            false,
            state,
        ));
    }
    if read_recovery_state(state).is_ok_and(|value| value.integration_active) {
        return Err(command_error(
            operation_id,
            "validating",
            "analysis_still_active",
            false,
            true,
            state,
        ));
    }
    let page = match fetch_observations(state, 0).await {
        Ok(page) => Some(page),
        Err(_) => {
            set_autosave_error(state, "autosave_read_failed");
            None
        }
    };
    if let Some(page) = page.as_ref() {
        let finalize_result = finalize_autosave(state, &page.observations, page.dropped);
        if finalize_result.is_err() {
            mark_recovery_autosave_failure(state);
            if !discard_unsaved {
                return Err(command_error(
                    operation_id,
                    "finalizing_log",
                    "unsaved_observations_confirmation_required",
                    false,
                    true,
                    state,
                ));
            }
        } else {
            mark_recovery_autosave_finalized(state);
        }
    }
    if page.is_none() && !discard_unsaved {
        mark_recovery_autosave_failure(state);
        return Err(command_error(
            operation_id,
            "finalizing_log",
            "unsaved_observations_confirmation_required",
            false,
            true,
            state,
        ));
    }
    emit_progress(
        app,
        operation_id,
        "stopping_capture",
        "Capture Proxyの中継を終了しています",
    );
    let (address, token) = management_connection(state).map_err(|_| {
        command_error(
            operation_id,
            "stopping_capture",
            "capture_not_running",
            false,
            false,
            state,
        )
    })?;
    let api = crate::moonbridge_api::ApiClient::new(state.client.clone(), address, token);
    let capture = api
        .post::<CaptureStatus>("/system/traffic-analysis/stop", serde_json::json!({}))
        .await
        .map_err(|_| {
            command_error(
                operation_id,
                "stopping_capture",
                "capture_stop_failed",
                false,
                true,
                state,
            )
        })?;
    if capture_relay_active(&capture) {
        return Err(command_error(
            operation_id,
            "stopping_capture",
            "capture_stop_failed",
            false,
            true,
            state,
        ));
    }
    if discard_unsaved {
        if let Ok(mut recovery) = read_recovery_state(state) {
            recovery.reconciliation_detail =
                Some("未保存ログを明示確認のうえ破棄しました".to_string());
            recovery.auto_log_status = Some("final_save_failed".to_string());
            recovery.unsaved_observations_may_remain = true;
            recovery.unsaved_discard_confirmed = true;
            let _ = write_recovery_state(state, &recovery);
        }
    }
    emit_progress(app, operation_id, "complete", "Capture Proxyを終了しました");
    let status = status(state).await.map_err(|_| {
        command_error(
            operation_id,
            "complete",
            "status_unavailable",
            false,
            false,
            state,
        )
    })?;
    Ok(TrafficAnalysisResult {
        operation_id: operation_id.to_string(),
        config_path: status.config_path.clone(),
        restart_codex_required: false,
        gateway_snapshot: snapshot(state).unwrap_or_else(|_| empty_gateway_snapshot()),
        status,
    })
}

pub async fn restore_config(
    app: &tauri::AppHandle,
    state: &GatewayStateStore,
    input: &RestoreInput,
) -> Result<TrafficAnalysisResult, TrafficCommandError> {
    let recovery = read_recovery_state(state).map_err(|_| {
        command_error(
            &input.operation_id,
            "restoring_config",
            "recovery_state_missing",
            false,
            false,
            state,
        )
    })?;
    let capture_is_active = if let Ok((address, token)) = management_connection(state) {
        let api = crate::moonbridge_api::ApiClient::new(state.client.clone(), address, token);
        api.get::<CaptureStatus>("/system/traffic-analysis/status")
            .await
            .is_ok_and(|capture| capture_relay_active(&capture))
    } else {
        false
    };
    if capture_is_active {
        emit_progress(
            app,
            &input.operation_id,
            "draining_capture",
            "新しい観測の記録を停止しています",
        );
        pause_capture(app, state, &input.operation_id).await?;
    }
    restore_active_config(
        app,
        state,
        &input.operation_id,
        &recovery,
        input.confirm_conflict,
    )
    .await
}

pub async fn clear(
    state: &GatewayStateStore,
    operation_id: &str,
) -> Result<TrafficAnalysisStatus, TrafficCommandError> {
    let (address, token) = management_connection(state).map_err(|_| {
        command_error(
            operation_id,
            "validating",
            "capture_not_running",
            false,
            false,
            state,
        )
    })?;
    let api = crate::moonbridge_api::ApiClient::new(state.client.clone(), address, token);
    api.post::<CaptureStatus>("/system/traffic-analysis/clear", serde_json::json!({}))
        .await
        .map_err(|_| {
            command_error(
                operation_id,
                "validating",
                "capture_clear_failed",
                false,
                true,
                state,
            )
        })?;
    status(state).await.map_err(|_| {
        command_error(
            operation_id,
            "complete",
            "status_unavailable",
            false,
            true,
            state,
        )
    })
}

pub async fn retry_autosave(
    state: &GatewayStateStore,
    operation_id: &str,
) -> Result<TrafficAnalysisStatus, TrafficCommandError> {
    let page = fetch_observations(state, 0).await.map_err(|_| {
        set_autosave_error(state, "autosave_read_failed");
        command_error(
            operation_id,
            "finalizing_log",
            "autosave_read_failed",
            false,
            true,
            state,
        )
    })?;
    sync_autosave(state, &page.observations, page.dropped).map_err(|_| {
        command_error(
            operation_id,
            "finalizing_log",
            "autosave_write_failed",
            false,
            true,
            state,
        )
    })?;
    finalize_autosave(state, &page.observations, page.dropped).map_err(|_| {
        command_error(
            operation_id,
            "finalizing_log",
            "autosave_finalize_failed",
            false,
            true,
            state,
        )
    })?;
    status(state).await.map_err(|_| {
        command_error(
            operation_id,
            "complete",
            "status_unavailable",
            false,
            true,
            state,
        )
    })
}

pub async fn observations(
    state: &GatewayStateStore,
    after: u64,
) -> Result<TrafficObservationPage, String> {
    let page = fetch_observations(state, after).await?;
    let _ = sync_autosave(state, &page.observations, page.dropped);
    Ok(page)
}

async fn fetch_observations(
    state: &GatewayStateStore,
    after: u64,
) -> Result<TrafficObservationPage, String> {
    let fetch_after = state
        .traffic_auto_save
        .lock()
        .map_err(|_| "traffic log state unavailable".to_string())?
        .as_ref()
        .filter(|session| !session.finalized)
        .map(|session| after.min(session.last_persisted_sequence))
        .unwrap_or(after);
    let (address, token) = management_connection(state)?;
    let api = crate::moonbridge_api::ApiClient::new(state.client.clone(), address, token);
    let page: serde_json::Value = api
        .get(&format!(
            "/system/traffic-analysis/observations?after={fetch_after}"
        ))
        .await
        .map_err(|_| "traffic observations unavailable".to_string())?;
    let observations = page
        .get("observations")
        .and_then(serde_json::Value::as_array)
        .cloned()
        .unwrap_or_default();
    let dropped = page
        .get("dropped")
        .and_then(serde_json::Value::as_u64)
        .unwrap_or_default();
    let last_sequence = observations
        .iter()
        .filter_map(|item| item.get("sequence").and_then(serde_json::Value::as_u64))
        .max()
        .unwrap_or(after);
    Ok(TrafficObservationPage {
        observations,
        dropped,
        last_sequence,
    })
}

fn traffic_log_directory(state: &GatewayStateStore) -> Result<PathBuf, String> {
    #[cfg(debug_assertions)]
    {
        let _ = state;
        let repository = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("..");
        return Ok(repository.join("logs").join("traffic-analysis"));
    }
    #[cfg(not(debug_assertions))]
    {
        Ok(state.app_paths()?.traffic_log_dir())
    }
}

const JOURNAL_MAX_BYTES: u64 = 1024 * 1024;

#[derive(Serialize)]
struct JournalRecord<'a> {
    schema: u8,
    t: String,
    session_id: Option<&'a str>,
    invocation_id: u64,
    kind: &'static str,
    command: &'a str,
    event: &'a str,
    result: Option<&'a str>,
}

/// Diagnostic JSONL journal for traffic-analysis commands. Records only
/// non-secret fields (command name, event, safe result code, session id,
/// timestamp). Never records payloads, auth, prompts, or error message text.
pub struct CommandJournal {
    path: PathBuf,
    counter: u64,
}

impl CommandJournal {
    pub fn disabled() -> Self {
        CommandJournal {
            path: PathBuf::new(),
            counter: 0,
        }
    }

    pub fn configure(&mut self, path: PathBuf) {
        self.path = path;
        self.counter = 0;
        self.rotate_if_needed();
    }

    pub fn next_id(&mut self) -> u64 {
        self.counter += 1;
        self.counter
    }

    pub fn journal(
        &mut self,
        invocation_id: u64,
        session_id: Option<&str>,
        command: &str,
        event: &str,
        result: Option<&str>,
    ) {
        if self.path.as_os_str().is_empty() {
            return;
        }
        self.rotate_if_needed();
        let record = JournalRecord {
            schema: 1,
            t: Utc::now().to_rfc3339(),
            session_id,
            invocation_id,
            kind: "command",
            command,
            event,
            result,
        };
        let mut line = serde_json::to_string(&record).unwrap_or_default();
        line.push('\n');
        if let Some(parent) = self.path.parent() {
            let _ = fs::create_dir_all(parent);
        }
        let _ = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.path)
            .and_then(|mut file| file.write_all(line.as_bytes()));
    }

    fn rotate_if_needed(&mut self) {
        if self.path.as_os_str().is_empty() {
            return;
        }
        let oversized = fs::metadata(&self.path)
            .map(|meta| meta.len() > JOURNAL_MAX_BYTES)
            .unwrap_or(false);
        if oversized {
            let stem = self
                .path
                .file_stem()
                .and_then(|name| name.to_str())
                .unwrap_or("command-journal");
            let rotated = self.path.with_file_name(format!("{stem}.1.jsonl"));
            // Overwrite a stale .1 instead of failing the rename on Windows
            // when the destination already exists (MoveFileExW uses
            // MOVEFILE_REPLACE_EXISTING; on other platforms a plain rename).
            let _ = replace_file(&self.path, &rotated);
        }
    }
}

pub fn next_journal_id(state: &GatewayStateStore) -> Option<u64> {
    state
        .command_journal
        .lock()
        .ok()
        .map(|mut journal| journal.next_id())
}

pub fn journal_command(
    state: &GatewayStateStore,
    invocation_id: u64,
    session_id: Option<&str>,
    command: &str,
    event: &str,
    result: Option<&str>,
) {
    if let Ok(mut journal) = state.command_journal.lock() {
        journal.journal(invocation_id, session_id, command, event, result);
    }
}

pub fn normalize_result(result: &Result<TrafficAnalysisResult, TrafficCommandError>) -> &str {
    match result {
        Ok(_) => "ok",
        Err(error) => match error.code.as_str() {
            "capture_pause_failed" | "capture_stop_failed" | "config_restore_failed" => {
                error.code.as_str()
            }
            _ => "unknown_error",
        },
    }
}

pub fn command_journal_path(state: &GatewayStateStore) -> Result<PathBuf, String> {
    #[cfg(debug_assertions)]
    {
        let _ = state;
        let repository = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("..");
        Ok(repository.join("logs").join("command-journal.jsonl"))
    }
    #[cfg(not(debug_assertions))]
    {
        Ok(state.app_paths()?.command_journal_path())
    }
}

fn initialize_autosave(state: &GatewayStateStore) -> Result<(), String> {
    let directory = traffic_log_directory(state)?;
    fs::create_dir_all(&directory).map_err(|_| "create traffic log directory failed")?;
    let destination = new_traffic_log_path(&directory)?;
    let started_at = Utc::now().to_rfc3339();
    let session_id = Uuid::new_v4().simple().to_string();
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&destination)
        .map_err(|_| "create traffic log failed")?;
    let header = render_log_header(&session_id, &started_at);
    if file.write_all(header.as_bytes()).is_err() || file.sync_all().is_err() {
        let _ = fs::remove_file(&destination);
        return Err("write traffic log header failed".to_string());
    }
    let session = TrafficAutoSaveSession {
        session_id,
        destination,
        started_at,
        last_persisted_sequence: 0,
        observations_written: 0,
        sequence_gaps: 0,
        last_synced_at: None,
        finalized: false,
        last_safe_error: None,
        dropped_observations: 0,
        last_checkpoint_sequence: 0,
        last_checkpoint_at: None,
    };
    *state
        .traffic_auto_save
        .lock()
        .map_err(|_| "traffic log state unavailable")? = Some(session);
    retain_traffic_logs(&directory);
    Ok(())
}

fn abort_autosave(state: &GatewayStateStore) {
    if let Ok(mut guard) = state.traffic_auto_save.lock() {
        if let Some(session) = guard.take() {
            let _ = fs::remove_file(session.destination);
        }
    }
}

fn new_traffic_log_path(directory: &Path) -> Result<PathBuf, String> {
    let stamp = Local::now().format("%Y%m%d-%H%M%S").to_string();
    for suffix in 0..1000u32 {
        let name = if suffix == 0 {
            format!("traffic-analysis-{stamp}.log")
        } else {
            format!("traffic-analysis-{stamp}-{suffix:03}.log")
        };
        let path = directory.join(name);
        if !path.exists() {
            return Ok(path);
        }
    }
    Err("traffic log filename collision limit reached".to_string())
}

fn sync_autosave(
    state: &GatewayStateStore,
    observations: &[serde_json::Value],
    dropped: u64,
) -> Result<(), String> {
    let mut guard = state
        .traffic_auto_save
        .lock()
        .map_err(|_| "traffic log state unavailable".to_string())?;
    let Some(session) = guard.as_mut() else {
        return Ok(());
    };
    session.dropped_observations = dropped;
    if session.finalized {
        return Ok(());
    }
    let mut candidates = observations
        .iter()
        .filter(|observation| safe_u64(observation, "sequence") > session.last_persisted_sequence)
        .collect::<Vec<_>>();
    candidates.sort_by_key(|observation| safe_u64(observation, "sequence"));
    let mut pending = Vec::new();
    let mut next_sequence = session.last_persisted_sequence;
    let mut gaps = 0u64;
    for observation in candidates {
        let sequence = safe_u64(observation, "sequence");
        if sequence > next_sequence.saturating_add(1) {
            gaps = gaps.saturating_add(sequence - next_sequence - 1);
        }
        pending.push((sequence, observation));
        next_sequence = sequence;
    }
    if pending.is_empty() {
        return Ok(());
    }
    let mut output = String::new();
    if gaps > 0 {
        output.push_str(&format!(
            "Gap: {gaps} observation sequence(s) unavailable\n\n"
        ));
    }
    for (_, observation) in &pending {
        output.push_str(&render_observation_log_entry(observation));
    }
    let result = (|| {
        let mut file = OpenOptions::new()
            .append(true)
            .open(&session.destination)
            .map_err(|_| "open traffic log failed")?;
        file.write_all(output.as_bytes())
            .map_err(|_| "append traffic log failed")?;
        file.sync_data().map_err(|_| "sync traffic log failed")?;
        Ok::<(), &str>(())
    })();
    if result.is_err() {
        session.last_safe_error = Some(auto_save_error("autosave_write_failed"));
        return Err("autosave_write_failed".to_string());
    }
    session.last_persisted_sequence = next_sequence;
    session.observations_written = session
        .observations_written
        .saturating_add(pending.len() as u64);
    session.sequence_gaps = session.sequence_gaps.saturating_add(gaps);
    let synced_at = Utc::now().to_rfc3339();
    session.last_synced_at = Some(synced_at.clone());
    session.last_safe_error = None;
    session.last_checkpoint_sequence = session.last_persisted_sequence;
    session.last_checkpoint_at = Some(synced_at);
    drop(guard);
    if checkpoint_recovery_state(state).is_err() {
        if let Ok(mut guard) = state.traffic_auto_save.lock() {
            if let Some(session) = guard.as_mut() {
                session.last_safe_error = Some(auto_save_error("recovery_checkpoint_failed"));
            }
        }
    }
    Ok(())
}

fn checkpoint_recovery_state(state: &GatewayStateStore) -> Result<(), String> {
    let mut recovery = read_recovery_state(state)?;
    recovery.auto_log = autosave_recovery_state(state);
    write_recovery_state(state, &recovery)
}

fn finalize_autosave(
    state: &GatewayStateStore,
    observations: &[serde_json::Value],
    dropped: u64,
) -> Result<(), String> {
    sync_autosave(state, observations, dropped)?;
    let mut guard = state
        .traffic_auto_save
        .lock()
        .map_err(|_| "traffic log state unavailable".to_string())?;
    let Some(session) = guard.as_mut() else {
        return Ok(());
    };
    if session.finalized {
        return Ok(());
    }
    let footer = render_log_footer(
        &session.session_id,
        &session.started_at,
        &Utc::now().to_rfc3339(),
        session.observations_written,
        session.sequence_gaps,
        session.dropped_observations,
    );
    let result = (|| {
        let mut file = OpenOptions::new()
            .append(true)
            .open(&session.destination)
            .map_err(|_| "open traffic log failed")?;
        file.write_all(footer.as_bytes())
            .map_err(|_| "append traffic log footer failed")?;
        file.sync_all()
            .map_err(|_| "sync traffic log footer failed")?;
        Ok::<(), &str>(())
    })();
    if result.is_err() {
        session.last_safe_error = Some(auto_save_error("autosave_finalize_failed"));
        return Err("autosave_finalize_failed".to_string());
    }
    session.finalized = true;
    session.last_synced_at = Some(Utc::now().to_rfc3339());
    session.last_safe_error = None;
    retain_traffic_logs(
        session
            .destination
            .parent()
            .unwrap_or_else(|| Path::new(".")),
    );
    Ok(())
}

fn auto_save_error(code: &str) -> AutoSaveError {
    AutoSaveError {
        code: code.to_string(),
        message: match code {
            "autosave_finalize_failed" => {
                "分析ログの最終保存に失敗しました。再試行してください".to_string()
            }
            _ => "分析ログの自動保存に失敗しました。通信は継続しています".to_string(),
        },
        retryable: true,
    }
}

fn set_autosave_error(state: &GatewayStateStore, code: &str) {
    if let Ok(mut guard) = state.traffic_auto_save.lock() {
        if let Some(session) = guard.as_mut() {
            session.last_safe_error = Some(auto_save_error(code));
        }
    }
}

fn retain_traffic_logs(directory: &Path) {
    let Ok(entries) = fs::read_dir(directory) else {
        return;
    };
    let mut paths = entries
        .filter_map(Result::ok)
        .filter_map(|entry| {
            let path = entry.path();
            let metadata = fs::symlink_metadata(&path).ok()?;
            if !metadata.file_type().is_file() || !is_traffic_log_name(path.file_name()?.to_str()?)
            {
                return None;
            }
            Some(path)
        })
        .collect::<Vec<_>>();
    paths.sort_by_key(|path| path.file_name().map(|name| name.to_os_string()));
    if paths.len() > 30 {
        let remove_count = paths.len() - 30;
        for path in paths.into_iter().take(remove_count) {
            let _ = fs::remove_file(path);
        }
    }
}

fn is_traffic_log_name(name: &str) -> bool {
    let Some(stem) = name.strip_suffix(".log") else {
        return false;
    };
    let Some(stamp) = stem.strip_prefix("traffic-analysis-") else {
        return false;
    };
    let parts = stamp.split('-').collect::<Vec<_>>();
    (parts.len() == 2 && parts[0].len() == 8 && parts[1].len() == 6
        || parts.len() == 3 && parts[0].len() == 8 && parts[1].len() == 6 && parts[2].len() == 3)
        && parts
            .iter()
            .all(|part| part.chars().all(|ch| ch.is_ascii_digit()))
}

pub async fn export(
    app: &tauri::AppHandle,
    state: &GatewayStateStore,
    operation_id: &str,
    destination: &str,
) -> Result<TrafficExportResult, TrafficCommandError> {
    let mut destination_path = PathBuf::from(destination);
    if !destination_path.is_absolute() || destination_path.file_name().is_none() {
        return Err(command_error(
            operation_id,
            "exporting",
            "export_path_invalid",
            false,
            false,
            state,
        ));
    }
    destination_path = normalize_export_path(destination_path)
        .map_err(|code| command_error(operation_id, "exporting", &code, false, false, state))?;
    emit_progress(app, operation_id, "exporting", "安全化観測を出力しています");
    let page = observations(state, 0).await.map_err(|_| {
        command_error(
            operation_id,
            "exporting",
            "observations_unavailable",
            false,
            false,
            state,
        )
    })?;
    let observation_count = page.observations.len();
    let payload = render_export_log(&page.observations, page.dropped);
    publish_config(&destination_path, &payload).map_err(|_| {
        command_error(
            operation_id,
            "exporting",
            "export_failed",
            false,
            false,
            state,
        )
    })?;
    let recorded_path = fs::canonicalize(&destination_path).map_err(|_| {
        command_error(
            operation_id,
            "exporting",
            "export_failed",
            false,
            false,
            state,
        )
    })?;
    state
        .remember_traffic_export(recorded_path.clone())
        .map_err(|_| {
            command_error(
                operation_id,
                "exporting",
                "export_failed",
                false,
                false,
                state,
            )
        })?;
    Ok(TrafficExportResult {
        operation_id: operation_id.to_string(),
        destination: recorded_path.display().to_string(),
        observation_count,
    })
}

pub async fn reveal_export(
    state: &GatewayStateStore,
    operation_id: &str,
    destination: &str,
) -> Result<TrafficRevealResult, TrafficCommandError> {
    if operation_id.trim().is_empty() {
        return Err(command_error(
            operation_id,
            "exporting",
            "invalid_operation_id",
            false,
            false,
            state,
        ));
    }
    let requested = normalize_export_path(PathBuf::from(destination))
        .map_err(|code| command_error(operation_id, "exporting", &code, false, false, state))?;
    let requested = fs::canonicalize(&requested).map_err(|_| {
        command_error(
            operation_id,
            "exporting",
            "export_missing",
            false,
            false,
            state,
        )
    })?;
    let recorded = state.last_traffic_export().map_err(|_| {
        command_error(
            operation_id,
            "exporting",
            "export_not_owned",
            false,
            false,
            state,
        )
    })?;
    if !is_recorded_export(&requested, recorded.as_deref()) {
        return Err(command_error(
            operation_id,
            "exporting",
            "export_not_owned",
            false,
            false,
            state,
        ));
    }
    #[cfg(windows)]
    {
        std::process::Command::new("explorer.exe")
            .arg(format!("/select,{}", requested.display()))
            .spawn()
            .map_err(|_| {
                command_error(
                    operation_id,
                    "exporting",
                    "reveal_failed",
                    false,
                    false,
                    state,
                )
            })?;
    }
    #[cfg(not(windows))]
    {
        return Err(command_error(
            operation_id,
            "exporting",
            "reveal_unsupported",
            false,
            false,
            state,
        ));
    }
    Ok(TrafficRevealResult {
        operation_id: operation_id.to_string(),
        destination: requested.display().to_string(),
    })
}

pub fn open_log_folder(state: &GatewayStateStore) -> Result<(), TrafficCommandError> {
    let directory = traffic_log_directory(state).map_err(|_| {
        command_error(
            "",
            "exporting",
            "log_folder_unavailable",
            false,
            false,
            state,
        )
    })?;
    fs::create_dir_all(&directory).map_err(|_| {
        command_error(
            "",
            "exporting",
            "log_folder_unavailable",
            false,
            false,
            state,
        )
    })?;
    #[cfg(windows)]
    {
        std::process::Command::new("explorer.exe")
            .arg(directory.as_os_str())
            .spawn()
            .map_err(|_| command_error("", "exporting", "reveal_failed", false, false, state))?;
        Ok(())
    }
    #[cfg(not(windows))]
    {
        let _ = directory;
        Err(command_error(
            "",
            "exporting",
            "reveal_unsupported",
            false,
            false,
            state,
        ))
    }
}

fn normalize_export_path(mut path: PathBuf) -> Result<PathBuf, String> {
    if !path.is_absolute() || path.file_name().is_none() {
        return Err("export_path_invalid".to_string());
    }
    match path.extension().and_then(|value| value.to_str()) {
        None => {
            path.set_extension("log");
        }
        Some(extension) if extension.eq_ignore_ascii_case("log") => {}
        Some(_) => return Err("export_path_invalid".to_string()),
    }
    if path.exists() && path.is_dir() {
        return Err("export_path_invalid".to_string());
    }
    Ok(path)
}

fn is_recorded_export(requested: &Path, recorded: Option<&Path>) -> bool {
    recorded == Some(requested)
}

fn render_export_log(observations: &[serde_json::Value], dropped: u64) -> Vec<u8> {
    let mut output = render_log_header("manual-export", &Utc::now().to_rfc3339());
    output.push_str(&format!("Exported-At: {}\n", Utc::now().to_rfc3339()));
    output.push_str(&format!("Observations: {}\n", observations.len()));
    output.push_str(&format!("Dropped: {dropped}\n\n"));
    for observation in observations {
        output.push_str(&render_observation_log_entry(observation));
    }
    output.into_bytes()
}

fn render_log_header(session_id: &str, started_at: &str) -> String {
    format!(
        "Moon Bridge Codex Traffic Analysis\nSession-ID: {session_id}\nStarted-At: {started_at}\nStatus: active\n\n"
    )
}

fn render_observation_log_entry(observation: &serde_json::Value) -> String {
    let timestamp = safe_string(observation, "timestamp");
    let sequence = safe_u64(observation, "sequence");
    let direction = safe_string(observation, "direction");
    let transport = safe_string(observation, "transport");
    let mut output = format!("{timestamp} #{sequence} {direction} {transport}\n");
    append_string_field(&mut output, "  method", observation, "method");
    append_string_field(&mut output, "  received_path", observation, "receivedPath");
    append_string_field(&mut output, "  upstream_path", observation, "upstreamPath");
    append_number_field(&mut output, "  status_code", observation, "statusCode");
    append_string_field(&mut output, "  content_type", observation, "contentType");
    append_string_field(
        &mut output,
        "  content_encoding",
        observation,
        "contentEncoding",
    );
    append_string_field(&mut output, "  payload_kind", observation, "payloadKind");
    append_number_field(
        &mut output,
        "  raw_payload_size",
        observation,
        "rawPayloadSize",
    );
    append_number_field(
        &mut output,
        "  decoded_size",
        observation,
        "decodedObservationSize",
    );
    append_string_field(
        &mut output,
        "  decoding_status",
        observation,
        "decodingStatus",
    );
    append_string_field(&mut output, "  sse_event", observation, "sseEventType");
    append_string_field(
        &mut output,
        "  websocket_message",
        observation,
        "websocketMessageType",
    );
    append_string_field(&mut output, "  disposition", observation, "disposition");
    append_string_field(&mut output, "  error_class", observation, "errorClass");
    let opaque_count = observation
        .get("opaqueFields")
        .and_then(serde_json::Value::as_array)
        .map_or(0, Vec::len);
    output.push_str(&format!("  opaque_fields: {opaque_count}\n"));
    if let Some(shape) = observation.get("payloadShape") {
        append_string_field_from(&mut output, "  model", shape, "modelValue");
        append_string_field_from(&mut output, "  reasoning", shape, "reasoningEffort");
        append_string_field_from(&mut output, "  event_type", shape, "eventType");
        append_string_field_from(&mut output, "  object_type", shape, "objectType");
        append_string_field_from(&mut output, "  status", shape, "status");
        if let Some(fields) = shape
            .get("topLevelFields")
            .and_then(serde_json::Value::as_array)
        {
            let safe_fields = fields
                .iter()
                .filter_map(serde_json::Value::as_str)
                .collect::<Vec<_>>()
                .join(",");
            if !safe_fields.is_empty() {
                output.push_str(&format!("  fields: {safe_fields}\n"));
            }
        }
    }
    output.push('\n');
    output
}

fn render_log_footer(
    session_id: &str,
    started_at: &str,
    ended_at: &str,
    observations: u64,
    gaps: u64,
    dropped: u64,
) -> String {
    format!(
        "Status: completed\nSession-ID: {session_id}\nStarted-At: {started_at}\nEnded-At: {ended_at}\nObservations: {observations}\nSequence-Gaps: {gaps}\nDropped: {dropped}\n"
    )
}

fn safe_string(value: &serde_json::Value, key: &str) -> String {
    value
        .get(key)
        .and_then(serde_json::Value::as_str)
        .unwrap_or("-")
        .to_string()
}

fn safe_u64(value: &serde_json::Value, key: &str) -> u64 {
    value
        .get(key)
        .and_then(serde_json::Value::as_u64)
        .unwrap_or_default()
}

fn append_string_field(output: &mut String, label: &str, value: &serde_json::Value, key: &str) {
    append_string_field_from(output, label, value, key);
}

fn append_string_field_from(
    output: &mut String,
    label: &str,
    value: &serde_json::Value,
    key: &str,
) {
    if let Some(text) = value.get(key).and_then(serde_json::Value::as_str) {
        output.push_str(&format!("{label}: {text}\n"));
    }
}

fn append_number_field(output: &mut String, label: &str, value: &serde_json::Value, key: &str) {
    if let Some(number) = value.get(key).and_then(serde_json::Value::as_u64) {
        output.push_str(&format!("{label}: {number}\n"));
    }
}

async fn restore_active_config(
    app: &tauri::AppHandle,
    state: &GatewayStateStore,
    operation_id: &str,
    recovery: &RecoveryState,
    confirm_conflict: bool,
) -> Result<TrafficAnalysisResult, TrafficCommandError> {
    let config_path = PathBuf::from(&recovery.config_path);
    let current = read_config(&config_path).map_err(|_| {
        command_error(
            operation_id,
            "restoring_config",
            "config_parse_failed",
            false,
            true,
            state,
        )
    })?;
    let external_change = hash_bytes(&current.bytes) != recovery.config_hash_after_apply;
    let current_url = current.previous_url.as_deref();
    let applied_url = Some(recovery.applied_openai_base_url.as_str());
    let previous_url = recovery.previous_openai_base_url.as_deref();
    let should_publish = restoration_requires_publish(
        current_url,
        applied_url,
        previous_url,
        external_change,
        confirm_conflict,
    )
    .map_err(|_| {
        command_error(
            operation_id,
            "restoring_config",
            "config_conflict",
            false,
            true,
            state,
        )
    })?;
    emit_progress(
        app,
        operation_id,
        "restoring_config",
        "openai_base_urlを元の値へ戻しています",
    );
    if should_publish {
        let candidate = render_with_previous_url(&current.document, previous_url);
        if recovery.previous_openai_base_url.is_none()
            && recovery.backup_path.is_none()
            && candidate.iter().all(u8::is_ascii_whitespace)
        {
            fs::remove_file(&config_path).map_err(|_| {
                command_error(
                    operation_id,
                    "restoring_config",
                    "config_publish_failed",
                    true,
                    true,
                    state,
                )
            })?;
        } else {
            publish_config(&config_path, &candidate).map_err(|_| {
                command_error(
                    operation_id,
                    "restoring_config",
                    "config_publish_failed",
                    true,
                    true,
                    state,
                )
            })?;
        }
    }
    verify_config(&config_path, recovery.previous_openai_base_url.as_deref()).map_err(|_| {
        command_error(
            operation_id,
            "verifying_config",
            "config_verification_failed",
            true,
            true,
            state,
        )
    })?;
    let mut inactive = recovery.clone();
    inactive.integration_active = false;
    write_recovery_state(state, &inactive).map_err(|_| {
        command_error(
            operation_id,
            "complete",
            "recovery_state_failed",
            true,
            false,
            state,
        )
    })?;
    emit_progress(
        app,
        operation_id,
        "complete",
        "Codex設定を復元しました。Codexを再起動後、中継を終了してください",
    );
    let status = status(state).await.map_err(|_| {
        command_error(
            operation_id,
            "complete",
            "status_unavailable",
            true,
            false,
            state,
        )
    })?;
    Ok(TrafficAnalysisResult {
        operation_id: operation_id.to_string(),
        status,
        config_path: config_path.display().to_string(),
        restart_codex_required: true,
        gateway_snapshot: snapshot(state).unwrap_or_else(|_| empty_gateway_snapshot()),
    })
}

async fn pause_capture(
    _app: &tauri::AppHandle,
    state: &GatewayStateStore,
    operation_id: &str,
) -> Result<CaptureStatus, TrafficCommandError> {
    let (address, token) = management_connection(state).map_err(|_| {
        command_error(
            operation_id,
            "draining_capture",
            "capture_not_ready",
            false,
            true,
            state,
        )
    })?;
    let api = crate::moonbridge_api::ApiClient::new(state.client.clone(), address, token);
    let pause_result = tokio::time::timeout(
        std::time::Duration::from_secs(6),
        api.post::<CaptureStatus>("/system/traffic-analysis/pause", serde_json::json!({})),
    )
    .await;
    let _pause_response = match pause_result {
        Ok(Ok(value)) => value,
        Ok(Err(error)) => {
            let code = error.code.as_deref().unwrap_or("capture_pause_failed");
            return Err(command_error(
                operation_id,
                "draining_capture",
                code,
                false,
                true,
                state,
            ));
        }
        Err(_) => {
            match api
                .get::<CaptureStatus>("/system/traffic-analysis/status")
                .await
            {
                Ok(value) if value.state == "passthrough" => value,
                Ok(value) if value.state == "capturing" || value.state == "draining" => {
                    return Err(command_error(
                        operation_id,
                        "draining_capture",
                        "capture_pause_drain_timeout",
                        false,
                        true,
                        state,
                    ));
                }
                Ok(_) => {
                    return Err(command_error(
                        operation_id,
                        "draining_capture",
                        "capture_pause_status_unavailable",
                        false,
                        true,
                        state,
                    ));
                }
                Err(_) => {
                    return Err(command_error(
                        operation_id,
                        "draining_capture",
                        "capture_pause_status_unavailable",
                        false,
                        true,
                        state,
                    ));
                }
            }
        }
    };
    let capture = match tokio::time::timeout(
        std::time::Duration::from_secs(6),
        api.get::<CaptureStatus>("/system/traffic-analysis/status"),
    )
    .await
    {
        Ok(Ok(value)) => value,
        _ => {
            return Err(command_error(
                operation_id,
                "draining_capture",
                "capture_pause_status_unavailable",
                false,
                true,
                state,
            ));
        }
    };
    if capture.instance_id != snapshot(state).ok().and_then(|value| value.instance_id) {
        return Err(command_error(
            operation_id,
            "draining_capture",
            "capture_instance_mismatch",
            false,
            true,
            state,
        ));
    }
    if capture.state != "passthrough" {
        return Err(command_error(
            operation_id,
            "draining_capture",
            "capture_pause_status_unavailable",
            false,
            capture_relay_active(&capture),
            state,
        ));
    }
    Ok(capture)
}

struct ReadConfig {
    bytes: Vec<u8>,
    document: DocumentMut,
    previous_url: Option<String>,
    existed: bool,
}

fn read_config(path: &Path) -> Result<ReadConfig, String> {
    if !path.exists() {
        return Ok(ReadConfig {
            bytes: Vec::new(),
            document: DocumentMut::new(),
            previous_url: None,
            existed: false,
        });
    }
    let bytes = fs::read(path).map_err(|_| "read config failed".to_string())?;
    let text = String::from_utf8(bytes.clone()).map_err(|_| "config is not UTF-8".to_string())?;
    let document = text
        .parse::<DocumentMut>()
        .map_err(|_| "config TOML parse failed".to_string())?;
    let previous_url = document
        .get("openai_base_url")
        .and_then(|item| item.as_value())
        .and_then(|item| item.as_str())
        .map(ToOwned::to_owned);
    if document.get("openai_base_url").is_some() && previous_url.is_none() {
        return Err("openai_base_url must be a string".to_string());
    }
    Ok(ReadConfig {
        bytes,
        document,
        previous_url,
        existed: true,
    })
}

fn restore_original_config(path: &Path, original: &ReadConfig) -> Result<(), String> {
    if original.existed {
        publish_config(path, &original.bytes)
    } else if path.exists() {
        fs::remove_file(path).map_err(|_| "remove newly created config failed".to_string())
    } else {
        Ok(())
    }
}

fn render_with_base_url(document: &DocumentMut, url: &str) -> Vec<u8> {
    let mut document = document.clone();
    document["openai_base_url"] = value(url);
    document.to_string().into_bytes()
}
fn render_with_previous_url(document: &DocumentMut, url: Option<&str>) -> Vec<u8> {
    let mut document = document.clone();
    match url {
        Some(url) => document["openai_base_url"] = value(url),
        None => {
            document.remove("openai_base_url");
        }
    };
    document.to_string().into_bytes()
}

fn verify_config(path: &Path, expected: Option<&str>) -> Result<(), String> {
    if !path.exists() {
        return if expected.is_none() {
            Ok(())
        } else {
            Err("published config verification mismatch".to_string())
        };
    }
    let content =
        fs::read_to_string(path).map_err(|_| "read published config failed".to_string())?;
    let document = content
        .parse::<DocumentMut>()
        .map_err(|_| "published config parse failed".to_string())?;
    let actual = document
        .get("openai_base_url")
        .and_then(|item| item.as_value())
        .and_then(|item| item.as_str());
    if actual == expected {
        Ok(())
    } else {
        Err("published config verification mismatch".to_string())
    }
}

fn create_backup(
    state: &GatewayStateStore,
    path: &Path,
    bytes: &[u8],
    existed: bool,
) -> Result<Option<PathBuf>, String> {
    if !existed && !path.exists() {
        return Ok(None);
    }
    let root = state.app_paths()?.backup_dir();
    fs::create_dir_all(&root).map_err(|_| "create backup directory failed".to_string())?;
    let backup = root.join(format!(
        "{}-config.toml",
        Utc::now().format("%Y%m%dT%H%M%S%3fZ")
    ));
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&backup)
        .map_err(|_| "create backup failed".to_string())?;
    file.write_all(bytes)
        .map_err(|_| "write backup failed".to_string())?;
    file.sync_all()
        .map_err(|_| "sync backup failed".to_string())?;
    retain_config_backups(&root, Some(&backup));
    Ok(Some(backup))
}

fn publish_config(path: &Path, bytes: &[u8]) -> Result<(), String> {
    #[cfg(test)]
    if consume_injected_publish_failure(path) {
        return Err("injected config publication failure".to_string());
    }
    let parent = path.parent().ok_or("config parent missing")?;
    fs::create_dir_all(parent).map_err(|_| "create config directory failed".to_string())?;
    let temporary = parent.join(format!(".config.toml.{}.tmp", Uuid::new_v4().simple()));
    let result = (|| {
        let mut file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temporary)
            .map_err(|_| "create config temporary failed".to_string())?;
        file.write_all(bytes)
            .map_err(|_| "write config temporary failed".to_string())?;
        file.sync_all()
            .map_err(|_| "sync config temporary failed".to_string())?;
        drop(file);
        replace_file(&temporary, path).map_err(|_| "replace config failed".to_string())?;
        sync_parent_directory(parent).map_err(|_| "sync config directory failed".to_string())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result
}

#[cfg(test)]
fn inject_publish_failure_once(path: &Path) {
    let failures = INJECTED_PUBLISH_FAILURES.get_or_init(|| Mutex::new(Vec::new()));
    failures
        .lock()
        .expect("lock injected publication failures")
        .push(path.to_path_buf());
}

#[cfg(test)]
fn consume_injected_publish_failure(path: &Path) -> bool {
    let Some(failures) = INJECTED_PUBLISH_FAILURES.get() else {
        return false;
    };
    let mut failures = failures.lock().expect("lock injected publication failures");
    if let Some(index) = failures.iter().position(|candidate| candidate == path) {
        failures.remove(index);
        true
    } else {
        false
    }
}

fn retain_config_backups(directory: &Path, protected: Option<&Path>) {
    let mut backups = fs::read_dir(directory)
        .ok()
        .into_iter()
        .flatten()
        .filter_map(Result::ok)
        .filter_map(|entry| {
            let file_type = entry.file_type().ok()?;
            if !file_type.is_file() || file_type.is_symlink() {
                return None;
            }
            let name = entry.file_name().to_string_lossy().to_string();
            if !is_config_backup_name(&name) {
                return None;
            }
            let modified = entry.metadata().ok()?.modified().ok()?;
            Some((modified, entry.path()))
        })
        .collect::<Vec<_>>();
    backups.sort_by(|left, right| right.0.cmp(&left.0));
    let mut kept = 0usize;
    for (_, path) in backups {
        if protected == Some(path.as_path()) {
            continue;
        }
        kept += 1;
        if kept <= 5 {
            continue;
        }
        let _ = fs::remove_file(path);
    }
}

fn is_config_backup_name(name: &str) -> bool {
    let Some(stem) = name.strip_suffix("-config.toml") else {
        return false;
    };
    stem.len() == 19
        && stem.chars().enumerate().all(|(index, value)| {
            (index == 8 && value == 'T')
                || (index == 18 && value == 'Z')
                || (index != 8 && index != 18 && value.is_ascii_digit())
        })
}

fn resolve_config_path() -> Result<PathBuf, String> {
    let home = std::env::var_os("CODEX_HOME")
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
        .or_else(|| {
            std::env::var_os("USERPROFILE").map(|value| PathBuf::from(value).join(".codex"))
        })
        .ok_or("Codex home is unavailable")?;
    if !home.is_absolute() {
        return Err("Codex home must be absolute".to_string());
    }
    reject_reparse(&home)?;
    let home = if home.exists() {
        fs::canonicalize(&home).map_err(|_| "Codex home could not be canonicalized".to_string())?
    } else if let Some(parent) = home.parent() {
        reject_reparse(parent)?;
        if parent.exists() {
            fs::canonicalize(parent)
                .map_err(|_| "Codex home parent could not be canonicalized".to_string())?
                .join(home.file_name().ok_or("Codex home name is missing")?)
        } else {
            home
        }
    } else {
        home
    };
    let config_path = home.join("config.toml");
    reject_reparse(&config_path)?;
    Ok(config_path)
}

fn reject_reparse(path: &Path) -> Result<(), String> {
    if let Ok(metadata) = fs::symlink_metadata(path) {
        if metadata.file_type().is_symlink() {
            return Err("Codex home symlink is unsupported".to_string());
        }
        #[cfg(windows)]
        {
            use std::os::windows::fs::MetadataExt;
            if metadata.file_attributes() & 0x400 != 0 {
                return Err("Codex home reparse point is unsupported".to_string());
            }
        }
    }
    Ok(())
}

fn read_recovery_state(state: &GatewayStateStore) -> Result<RecoveryState, String> {
    let paths = state.app_paths()?;
    let current = paths.recovery_state_path();
    if current.is_file() {
        let content =
            fs::read_to_string(&current).map_err(|_| "recovery state unavailable".to_string())?;
        let mut recovery: RecoveryState =
            serde_json::from_str(&content).map_err(|_| "recovery state invalid".to_string())?;
        if recovery.schema_version < 2 {
            recovery =
                migrate_recovery_state(state, recovery, current.clone(), content.into_bytes())?;
        }
        return Ok(recovery);
    }
    let legacy = paths.legacy_recovery_state_path()?;
    let content =
        fs::read_to_string(&legacy).map_err(|_| "recovery state unavailable".to_string())?;
    let legacy_state: RecoveryState =
        serde_json::from_str(&content).map_err(|_| "recovery state invalid".to_string())?;
    migrate_recovery_state(state, legacy_state, legacy, content.into_bytes())
}

fn migrate_recovery_state(
    state: &GatewayStateStore,
    mut recovery: RecoveryState,
    source_path: PathBuf,
    source_bytes: Vec<u8>,
) -> Result<RecoveryState, String> {
    let paths = state.app_paths()?;
    let source_schema_version = recovery.schema_version;
    recovery.schema_version = 2;
    recovery.phase = if recovery.integration_active {
        "integration_applied".to_string()
    } else {
        "recovered".to_string()
    };
    recovery.updated_at = Some(Utc::now().to_rfc3339());
    recovery.migration = Some(RecoveryMigrationState {
        source_path: source_path.display().to_string(),
        source_schema_version,
        migrated_at: Utc::now().to_rfc3339(),
    });
    write_recovery_state(state, &recovery)?;
    let archive_dir = paths.recovery_dir().join("migrated-v1");
    fs::create_dir_all(&archive_dir).map_err(|_| "create migration archive failed".to_string())?;
    let archive_path = unique_migration_archive_path(&archive_dir);
    write_new_file_sync(&archive_path, &source_bytes)
        .map_err(|_| "archive legacy recovery state failed".to_string())?;
    Ok(recovery)
}

fn unique_migration_archive_path(directory: &Path) -> PathBuf {
    let stamp = Utc::now().format("%Y%m%dT%H%M%S%3fZ").to_string();
    for suffix in 0..1000u32 {
        let name = if suffix == 0 {
            format!("integration-state-{stamp}.json")
        } else {
            format!("integration-state-{stamp}-{suffix:03}.json")
        };
        let candidate = directory.join(name);
        if !candidate.exists() {
            return candidate;
        }
    }
    directory.join(format!("integration-state-{stamp}-overflow.json"))
}

fn write_new_file_sync(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    let mut file = OpenOptions::new().create_new(true).write(true).open(path)?;
    file.write_all(bytes)?;
    file.sync_all()
}

fn write_recovery_state(state: &GatewayStateStore, recovery: &RecoveryState) -> Result<(), String> {
    let paths = state.app_paths()?;
    fs::create_dir_all(paths.recovery_dir())
        .map_err(|_| "create recovery directory failed".to_string())?;
    let mut recovery = recovery.clone();
    recovery.schema_version = 2;
    recovery.updated_at = Some(Utc::now().to_rfc3339());
    let path = paths.recovery_state_path();
    let bytes = serde_json::to_vec_pretty(&recovery)
        .map_err(|_| "serialize recovery state failed".to_string())?;
    publish_config(&path, &bytes)
}

fn autosave_recovery_state(state: &GatewayStateStore) -> Option<AutoLogRecoveryState> {
    state.traffic_auto_save.lock().ok().and_then(|guard| {
        guard.as_ref().map(|session| AutoLogRecoveryState {
            session_id: session.session_id.clone(),
            path: session.destination.display().to_string(),
            last_checkpoint_sequence: session.last_checkpoint_sequence,
            finalized: session.finalized,
            last_checkpoint_at: session.last_checkpoint_at.clone(),
        })
    })
}

fn mark_recovery_autosave_failure(state: &GatewayStateStore) {
    if let Ok(mut recovery) = read_recovery_state(state) {
        recovery.auto_log_status = Some("final_save_failed".to_string());
        recovery.unsaved_observations_may_remain = true;
        recovery.unsaved_discard_confirmed = false;
        let _ = write_recovery_state(state, &recovery);
    }
}

fn mark_recovery_autosave_finalized(state: &GatewayStateStore) {
    if let Ok(mut recovery) = read_recovery_state(state) {
        recovery.auto_log_status = Some("finalized".to_string());
        recovery.unsaved_observations_may_remain = false;
        recovery.unsaved_discard_confirmed = false;
        let _ = write_recovery_state(state, &recovery);
    }
}

fn hash_bytes(bytes: &[u8]) -> String {
    let mut digest = Sha256::new();
    digest.update(bytes);
    format!("{:x}", digest.finalize())
}

async fn cleanup_start_failure(
    app: &tauri::AppHandle,
    state: &GatewayStateStore,
    operation_id: &str,
    gateway_started: bool,
    stage: &str,
    code: &str,
) -> TrafficCommandError {
    let capture_running = finish_relay(app, state, operation_id, true).await.is_err();
    mark_recovery_aborted(state);
    abort_autosave(state);
    if gateway_started {
        let _ = stop_gateway_inner(state).await;
    }
    emit_progress(
        app,
        operation_id,
        stage,
        "分析開始を中止しました。Codex設定は変更していません",
    );
    command_error(operation_id, stage, code, false, capture_running, state)
}

fn mark_recovery_aborted(state: &GatewayStateStore) {
    let Ok(mut recovery) = read_recovery_state(state) else {
        return;
    };
    recovery.integration_active = false;
    recovery.phase = "aborted".to_string();
    recovery.capture_state_last_known = Some("stopped".to_string());
    recovery.relay_active_last_known = false;
    recovery.auto_log = None;
    let _ = write_recovery_state(state, &recovery);
}

fn command_error(
    operation_id: &str,
    stage: &str,
    code: &str,
    config_changed: bool,
    capture_running: bool,
    state: &GatewayStateStore,
) -> TrafficCommandError {
    TrafficCommandError {
        operation: "traffic_analysis".to_string(),
        operation_id: operation_id.to_string(),
        stage: stage.to_string(),
        code: code.to_string(),
        message: safe_message(code),
        retryable: matches!(
            code,
            "capture_not_ready"
                | "sidecar_incompatible"
                | "capture_start_failed"
                | "capture_relay_active"
                | "capture_pause_failed"
                | "capture_pause_drain_timeout"
                | "capture_pause_status_unavailable"
                | "capture_instance_mismatch"
                | "config_publish_failed"
                | "capture_stop_failed"
                | "autosave_read_failed"
                | "autosave_write_failed"
                | "autosave_finalize_failed"
                | "unsaved_observations_confirmation_required"
        ),
        config_changed,
        capture_running,
        restart_codex_required: config_changed,
        gateway_snapshot: snapshot(state).ok(),
    }
}
fn safe_message(code: &str) -> String {
    match code {
        "sidecar_incompatible" => {
            "Moon Bridge sidecarのAPI契約または機能が対応していません。sidecarを更新して再試行してください".to_string()
        }
        "config_conflict" => "Codex設定が外部変更されたため、自動復元を停止しました".to_string(),
        "config_parse_failed" => "Codex設定を解析できませんでした".to_string(),
        "backup_failed" => "Codex設定のバックアップに失敗しました".to_string(),
        "capture_start_failed" => "Capture Proxyを開始できませんでした".to_string(),
        "recovery_not_active" => "復旧対象の分析状態はありません".to_string(),
        "recovery_restart_already_attempted" => {
            "この復旧事象ではCaptureの再起動を既に試行しています".to_string()
        }
        "recovery_config_conflict" => {
            "Codex設定が変更されているため、Captureを再起動できません".to_string()
        }
        "capture_relay_active" => {
            "Capture Proxyは中継中です。先に中継を終了してください".to_string()
        }
        "capture_pause_failed" => {
            "観測を停止できませんでした。Capture Proxyは中継を継続しています".to_string()
        }
        "capture_pause_drain_timeout" => {
            "観測の停止がタイムアウトしました。Capture Proxyは中継を継続しています".to_string()
        }
        "capture_pause_status_unavailable" => {
            "観測停止後のCapture状態を確認できませんでした".to_string()
        }
        "capture_instance_mismatch" => {
            "想定外のCapture Proxyが応答したため操作を中止しました".to_string()
        }
        "capture_stop_failed" => "Capture Proxyを停止できませんでした".to_string(),
        "autosave_init_failed" => "分析ログの保存先を準備できませんでした".to_string(),
        "autosave_read_failed" => "分析ログへ書き込む観測を取得できませんでした".to_string(),
        "autosave_write_failed" => {
            "分析ログの自動保存に失敗しました。再試行してください".to_string()
        }
        "autosave_finalize_failed" => {
            "分析ログの最終保存に失敗しました。再試行してください".to_string()
        }
        "unsaved_observations_confirmation_required" => {
            "未保存の観測があります。保存を再試行するか、破棄して中継を終了するか選択してください"
                .to_string()
        }
        "log_folder_unavailable" => "分析ログフォルダーを準備できませんでした".to_string(),
        "analysis_still_active" => "先に分析を停止してCodex設定を復元してください".to_string(),
        "export_path_invalid" => "ログ保存先が不正です".to_string(),
        "export_missing" => "直前に保存したログファイルが見つかりません".to_string(),
        "export_not_owned" => "このアプリが直前に保存したログファイルではありません".to_string(),
        "reveal_failed" => "ログ保存先を開けませんでした".to_string(),
        "reveal_unsupported" => "このOSではログ保存先の表示に対応していません".to_string(),
        _ => "Traffic Analysis操作に失敗しました".to_string(),
    }
}
fn emit_progress(app: &tauri::AppHandle, operation_id: &str, stage: &str, message: &str) {
    let _ = app.emit(
        "traffic-analysis-progress",
        TrafficProgress {
            operation_id: operation_id.to_string(),
            operation: "traffic_analysis".to_string(),
            stage: stage.to_string(),
            message: message.to_string(),
        },
    );
}
fn stopped_capture_status() -> CaptureStatus {
    CaptureStatus {
        state: "stopped".to_string(),
        capture_address: "127.0.0.1:38441".to_string(),
        upstream_host: "chatgpt.com".to_string(),
        ..Default::default()
    }
}

fn capture_relay_active(capture: &CaptureStatus) -> bool {
    matches!(
        capture.state.as_str(),
        "capturing" | "passthrough" | "draining"
    )
}

fn restoration_requires_publish(
    current_url: Option<&str>,
    applied_url: Option<&str>,
    previous_url: Option<&str>,
    external_change: bool,
    confirm_conflict: bool,
) -> Result<bool, ()> {
    let already_restored = current_url == previous_url;
    let still_applied = current_url == applied_url;
    if !still_applied && !already_restored && external_change && !confirm_conflict {
        return Err(());
    }
    Ok(!already_restored)
}

fn empty_gateway_snapshot() -> GatewaySnapshot {
    GatewaySnapshot {
        state: crate::GatewayState::Stopped,
        address: "127.0.0.1:38440".to_string(),
        config_path: String::new(),
        pid: None,
        instance_id: None,
        error: None,
    }
}

#[cfg(unix)]
fn replace_file(source: &Path, destination: &Path) -> std::io::Result<()> {
    std::fs::rename(source, destination)
}

#[cfg(unix)]
fn sync_parent_directory(parent: &Path) -> std::io::Result<()> {
    // POSIX directory fsync makes the atomic rename durable across a crash.
    OpenOptions::new().read(true).open(parent)?.sync_all()
}

#[cfg(windows)]
fn sync_parent_directory(_parent: &Path) -> std::io::Result<()> {
    // Windows does not expose a portable directory-handle Sync equivalent.
    // MoveFileExW(MOVEFILE_WRITE_THROUGH) is the durability boundary here.
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn temp_path(name: &str) -> PathBuf {
        std::env::temp_dir().join(format!(
            "moon-bridge-traffic-{name}-{}.toml",
            Uuid::new_v4().simple()
        ))
    }

    #[test]
    fn changes_only_top_level_openai_base_url() {
        let original = "# keep this comment\nmodel = \"gpt-5\"\nunknown_key = \"keep\"\n\n[mcp_servers.tools]\ncommand = \"tool\"\n";
        let path = temp_path("only-base-url");
        fs::write(&path, original).expect("write fixture");
        let loaded = read_config(&path).expect("read fixture");
        assert_eq!(loaded.previous_url, None);
        let previous_url = loaded.previous_url.clone();
        let candidate = render_with_base_url(&loaded.document, CAPTURE_BASE_URL);
        publish_config(&path, &candidate).expect("publish candidate");
        let rendered = fs::read_to_string(&path).expect("read candidate");
        assert!(rendered.contains("# keep this comment"));
        assert!(rendered.contains("unknown_key = \"keep\""));
        assert!(rendered.contains("[mcp_servers.tools]"));
        verify_config(&path, Some(CAPTURE_BASE_URL)).expect("verify applied URL");
        let latest = read_config(&path).expect("read latest");
        let restored = render_with_previous_url(&latest.document, previous_url.as_deref());
        publish_config(&path, &restored).expect("restore candidate");
        verify_config(&path, None).expect("verify removed URL");
        fs::remove_file(path).expect("remove fixture");
    }

    #[test]
    fn existing_openai_base_url_is_restored_exactly() {
        let path = temp_path("restore-existing");
        fs::write(
            &path,
            "openai_base_url = \"https://example.invalid\"\nmodel = \"gpt-5\"\n",
        )
        .expect("write fixture");
        let loaded = read_config(&path).expect("read fixture");
        assert_eq!(
            loaded.previous_url.as_deref(),
            Some("https://example.invalid")
        );
        let candidate = render_with_base_url(&loaded.document, CAPTURE_BASE_URL);
        publish_config(&path, &candidate).expect("publish candidate");
        let latest = read_config(&path).expect("read candidate");
        let restored = render_with_previous_url(&latest.document, loaded.previous_url.as_deref());
        publish_config(&path, &restored).expect("restore candidate");
        verify_config(&path, Some("https://example.invalid")).expect("verify restored URL");
        fs::remove_file(path).expect("remove fixture");
    }

    #[test]
    fn malformed_toml_is_rejected_before_publication() {
        let path = temp_path("malformed");
        let original = b"openai_base_url = [\n";
        fs::write(&path, original).expect("write fixture");
        assert!(read_config(&path).is_err());
        assert_eq!(fs::read(&path).expect("read fixture"), original);
        fs::remove_file(path).expect("remove fixture");
    }

    #[test]
    fn missing_config_is_removed_after_restore() {
        let path = temp_path("missing-restore");
        let original = read_config(&path).expect("read missing fixture");
        let candidate = render_with_base_url(&original.document, CAPTURE_BASE_URL);
        publish_config(&path, &candidate).expect("publish candidate");
        restore_original_config(&path, &original).expect("restore missing fixture");
        assert!(!path.exists());
    }

    #[test]
    fn atomic_publication_failure_preserves_existing_destination() {
        let path = temp_path("publication-failure");
        fs::create_dir_all(&path).expect("create destination directory");
        assert!(publish_config(&path, b"new content").is_err());
        assert!(path.is_dir());
        fs::remove_dir_all(path).expect("remove fixture");
    }

    #[test]
    fn injected_recovery_publication_failure_preserves_previous_state() {
        let root = std::env::temp_dir().join(format!(
            "moon-bridge-recovery-publication-failure-{}",
            Uuid::new_v4().simple()
        ));
        let state = GatewayStateStore::new();
        state
            .set_app_paths(crate::paths::AppPaths::from_local_data_dir(root.clone()))
            .expect("set test app paths");
        let recovery = RecoveryState {
            schema_version: 2,
            integration_active: false,
            phase: "aborted".to_string(),
            operation_id: "fault-op".to_string(),
            config_path: "C:\\fixture\\codex\\config.toml".to_string(),
            previous_openai_base_url_present: false,
            previous_openai_base_url: None,
            applied_openai_base_url: CAPTURE_BASE_URL.to_string(),
            config_hash_before_apply: "before".to_string(),
            config_hash_after_apply: "after".to_string(),
            backup_path: None,
            started_at: "2026-08-03T00:00:00Z".to_string(),
            updated_at: None,
            auto_log: None,
            auto_log_status: None,
            unsaved_observations_may_remain: false,
            unsaved_discard_confirmed: false,
            migration: None,
            capture_state_last_known: Some("stopped".to_string()),
            relay_active_last_known: false,
            reconciliation_status: Some("inactive".to_string()),
            reconciled_at: None,
            reconciliation_detail: None,
            restart_attempted: false,
        };
        write_recovery_state(&state, &recovery).expect("write initial recovery state");
        let path = state
            .app_paths()
            .expect("resolve test app paths")
            .recovery_state_path();
        let before = fs::read(&path).expect("read initial recovery state");

        let mut changed = recovery;
        changed.phase = "reconciliation_conflict".to_string();
        inject_publish_failure_once(&path);
        assert!(write_recovery_state(&state, &changed).is_err());
        assert_eq!(
            fs::read(&path).expect("read preserved recovery state"),
            before
        );
        fs::remove_dir_all(root).expect("remove publication fault fixture");
    }

    #[test]
    fn recovery_v1_is_migrated_without_losing_original_bytes() {
        let root = std::env::temp_dir().join(format!(
            "moon-bridge-recovery-migration-{}",
            Uuid::new_v4().simple()
        ));
        let state = GatewayStateStore::new();
        state
            .set_app_paths(crate::paths::AppPaths::from_local_data_dir(root.clone()))
            .expect("set test app paths");
        let paths = state.app_paths().expect("resolve test app paths");
        fs::create_dir_all(paths.recovery_dir()).expect("create recovery fixture");
        let legacy = RecoveryState {
            schema_version: 1,
            integration_active: false,
            phase: "integration_applied".to_string(),
            operation_id: "legacy-op".to_string(),
            config_path: "C:\\fixture\\codex\\config.toml".to_string(),
            previous_openai_base_url_present: false,
            previous_openai_base_url: None,
            applied_openai_base_url: CAPTURE_BASE_URL.to_string(),
            config_hash_before_apply: "before".to_string(),
            config_hash_after_apply: "after".to_string(),
            backup_path: None,
            started_at: "2026-08-03T00:00:00Z".to_string(),
            updated_at: None,
            auto_log: None,
            auto_log_status: None,
            unsaved_observations_may_remain: false,
            unsaved_discard_confirmed: false,
            migration: None,
            capture_state_last_known: None,
            relay_active_last_known: false,
            reconciliation_status: None,
            reconciled_at: None,
            reconciliation_detail: None,
            restart_attempted: false,
        };
        let original = serde_json::to_vec_pretty(&legacy).expect("serialize legacy state");
        fs::write(paths.recovery_state_path(), &original).expect("write legacy state");

        let migrated = read_recovery_state(&state).expect("migrate recovery state");
        assert_eq!(migrated.schema_version, 2);
        assert_eq!(
            migrated.migration.as_ref().unwrap().source_schema_version,
            1
        );
        assert_ne!(fs::read(paths.recovery_state_path()).unwrap(), original);
        let archive_dir = paths.recovery_dir().join("migrated-v1");
        let archives = fs::read_dir(&archive_dir)
            .expect("read migration archive")
            .filter_map(Result::ok)
            .collect::<Vec<_>>();
        assert_eq!(archives.len(), 1);
        assert_eq!(fs::read(archives[0].path()).unwrap(), original);
        let migrated_again = read_recovery_state(&state).expect("read migrated state again");
        assert_eq!(migrated_again.schema_version, 2);
        let archive_count = fs::read_dir(&archive_dir)
            .expect("read migration archive again")
            .filter_map(Result::ok)
            .count();
        assert_eq!(archive_count, 1);
        fs::remove_dir_all(root).expect("remove migration fixture");
    }

    #[test]
    fn config_backup_retention_does_not_touch_unknown_files() {
        let directory = std::env::temp_dir().join(format!(
            "moon-bridge-backup-retention-{}",
            Uuid::new_v4().simple()
        ));
        fs::create_dir_all(&directory).expect("create backup fixture");
        for day in 1..=7 {
            fs::write(
                directory.join(format!("202608{day:02}T000000000Z-config.toml")),
                b"model = \"gpt-5\"\n",
            )
            .expect("write backup fixture");
        }
        let unknown = directory.join("keep-me.txt");
        fs::write(&unknown, b"keep").expect("write unknown fixture");
        retain_config_backups(&directory, None);
        let backups = fs::read_dir(&directory)
            .expect("read backup fixture")
            .filter_map(Result::ok)
            .filter(|entry| is_config_backup_name(&entry.file_name().to_string_lossy()))
            .count();
        assert_eq!(backups, 5);
        assert!(unknown.is_file());
        fs::remove_dir_all(directory).expect("remove backup fixture");
    }

    #[test]
    fn recovery_state_does_not_contain_config_contents_or_keys() {
        let state = RecoveryState {
            schema_version: 1,
            integration_active: true,
            phase: "integration_applied".to_string(),
            operation_id: "op-1".to_string(),
            config_path: "C:\\fixture\\codex\\config.toml".to_string(),
            previous_openai_base_url_present: false,
            previous_openai_base_url: None,
            applied_openai_base_url: CAPTURE_BASE_URL.to_string(),
            config_hash_before_apply: "before".to_string(),
            config_hash_after_apply: "after".to_string(),
            backup_path: None,
            started_at: Utc::now().to_rfc3339(),
            updated_at: None,
            auto_log: None,
            auto_log_status: None,
            unsaved_observations_may_remain: false,
            unsaved_discard_confirmed: false,
            migration: None,
            capture_state_last_known: None,
            relay_active_last_known: false,
            reconciliation_status: None,
            reconciled_at: None,
            reconciliation_detail: None,
            restart_attempted: false,
        };
        let serialized = serde_json::to_string(&state).expect("serialize state");
        assert!(!serialized.contains("auth.json"));
        assert!(!serialized.contains("api_key"));
        assert!(!serialized.contains("model ="));
    }

    #[test]
    fn reconciliation_classifies_pending_restore_without_republishing() {
        let previous = b"model = \"gpt-5\"\n";
        let applied = render_with_base_url(
            &std::str::from_utf8(previous)
                .expect("UTF-8 config")
                .parse::<DocumentMut>()
                .expect("parse config"),
            CAPTURE_BASE_URL,
        );
        let recovery = RecoveryState {
            schema_version: 2,
            integration_active: true,
            phase: "integration_applied".to_string(),
            operation_id: "op".to_string(),
            config_path: "C:\\fixture\\codex\\config.toml".to_string(),
            previous_openai_base_url_present: false,
            previous_openai_base_url: None,
            applied_openai_base_url: CAPTURE_BASE_URL.to_string(),
            config_hash_before_apply: hash_bytes(previous),
            config_hash_after_apply: hash_bytes(&applied),
            backup_path: None,
            started_at: "2026-08-03T00:00:00Z".to_string(),
            updated_at: None,
            auto_log: None,
            auto_log_status: None,
            unsaved_observations_may_remain: false,
            unsaved_discard_confirmed: false,
            migration: None,
            capture_state_last_known: Some("capturing".to_string()),
            relay_active_last_known: true,
            reconciliation_status: None,
            reconciled_at: None,
            reconciliation_detail: None,
            restart_attempted: false,
        };
        let current = ReadConfig {
            bytes: applied,
            document: DocumentMut::new(),
            previous_url: Some(CAPTURE_BASE_URL.to_string()),
            existed: true,
        };
        let decision = classify_recovery_after_startup(&recovery, &current);
        assert_eq!(decision.status, "pending_restore");
        assert_eq!(decision.phase, "reconciliation_required");
        assert!(decision.integration_active);
    }

    #[test]
    fn crash_phases_are_reconciled_as_pending_restore_without_republication() {
        let previous = b"model = \"gpt-5\"\n";
        let applied = render_with_base_url(
            &std::str::from_utf8(previous)
                .expect("UTF-8 config")
                .parse::<DocumentMut>()
                .expect("parse config"),
            CAPTURE_BASE_URL,
        );
        for (phase, integration_active) in [("prepared", false), ("capture_started", false)] {
            let recovery = RecoveryState {
                schema_version: 2,
                integration_active,
                phase: phase.to_string(),
                operation_id: "crash-op".to_string(),
                config_path: "C:\\fixture\\codex\\config.toml".to_string(),
                previous_openai_base_url_present: false,
                previous_openai_base_url: None,
                applied_openai_base_url: CAPTURE_BASE_URL.to_string(),
                config_hash_before_apply: hash_bytes(previous),
                config_hash_after_apply: hash_bytes(&applied),
                backup_path: None,
                started_at: "2026-08-03T00:00:00Z".to_string(),
                updated_at: None,
                auto_log: None,
                auto_log_status: None,
                unsaved_observations_may_remain: false,
                unsaved_discard_confirmed: false,
                migration: None,
                capture_state_last_known: Some("capturing".to_string()),
                relay_active_last_known: true,
                reconciliation_status: None,
                reconciled_at: None,
                reconciliation_detail: None,
                restart_attempted: false,
            };
            let current = ReadConfig {
                bytes: applied.clone(),
                document: DocumentMut::new(),
                previous_url: Some(CAPTURE_BASE_URL.to_string()),
                existed: true,
            };
            let decision = classify_recovery_after_startup(&recovery, &current);
            assert_eq!(decision.status, "pending_restore", "phase {phase}");
            assert_eq!(decision.phase, "reconciliation_required", "phase {phase}");
            assert!(decision.integration_active, "phase {phase}");
        }
    }

    #[test]
    fn startup_reconciliation_never_rewrites_codex_config() {
        let root =
            std::env::temp_dir().join(format!("moon-bridge-reconcile-{}", Uuid::new_v4().simple()));
        let config_path = root.join("codex").join("config.toml");
        let original = b"model = \"gpt-5\"\nopenai_base_url = \"http://127.0.0.1:38441\"\n";
        fs::create_dir_all(config_path.parent().unwrap()).expect("create config fixture");
        fs::write(&config_path, original).expect("write config fixture");

        let state = GatewayStateStore::new();
        state
            .set_app_paths(crate::paths::AppPaths::from_local_data_dir(root.clone()))
            .expect("set test app paths");
        let recovery = RecoveryState {
            schema_version: 2,
            integration_active: true,
            phase: "integration_applied".to_string(),
            operation_id: "op".to_string(),
            config_path: config_path.display().to_string(),
            previous_openai_base_url_present: false,
            previous_openai_base_url: None,
            applied_openai_base_url: CAPTURE_BASE_URL.to_string(),
            config_hash_before_apply: hash_bytes(b"model = \"gpt-5\"\n"),
            config_hash_after_apply: hash_bytes(original),
            backup_path: None,
            started_at: "2026-08-03T00:00:00Z".to_string(),
            updated_at: None,
            auto_log: None,
            auto_log_status: None,
            unsaved_observations_may_remain: false,
            unsaved_discard_confirmed: false,
            migration: None,
            capture_state_last_known: Some("capturing".to_string()),
            relay_active_last_known: true,
            reconciliation_status: None,
            reconciled_at: None,
            reconciliation_detail: None,
            restart_attempted: false,
        };
        write_recovery_state(&state, &recovery).expect("write recovery fixture");

        reconcile_startup(&state).expect("reconcile startup");
        assert_eq!(fs::read(&config_path).expect("read config"), original);
        let reconciled = read_recovery_state(&state).expect("read reconciled state");
        assert_eq!(
            reconciled.reconciliation_status.as_deref(),
            Some("pending_restore")
        );
        assert!(reconciled.integration_active);
        fs::remove_dir_all(root).expect("remove reconciliation fixture");
    }

    #[test]
    fn reconciliation_classifies_already_restored_and_external_conflict() {
        let previous = b"model = \"gpt-5\"\n";
        let recovery = RecoveryState {
            schema_version: 2,
            integration_active: true,
            phase: "integration_applied".to_string(),
            operation_id: "op".to_string(),
            config_path: "C:\\fixture\\codex\\config.toml".to_string(),
            previous_openai_base_url_present: false,
            previous_openai_base_url: None,
            applied_openai_base_url: CAPTURE_BASE_URL.to_string(),
            config_hash_before_apply: hash_bytes(previous),
            config_hash_after_apply: hash_bytes(b"applied"),
            backup_path: None,
            started_at: "2026-08-03T00:00:00Z".to_string(),
            updated_at: None,
            auto_log: None,
            auto_log_status: None,
            unsaved_observations_may_remain: false,
            unsaved_discard_confirmed: false,
            migration: None,
            capture_state_last_known: None,
            relay_active_last_known: false,
            reconciliation_status: None,
            reconciled_at: None,
            reconciliation_detail: None,
            restart_attempted: false,
        };
        let restored = ReadConfig {
            bytes: previous.to_vec(),
            document: DocumentMut::new(),
            previous_url: None,
            existed: true,
        };
        let decision = classify_recovery_after_startup(&recovery, &restored);
        assert_eq!(decision.status, "already_restored");
        assert!(!decision.integration_active);

        let external = ReadConfig {
            bytes: b"model = \"other\"\n".to_vec(),
            document: DocumentMut::new(),
            previous_url: Some("https://external.invalid".to_string()),
            existed: true,
        };
        let decision = classify_recovery_after_startup(&recovery, &external);
        assert_eq!(decision.status, "config_conflict");
        assert!(decision.integration_active);
    }

    #[test]
    fn export_log_contains_only_safe_fields() {
        let observation = serde_json::json!({
            "sequence": 1,
            "timestamp": "2026-08-03T00:00:00Z",
            "direction": "client_to_upstream",
            "transport": "http",
            "method": "POST",
            "receivedPath": "/responses",
            "payloadKind": "json",
            "rawPayloadSize": 12,
            "decodedObservationSize": 12,
            "decodingStatus": "identity",
            "rawPayloadHmac": "SENTINEL_RAW_HMAC",
            "prompt": "SENTINEL_PROMPT",
            "headerSummary": {"authorizationPresent": true},
            "opaqueFields": [{"opaqueContentHmac": "SENTINEL_OPAQUE"}],
            "payloadShape": {"modelValue": "gpt-5", "topLevelFields": ["model"]}
        });
        let rendered =
            String::from_utf8(render_export_log(&[observation], 0)).expect("UTF-8 export");
        assert!(rendered.contains("model: gpt-5"));
        assert!(!rendered.contains("SENTINEL_RAW_HMAC"));
        assert!(!rendered.contains("SENTINEL_PROMPT"));
        assert!(!rendered.contains("SENTINEL_OPAQUE"));
    }

    #[test]
    fn export_path_and_reveal_ownership_are_restricted() {
        let base = std::env::temp_dir().join("moon-bridge-export-test");
        let without_extension = base.with_extension("");
        assert_eq!(
            normalize_export_path(without_extension)
                .expect("add log extension")
                .extension()
                .and_then(|value| value.to_str()),
            Some("log")
        );
        assert!(normalize_export_path(base.with_extension("txt")).is_err());
        assert!(normalize_export_path(PathBuf::from("relative.log")).is_err());
        let recorded = base.with_extension("log");
        assert!(is_recorded_export(&recorded, Some(&recorded)));
        assert!(!is_recorded_export(
            &recorded,
            Some(&base.with_extension("other.log"))
        ));
        assert!(!is_recorded_export(&recorded, None));
    }

    #[test]
    fn relay_active_is_derived_from_capture_state_not_config_integration() {
        for state in ["capturing", "passthrough", "draining"] {
            let capture = CaptureStatus {
                state: state.to_string(),
                ..CaptureStatus::default()
            };
            assert!(capture_relay_active(&capture), "state {state}");
        }
        for state in ["stopped", "failed"] {
            let capture = CaptureStatus {
                state: state.to_string(),
                ..CaptureStatus::default()
            };
            assert!(!capture_relay_active(&capture), "state {state}");
        }
    }

    #[test]
    fn restoration_accepts_already_restored_configuration() {
        assert!(
            !restoration_requires_publish(None, Some(CAPTURE_BASE_URL), None, true, false,)
                .expect("already restored config should not conflict")
        );
        assert!(restoration_requires_publish(
            Some(CAPTURE_BASE_URL),
            Some(CAPTURE_BASE_URL),
            None,
            false,
            false,
        )
        .expect("applied config should be restored"));
        assert!(restoration_requires_publish(
            Some("https://external.example"),
            Some(CAPTURE_BASE_URL),
            None,
            true,
            false,
        )
        .is_err());
        assert!(restoration_requires_publish(
            Some("https://external.example"),
            Some(CAPTURE_BASE_URL),
            None,
            true,
            true,
        )
        .expect("explicit conflict confirmation should permit restore"));
    }

    #[test]
    fn auto_log_names_are_strict_and_collisions_get_suffixes() {
        let directory =
            std::env::temp_dir().join(format!("moon-bridge-auto-log-{}", Uuid::new_v4().simple()));
        fs::create_dir_all(&directory).expect("create log fixture");
        let first = directory.join(format!(
            "traffic-analysis-{}.log",
            Local::now().format("%Y%m%d-%H%M%S")
        ));
        fs::write(&first, b"existing").expect("write collision fixture");
        let next = new_traffic_log_path(&directory).expect("allocate suffixed path");
        assert!(next
            .file_name()
            .unwrap()
            .to_string_lossy()
            .ends_with("-001.log"));
        assert!(is_traffic_log_name("traffic-analysis-20260803-201521.log"));
        assert!(is_traffic_log_name(
            "traffic-analysis-20260803-201521-001.log"
        ));
        assert!(!is_traffic_log_name("traffic-analysis-20260803-201521.txt"));
        assert!(!is_traffic_log_name(
            "traffic-analysis-20260803-201521-extra.log"
        ));
        fs::remove_dir_all(directory).expect("remove log fixture");
    }

    #[test]
    fn auto_save_appends_each_sequence_once_and_finalizes_idempotently() {
        let path = std::env::temp_dir().join(format!(
            "moon-bridge-auto-log-{}.log",
            Uuid::new_v4().simple()
        ));
        fs::write(&path, render_log_header("session", "2026-08-03T00:00:00Z"))
            .expect("write header");
        let state = GatewayStateStore::new();
        *state.traffic_auto_save.lock().expect("lock autosave") = Some(TrafficAutoSaveSession {
            session_id: "session".to_string(),
            destination: path.clone(),
            started_at: "2026-08-03T00:00:00Z".to_string(),
            last_persisted_sequence: 0,
            observations_written: 0,
            sequence_gaps: 0,
            last_synced_at: None,
            finalized: false,
            last_safe_error: None,
            dropped_observations: 0,
            last_checkpoint_sequence: 0,
            last_checkpoint_at: None,
        });
        let observations = vec![
            serde_json::json!({"sequence": 1, "timestamp": "2026-08-03T00:00:01Z", "direction": "client_to_upstream", "transport": "http"}),
            serde_json::json!({"sequence": 2, "timestamp": "2026-08-03T00:00:02Z", "direction": "upstream_to_client", "transport": "sse"}),
        ];
        sync_autosave(&state, &observations, 0).expect("append observations");
        sync_autosave(&state, &observations, 0).expect("ignore duplicate observations");
        finalize_autosave(&state, &observations, 0).expect("write footer");
        finalize_autosave(&state, &observations, 0).expect("idempotent finalization");
        let content = fs::read_to_string(&path).expect("read autosave");
        assert_eq!(content.matches("#1 ").count(), 1);
        assert_eq!(content.matches("#2 ").count(), 1);
        assert_eq!(content.matches("Status: completed").count(), 1);
        fs::remove_file(path).expect("remove log fixture");
    }

    #[test]
    fn journal_records_only_safe_fields_and_reuses_snapshot_session_id() {
        let path = temp_path("journal-safe");
        let mut journal = CommandJournal::disabled();
        journal.configure(path.clone());
        // One command call writes invoke/acquired/end sharing one invocation id:
        // start=1, stop=2, finish_relay=3, matching the Tauri wrappers.
        journal.journal(1, None, "traffic_analysis_start", "invoke", None);
        journal.journal(1, None, "traffic_analysis_start", "acquired", None);
        journal.journal(
            1,
            Some("session-abc"),
            "traffic_analysis_start",
            "end",
            Some("ok"),
        );
        journal.journal(
            2,
            Some("session-abc"),
            "traffic_analysis_stop",
            "invoke",
            None,
        );
        journal.journal(
            2,
            Some("session-abc"),
            "traffic_analysis_stop",
            "acquired",
            None,
        );
        journal.journal(
            2,
            Some("session-abc"),
            "traffic_analysis_stop",
            "end",
            Some("ok"),
        );
        journal.journal(
            3,
            Some("session-abc"),
            "traffic_analysis_finish_relay",
            "invoke",
            None,
        );
        journal.journal(
            3,
            Some("session-abc"),
            "traffic_analysis_finish_relay",
            "acquired",
            None,
        );
        journal.journal(
            3,
            Some("session-abc"),
            "traffic_analysis_finish_relay",
            "end",
            Some("ok"),
        );

        let content = fs::read_to_string(&path).expect("read journal");
        let lines: Vec<serde_json::Value> = content
            .lines()
            .map(|line| serde_json::from_str(line).expect("valid jsonl line"))
            .collect();
        assert_eq!(lines.len(), 9, "one line per event");
        let allowed = [
            "schema",
            "t",
            "session_id",
            "invocation_id",
            "kind",
            "command",
            "event",
            "result",
        ];
        for record in &lines {
            let object = record.as_object().expect("record object");
            for key in object.keys() {
                assert!(allowed.contains(&key.as_str()), "unexpected field {key}");
            }
            assert_eq!(object["kind"], "command");
            assert_eq!(object["schema"], 1);
        }
        // start invoke/acquired carry no session id; end carries the created id.
        assert!(lines[0]["session_id"].is_null());
        assert!(lines[1]["session_id"].is_null());
        assert_eq!(lines[2]["session_id"], "session-abc");
        // stop and finish_relay events share the snapshot session id.
        for record in &lines[3..9] {
            assert_eq!(record["session_id"], "session-abc");
        }
        // Each call's three events share one invocation id; groups are in order.
        let ids: Vec<u64> = lines
            .iter()
            .map(|record| record["invocation_id"].as_u64().unwrap())
            .collect();
        assert_eq!(ids, vec![1, 1, 1, 2, 2, 2, 3, 3, 3]);
        for (index, command) in [
            "traffic_analysis_start",
            "traffic_analysis_stop",
            "traffic_analysis_finish_relay",
        ]
        .iter()
        .enumerate()
        {
            let events: Vec<&str> = lines[index * 3..index * 3 + 3]
                .iter()
                .map(|record| record["event"].as_str().unwrap())
                .collect();
            assert_eq!(
                events,
                vec!["invoke", "acquired", "end"],
                "{command} invoke/acquired/end share invocation id {}",
                index + 1
            );
        }
        fs::remove_file(path).expect("remove journal fixture");
    }

    #[test]
    fn journal_keeps_known_result_codes_on_end() {
        let path = temp_path("journal-codes");
        let mut journal = CommandJournal::disabled();
        journal.configure(path.clone());
        journal.journal(
            1,
            Some("session-abc"),
            "traffic_analysis_stop",
            "end",
            Some("ok"),
        );
        journal.journal(
            2,
            Some("session-abc"),
            "traffic_analysis_finish_relay",
            "end",
            Some("capture_stop_failed"),
        );
        let content = fs::read_to_string(&path).expect("read journal");
        assert!(content.contains("\"result\":\"ok\""));
        assert!(content.contains("\"result\":\"capture_stop_failed\""));
        assert_eq!(content.lines().count(), 2);
        fs::remove_file(path).expect("remove journal fixture");
    }

    #[test]
    fn journal_rotates_oversized_file_to_dot_one() {
        let path = temp_path("journal-rotate");
        let file = fs::File::create(&path).expect("create oversized");
        file.set_len(JOURNAL_MAX_BYTES + 1)
            .expect("oversize journal");
        drop(file);
        let stem = path
            .file_stem()
            .and_then(|name| name.to_str())
            .expect("stem");
        let rotated = path.with_file_name(format!("{stem}.1.jsonl"));

        let mut journal = CommandJournal::disabled();
        journal.configure(path.clone());
        assert!(rotated.exists(), "oversized file was rotated");
        let rotated_len = fs::metadata(&rotated).map(|meta| meta.len()).unwrap_or(0);
        assert!(
            rotated_len > JOURNAL_MAX_BYTES,
            "rotated file keeps the content"
        );

        journal.journal(
            1,
            Some("session-abc"),
            "traffic_analysis_stop",
            "end",
            Some("ok"),
        );
        let fresh = fs::read_to_string(&path).expect("read fresh journal");
        let lines: Vec<&str> = fresh.lines().collect();
        assert_eq!(lines.len(), 1, "fresh journal starts over");
        let parsed: serde_json::Value = serde_json::from_str(lines[0]).expect("valid line");
        assert_eq!(parsed["command"], "traffic_analysis_stop");
        fs::remove_file(path).expect("remove fresh journal");
        fs::remove_file(rotated).expect("remove rotated journal");
    }

    #[test]
    fn disabled_journal_writes_nothing() {
        let mut journal = CommandJournal::disabled();
        journal.journal(
            1,
            Some("session-abc"),
            "traffic_analysis_stop",
            "invoke",
            None,
        );
        assert!(journal.path.as_os_str().is_empty());
    }

    #[test]
    fn normalize_result_maps_known_codes_and_hides_error_text() {
        let error = |code: &str| TrafficCommandError {
            operation: "traffic_analysis_stop".to_string(),
            operation_id: "id".to_string(),
            stage: "complete".to_string(),
            code: code.to_string(),
            message: "SENSITIVE ERROR MESSAGE".to_string(),
            retryable: false,
            config_changed: false,
            capture_running: false,
            restart_codex_required: false,
            gateway_snapshot: None,
        };
        for code in [
            "capture_pause_failed",
            "capture_stop_failed",
            "config_restore_failed",
        ] {
            assert_eq!(normalize_result(&Err(error(code))), code);
        }
        assert_eq!(
            normalize_result(&Err(error("unexpected_boom"))),
            "unknown_error"
        );
        assert!(!normalize_result(&Err(error("unexpected_boom"))).contains("SENSITIVE"));

        let result = TrafficAnalysisResult {
            operation_id: "op".to_string(),
            status: TrafficAnalysisStatus {
                capture: CaptureStatus::default(),
                config_path: String::new(),
                config_exists: false,
                integration_active: false,
                relay_active: false,
                recovery_available: false,
                applied_openai_base_url: None,
                recovery_phase: None,
                reconciliation_status: None,
                reconciled_at: None,
                auto_save: TrafficAutoSaveStatus::default(),
            },
            config_path: String::new(),
            restart_codex_required: false,
            gateway_snapshot: empty_gateway_snapshot(),
        };
        assert_eq!(normalize_result(&Ok(result)), "ok");
    }
}

#[cfg(windows)]
fn replace_file(source: &Path, destination: &Path) -> std::io::Result<()> {
    use std::os::windows::ffi::OsStrExt;
    use windows_sys::Win32::Storage::FileSystem::{
        MoveFileExW, MOVEFILE_REPLACE_EXISTING, MOVEFILE_WRITE_THROUGH,
    };
    let source: Vec<u16> = source.as_os_str().encode_wide().chain(Some(0)).collect();
    let destination: Vec<u16> = destination
        .as_os_str()
        .encode_wide()
        .chain(Some(0))
        .collect();
    let result = unsafe {
        MoveFileExW(
            source.as_ptr(),
            destination.as_ptr(),
            MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH,
        )
    };
    if result == 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(())
    }
}

#[cfg(not(any(unix, windows)))]
fn replace_file(source: &Path, destination: &Path) -> std::io::Result<()> {
    std::fs::rename(source, destination)
}

#[cfg(not(any(unix, windows)))]
fn sync_parent_directory(_parent: &Path) -> std::io::Result<()> {
    Ok(())
}
