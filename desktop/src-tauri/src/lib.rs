#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod codex;
mod deepseek;
mod job;
mod moonbridge_api;
mod paths;
mod traffic_analysis;

use chrono::{DateTime, Utc};
use reqwest::Client;
use serde::{Deserialize, Serialize};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use std::time::Duration;
use tauri::Emitter;
use tauri::State;
use tauri::{Manager, WindowEvent};
use tauri_plugin_shell::{process::CommandChild, process::CommandEvent, ShellExt};
use tokio::net::TcpStream;
use tokio::time::sleep;
use uuid::Uuid;

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum GatewayState {
    Stopped,
    Starting,
    Running,
    Stopping,
    Error,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct GatewaySnapshot {
    pub state: GatewayState,
    pub address: String,
    pub config_path: String,
    pub pid: Option<u32>,
    pub instance_id: Option<String>,
    pub error: Option<String>,
}

#[derive(Clone, Serialize)]
pub struct GatewayLog {
    pub stream: String,
    pub line: String,
    pub timestamp: DateTime<Utc>,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct OperationProgress {
    operation_id: String,
    operation: String,
    stage: String,
    message: String,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct DeepSeekSaveResult {
    operation_id: String,
    status: deepseek::DeepSeekStatus,
    gateway_snapshot: GatewaySnapshot,
    gateway_left_running: bool,
    warning: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct CommandError {
    operation: String,
    stage: String,
    code: String,
    message: String,
    field: Option<String>,
    retryable: bool,
    mutation_started: bool,
    gateway_left_running: bool,
    gateway_snapshot: Option<GatewaySnapshot>,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct DeepSeekConnectionTestResult {
    operation_id: String,
    result: deepseek::ConnectionTestResult,
    gateway_snapshot: GatewaySnapshot,
    gateway_left_running: bool,
    warning: Option<String>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct OperationInput {
    operation_id: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct TrafficAnalysisInput {
    operation_id: String,
    #[serde(default)]
    discard_unsaved: bool,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct TrafficObservationInput {
    after: u64,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct TrafficExportInput {
    operation_id: String,
    destination: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct TrafficRevealExportInput {
    operation_id: String,
    destination: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct DeepSeekSaveInput {
    operation_id: String,
    api_key: Option<String>,
    model: String,
    reasoning_effort: String,
}

struct GatewayStateStore {
    client: Client,
    snapshot: Mutex<GatewaySnapshot>,
    child: Mutex<Option<CommandChild>>,
    job: Mutex<Option<job::JobHandle>>,
    token: Mutex<Option<String>>,
    operation: tokio::sync::Mutex<()>,
    codex_launched_this_run: AtomicBool,
    exit_prompt_open: AtomicBool,
    exit_approved: AtomicBool,
    traffic_analysis_active: AtomicBool,
    last_traffic_export: Mutex<Option<PathBuf>>,
    traffic_auto_save: Mutex<Option<traffic_analysis::TrafficAutoSaveSession>>,
    command_journal: Mutex<traffic_analysis::CommandJournal>,
    app_paths: Mutex<Option<paths::AppPaths>>,
}

impl GatewayStateStore {
    fn new() -> Self {
        let config_path = paths::config_path()
            .map(|p| p.display().to_string())
            .unwrap_or_default();
        Self {
            client: Client::new(),
            snapshot: Mutex::new(GatewaySnapshot {
                state: GatewayState::Stopped,
                address: "127.0.0.1:38440".to_string(),
                config_path,
                pid: None,
                instance_id: None,
                error: None,
            }),
            child: Mutex::new(None),
            job: Mutex::new(None),
            token: Mutex::new(None),
            operation: tokio::sync::Mutex::new(()),
            codex_launched_this_run: AtomicBool::new(false),
            exit_prompt_open: AtomicBool::new(false),
            exit_approved: AtomicBool::new(false),
            traffic_analysis_active: AtomicBool::new(false),
            last_traffic_export: Mutex::new(None),
            traffic_auto_save: Mutex::new(None),
            command_journal: Mutex::new(traffic_analysis::CommandJournal::disabled()),
            app_paths: Mutex::new(None),
        }
    }

    /// Snapshots the current auto-save session id without holding the
    /// auto-save lock across the journal or operation locks.
    fn session_id_snapshot(&self) -> Option<String> {
        self.traffic_auto_save
            .lock()
            .ok()
            .and_then(|guard| guard.as_ref().map(|session| session.session_id.clone()))
    }

    fn remember_traffic_export(&self, path: PathBuf) -> Result<(), String> {
        *self
            .last_traffic_export
            .lock()
            .map_err(|error| error.to_string())? = Some(path);
        Ok(())
    }

    fn last_traffic_export(&self) -> Result<Option<PathBuf>, String> {
        Ok(self
            .last_traffic_export
            .lock()
            .map_err(|error| error.to_string())?
            .clone())
    }

    fn set_app_paths(&self, paths: paths::AppPaths) -> Result<(), String> {
        *self.app_paths.lock().map_err(|error| error.to_string())? = Some(paths);
        Ok(())
    }

    fn app_paths(&self) -> Result<paths::AppPaths, String> {
        if let Some(paths) = self
            .app_paths
            .lock()
            .map_err(|error| error.to_string())?
            .clone()
        {
            return Ok(paths);
        }
        paths::AppPaths::from_environment()
    }
}

#[derive(Deserialize)]
struct SidecarStatus {
    status: String,
    #[serde(default)]
    instance_id: Option<String>,
    #[serde(default)]
    desktop_mode: Option<bool>,
    #[serde(default)]
    api_version: Option<u32>,
    #[serde(default)]
    capabilities: Vec<String>,
}

#[tauri::command]
async fn gateway_status(state: State<'_, GatewayStateStore>) -> Result<GatewaySnapshot, String> {
    snapshot(&state)
}

#[tauri::command]
async fn gateway_start(
    app: tauri::AppHandle,
    state: State<'_, GatewayStateStore>,
) -> Result<GatewaySnapshot, String> {
    let _guard = state.operation.lock().await;
    start_gateway_inner(&app, &state).await
}

async fn start_gateway_inner(
    app: &tauri::AppHandle,
    state: &GatewayStateStore,
) -> Result<GatewaySnapshot, String> {
    let current = snapshot(state)?;
    if matches!(
        current.state,
        GatewayState::Running | GatewayState::Starting
    ) {
        return snapshot(&state);
    }
    if matches!(current.state, GatewayState::Error) {
        // A health/identity failure leaves the process for explicit user
        // inspection. A subsequent user-initiated start must first dispose
        // of that known failed child so it cannot race the new sidecar.
        if let Some(child) = state.child.lock().map_err(|e| e.to_string())?.take() {
            let _ = child.kill();
        }
        *state.job.lock().map_err(|e| e.to_string())? = None;
        *state.token.lock().map_err(|e| e.to_string())? = None;
    }
    let config_path = paths::config_path()?;
    let data_dir = paths::data_dir()?;
    std::fs::create_dir_all(&data_dir).map_err(|e| format!("create data directory: {e}"))?;
    let address = "127.0.0.1:38440".to_string();
    let token = Uuid::new_v4().simple().to_string();
    let instance_id = Uuid::new_v4().simple().to_string();
    set_snapshot(
        &state,
        GatewayState::Starting,
        address.clone(),
        None,
        Some(instance_id.clone()),
        None,
    )?;

    let init = app
        .shell()
        .sidecar("moonbridge")
        .map_err(|e| format!("sidecar is not configured: {e}"))?
        .args([
            "-config",
            config_path.to_string_lossy().as_ref(),
            "-init-config",
        ])
        .output()
        .await
        .map_err(|e| format!("initialize config: {e}"))?;
    if !init.status.success() {
        let error = String::from_utf8_lossy(&init.stderr).trim().to_string();
        return fail(
            &state,
            error_or(&error, "Moon Bridge config initialization failed"),
        );
    }

    let (mut events, child) = app
        .shell()
        .sidecar("moonbridge")
        .map_err(|e| format!("sidecar is not configured: {e}"))?
        .args([
            "-config",
            config_path.to_string_lossy().as_ref(),
            "-desktop-mode",
            "-desktop-instance-id",
            &instance_id,
        ])
        .env("MOONBRIDGE_DESKTOP_TOKEN", &token)
        .spawn()
        .map_err(|e| format!("start Moon Bridge: {e}"))?;
    let pid = child.pid();
    let job_handle = match job::attach(pid) {
        Ok(handle) => handle,
        Err(error) => {
            let _ = child.kill();
            return fail(&state, format!("attach sidecar to job object: {error}"));
        }
    };
    {
        *state.child.lock().map_err(|e| e.to_string())? = Some(child);
        *state.job.lock().map_err(|e| e.to_string())? = Some(job_handle);
        *state.token.lock().map_err(|e| e.to_string())? = Some(token.clone());
        let mut snapshot = state.snapshot.lock().map_err(|e| e.to_string())?;
        snapshot.pid = Some(pid);
    }
    emit_log(
        &app,
        "system",
        format!("Moon Bridge sidecar started (pid={pid})"),
    );
    let log_app = (*app).clone();
    tauri::async_runtime::spawn(async move {
        while let Some(event) = events.recv().await {
            match event {
                CommandEvent::Stdout(line) => emit_log(
                    &log_app,
                    "stdout",
                    String::from_utf8_lossy(&line).trim_end().to_string(),
                ),
                CommandEvent::Stderr(line) => emit_log(
                    &log_app,
                    "stderr",
                    String::from_utf8_lossy(&line).trim_end().to_string(),
                ),
                CommandEvent::Terminated(payload) => {
                    let message = format!("Moon Bridge terminated: {payload:?}");
                    emit_log(&log_app, "system", message.clone());
                    let state = log_app.state::<GatewayStateStore>();
                    if let Ok(current) = snapshot(&state) {
                        if !matches!(
                            current.state,
                            GatewayState::Stopped | GatewayState::Stopping
                        ) {
                            let _ = set_snapshot(
                                &state,
                                GatewayState::Error,
                                current.address,
                                None,
                                None,
                                Some(message),
                            );
                        }
                    }
                }
                CommandEvent::Error(error) => emit_log(&log_app, "stderr", error),
                _ => {}
            }
        }
    });

    let client = Client::new();
    let status_url = format!("http://{address}/api/v1/system/status");
    for _ in 0..120 {
        if TcpStream::connect(&address).await.is_ok() {
            if let Ok(response) = client.get(&status_url).bearer_auth(&token).send().await {
                if response.status().is_success()
                    && response
                        .json::<SidecarStatus>()
                        .await
                        .map(|s| {
                            s.status == "ok"
                                && s.desktop_mode == Some(true)
                                && s.api_version == Some(2)
                                && required_sidecar_capabilities(&s.capabilities)
                                && s.instance_id.as_deref() == Some(instance_id.as_str())
                        })
                        .unwrap_or(false)
                {
                    set_snapshot(
                        &state,
                        GatewayState::Running,
                        address,
                        Some(pid),
                        Some(instance_id),
                        None,
                    )?;
                    return snapshot(&state);
                }
            }
        }
        sleep(Duration::from_millis(250)).await;
    }
    let _ = state
        .child
        .lock()
        .ok()
        .and_then(|mut child| child.take().map(|c| c.kill()));
    fail(
        &state,
        "Moon Bridge did not become ready within 30 seconds".to_string(),
    )
}

fn spawn_gateway_monitor(app: tauri::AppHandle) {
    tauri::async_runtime::spawn(async move {
        let client = Client::new();
        loop {
            sleep(Duration::from_secs(2)).await;
            let state = app.state::<GatewayStateStore>();
            let Ok(current) = snapshot(&state) else {
                continue;
            };
            if !matches!(current.state, GatewayState::Running) {
                continue;
            }
            let Some(token) = state.token.lock().ok().and_then(|value| value.clone()) else {
                continue;
            };
            let expected_instance = current.instance_id.clone();
            let status_url = format!("http://{}/api/v1/system/status", current.address);
            let healthy = match client.get(status_url).bearer_auth(token).send().await {
                Ok(response) if response.status().is_success() => response
                    .json::<SidecarStatus>()
                    .await
                    .ok()
                    .is_some_and(|status| {
                        status.status == "ok"
                            && status.desktop_mode == Some(true)
                            && status.api_version == Some(2)
                            && required_sidecar_capabilities(&status.capabilities)
                            && status.instance_id == expected_instance
                    }),
                _ => false,
            };
            if !healthy {
                let message = "Moon Bridge sidecar health or instance identity check failed";
                emit_log(&app, "system", message.to_string());
                let _ = set_snapshot(
                    &state,
                    GatewayState::Error,
                    current.address,
                    current.pid,
                    current.instance_id,
                    Some(message.to_string()),
                );
            }
        }
    });
}

fn required_sidecar_capabilities(capabilities: &[String]) -> bool {
    [
        "traffic-analysis",
        "traffic-analysis-pause",
        "traffic-analysis-passthrough",
        "traffic-analysis-final-stop",
    ]
    .iter()
    .all(|required| capabilities.iter().any(|value| value == required))
}

pub(crate) async fn validate_running_sidecar(state: &GatewayStateStore) -> Result<(), String> {
    let (address, token) = management_connection(state)?;
    let status: SidecarStatus =
        moonbridge_api::ApiClient::new(state.client.clone(), address, token)
            .get("/system/status")
            .await
            .map_err(|error| error.message)?;
    if status.status != "ok"
        || status.desktop_mode != Some(true)
        || status.api_version != Some(2)
        || !required_sidecar_capabilities(&status.capabilities)
        || status.instance_id != snapshot(state)?.instance_id
    {
        return Err("sidecar contract is incompatible".to_string());
    }
    Ok(())
}

#[tauri::command]
async fn gateway_stop(state: State<'_, GatewayStateStore>) -> Result<GatewaySnapshot, String> {
    let _guard = state.operation.lock().await;
    if state.traffic_analysis_active.load(Ordering::Acquire) {
        return Err("traffic_analysis_relay_active".to_string());
    }
    stop_gateway_inner(&state).await
}

async fn stop_gateway_inner(state: &GatewayStateStore) -> Result<GatewaySnapshot, String> {
    let current = snapshot(&state)?;
    if current.pid.is_none() {
        return Ok(current);
    }
    set_snapshot(
        &state,
        GatewayState::Stopping,
        current.address.clone(),
        current.pid,
        current.instance_id.clone(),
        None,
    )?;
    let token = state.token.lock().map_err(|e| e.to_string())?.clone();
    if let Some(token) = token {
        let url = format!("http://{}/api/v1/system/shutdown", current.address);
        let _ = Client::new().post(url).bearer_auth(token).send().await;
    }
    sleep(Duration::from_millis(250)).await;
    if let Some(child) = state.child.lock().map_err(|e| e.to_string())?.take() {
        let _ = child.kill();
    }
    *state.job.lock().map_err(|e| e.to_string())? = None;
    *state.token.lock().map_err(|e| e.to_string())? = None;
    set_snapshot(
        &state,
        GatewayState::Stopped,
        current.address,
        None,
        None,
        None,
    )?;
    snapshot(&state)
}

#[tauri::command]
fn open_config_folder() -> Result<(), String> {
    let dir = paths::data_dir()?;
    std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    #[cfg(windows)]
    std::process::Command::new("explorer")
        .arg(&dir)
        .spawn()
        .map_err(|e| e.to_string())?;
    Ok(())
}

#[tauri::command]
async fn deepseek_status(
    state: State<'_, GatewayStateStore>,
) -> Result<deepseek::DeepSeekStatus, String> {
    let (address, token) = management_connection(&state)?;
    deepseek::status(&moonbridge_api::ApiClient::new(
        state.client.clone(),
        address,
        token,
    ))
    .await
    .map_err(format_api_error)
}

#[tauri::command]
async fn codex_status() -> Result<codex::CodexStatus, String> {
    codex::status().await
}

#[tauri::command]
async fn codex_launch(
    app: tauri::AppHandle,
    state: State<'_, GatewayStateStore>,
    input: codex::CodexLaunchInput,
) -> Result<codex::CodexLaunchResult, codex::CodexCommandError> {
    if input.operation_id.trim().is_empty() {
        return Err(codex_error(
            &input,
            "validating_input",
            "invalid_operation_id",
            "Codex launch operation ID is required",
            false,
            &state,
        ));
    }

    let project = match codex::validate_project_directory(&input.project_directory) {
        Ok(project) => project,
        Err(error) => {
            return Err(codex_error(
                &input,
                "validating_project",
                error_code(&error, "invalid_project"),
                error_message(&error),
                false,
                &state,
            ))
        }
    };
    emit_codex_progress(
        &app,
        &input.operation_id,
        "validating_project",
        "プロジェクトディレクトリを検証しました",
    );

    let installation = match codex::discover_codex().await {
        Ok(installation) => installation,
        Err(error) => {
            return Err(codex_error(
                &input,
                "detecting_codex",
                error_code(&error, "codex_not_found"),
                error_message(&error),
                false,
                &state,
            ))
        }
    };
    emit_codex_progress(
        &app,
        &input.operation_id,
        "detecting_codex",
        "Codex CLIを確認しました",
    );

    let _guard = state.operation.lock().await;
    let before = snapshot(&state).map_err(|error| {
        codex_error(
            &input,
            "checking_route",
            "state_unavailable",
            error,
            false,
            &state,
        )
    })?;
    let started_by_operation = !matches!(before.state, GatewayState::Running);
    if started_by_operation {
        emit_codex_progress(
            &app,
            &input.operation_id,
            "starting_gateway",
            "Gatewayを起動しています",
        );
        if let Err(error) = start_gateway_inner(&app, &state).await {
            return Err(codex_error(
                &input,
                "starting_gateway",
                "gateway_start_failed",
                error,
                true,
                &state,
            ));
        }
    }

    let running = snapshot(&state).map_err(|error| {
        codex_error(
            &input,
            "checking_route",
            "state_unavailable",
            error,
            started_by_operation,
            &state,
        )
    })?;
    if !matches!(running.state, GatewayState::Running) {
        return Err(codex_failure(
            &input,
            "checking_route",
            "gateway_unavailable",
            "Gatewayが起動完了していません",
            started_by_operation,
            &state,
        )
        .await);
    }

    emit_codex_progress(
        &app,
        &input.operation_id,
        "checking_route",
        "保存済みDeepSeekルートを確認しています",
    );
    let (address, token) = match management_connection(&state) {
        Ok(connection) => connection,
        Err(error) => {
            return Err(codex_failure(
                &input,
                "checking_route",
                "gateway_unavailable",
                error,
                started_by_operation,
                &state,
            )
            .await)
        }
    };
    let api = moonbridge_api::ApiClient::new(state.client.clone(), address, token);
    let deepseek_status = match deepseek::status(&api).await {
        Ok(status) => status,
        Err(error) => {
            return Err(codex_failure(
                &input,
                "checking_route",
                "route_not_configured",
                format_api_error(error),
                started_by_operation,
                &state,
            )
            .await)
        }
    };
    let selected_model_is_supported = deepseek_status
        .selected_model
        .as_deref()
        .is_some_and(|model| model == deepseek::PRO_MODEL || model == deepseek::FLASH_MODEL);
    if !deepseek_status.provider_exists
        || !deepseek_status.api_key_set
        || !deepseek_status.configured
        || !deepseek_status.active
        || deepseek_status.route_alias != deepseek::ROUTE_ID
        || !selected_model_is_supported
    {
        return Err(codex_failure(
            &input,
            "checking_route",
            "route_not_configured",
            "保存済みのDeepSeekルートが利用可能な状態ではありません",
            started_by_operation,
            &state,
        )
        .await);
    }

    let codex_home = match paths::codex_home() {
        Ok(path) => path,
        Err(error) => {
            return Err(codex_failure(
                &input,
                "generating_config",
                "config_generation_failed",
                error,
                started_by_operation,
                &state,
            )
            .await)
        }
    };
    let config_path = match paths::codex_config_path() {
        Ok(path) => path,
        Err(error) => {
            return Err(codex_failure(
                &input,
                "generating_config",
                "config_generation_failed",
                error,
                started_by_operation,
                &state,
            )
            .await)
        }
    };
    let moonbridge_config = match paths::config_path() {
        Ok(path) => path,
        Err(error) => {
            return Err(codex_failure(
                &input,
                "generating_config",
                "config_generation_failed",
                error,
                started_by_operation,
                &state,
            )
            .await)
        }
    };
    emit_codex_progress(
        &app,
        &input.operation_id,
        "generating_config",
        "専用CODEX_HOME用の設定を生成しています",
    );
    let generated = match codex::generate_config(&app, &moonbridge_config, &codex_home).await {
        Ok(config) => config,
        Err(error) => {
            return Err(codex_failure(
                &input,
                "generating_config",
                "config_generation_failed",
                error,
                started_by_operation,
                &state,
            )
            .await)
        }
    };
    emit_codex_progress(
        &app,
        &input.operation_id,
        "publishing_config",
        "専用CODEX_HOMEへ設定を公開しています",
    );
    if let Err(error) = codex::publish_config(&codex_home, &generated) {
        return Err(codex_failure(
            &input,
            "publishing_config",
            "config_publish_failed",
            error,
            started_by_operation,
            &state,
        )
        .await);
    }

    emit_codex_progress(
        &app,
        &input.operation_id,
        "launching_terminal",
        "CodexをPowerShellターミナルで起動しています",
    );
    let terminal_pid =
        match codex::spawn_visible_terminal(&installation.executable_path, &project, &codex_home) {
            Ok(pid) => pid,
            Err(error) => {
                return Err(codex_failure(
                    &input,
                    "launching_terminal",
                    "terminal_launch_failed",
                    error,
                    started_by_operation,
                    &state,
                )
                .await)
            }
        };
    state.codex_launched_this_run.store(true, Ordering::Release);
    let gateway_snapshot = snapshot(&state).map_err(|error| {
        codex_error(
            &input,
            "complete",
            "state_unavailable",
            error,
            started_by_operation,
            &state,
        )
    })?;
    emit_codex_progress(&app, &input.operation_id, "complete", "Codexを起動しました");
    Ok(codex::CodexLaunchResult {
        operation_id: input.operation_id,
        terminal_pid,
        project_directory: project.display().to_string(),
        codex_home: codex_home.display().to_string(),
        config_path: config_path.display().to_string(),
        codex_version: installation.version,
        gateway_started_by_operation: started_by_operation,
        gateway_snapshot,
        warning: None,
    })
}

#[tauri::command]
fn desktop_cancel_exit(state: State<'_, GatewayStateStore>) -> Result<(), String> {
    state.exit_prompt_open.store(false, Ordering::Release);
    state.exit_approved.store(false, Ordering::Release);
    Ok(())
}

#[tauri::command]
fn desktop_confirm_exit(state: State<'_, GatewayStateStore>) -> Result<(), String> {
    state.exit_prompt_open.store(false, Ordering::Release);
    state.exit_approved.store(true, Ordering::Release);
    Ok(())
}

#[tauri::command]
fn deepseek_metadata() -> deepseek::DeepSeekMetadata {
    deepseek::metadata()
}

#[tauri::command]
async fn deepseek_configure(
    state: State<'_, GatewayStateStore>,
    input: deepseek::ConfigureInput,
) -> Result<deepseek::DeepSeekStatus, String> {
    let _guard = state.operation.lock().await;
    let (address, token) = management_connection(&state)?;
    deepseek::configure(
        &moonbridge_api::ApiClient::new(state.client.clone(), address, token),
        input,
    )
    .await
    .map_err(format_api_error)
}

#[tauri::command]
async fn deepseek_save(
    app: tauri::AppHandle,
    state: State<'_, GatewayStateStore>,
    input: DeepSeekSaveInput,
) -> Result<DeepSeekSaveResult, CommandError> {
    let _guard = state.operation.lock().await;
    let before = snapshot(&state).map_err(|error| {
        command_error(
            "save",
            "validating_input",
            "state_unavailable",
            error,
            false,
            &state,
        )
    })?;
    let configure_input = deepseek::ConfigureInput {
        api_key: input.api_key,
        model: input.model,
        reasoning_effort: input.reasoning_effort,
    };
    deepseek::validate_input(&configure_input).map_err(|error| {
        command_error(
            "save",
            "validating_input",
            "invalid_input",
            error.message,
            false,
            &state,
        )
    })?;
    emit_operation_progress(
        &app,
        &input.operation_id,
        "validating_input",
        "設定値を検証しています",
    );

    let started_by_us = !matches!(before.state, GatewayState::Running);
    if started_by_us {
        emit_operation_progress(
            &app,
            &input.operation_id,
            "starting_gateway",
            "Gatewayを一時起動しています",
        );
        if let Err(error) = start_gateway_inner(&app, &state).await {
            return Err(command_error(
                "save",
                "starting_gateway",
                "gateway_start_failed",
                error,
                false,
                &state,
            ));
        }
    }

    emit_operation_progress(
        &app,
        &input.operation_id,
        "loading_config",
        "現在の設定を読み込んでいます",
    );
    let (address, token) = management_connection(&state).map_err(|error| {
        command_error(
            "save",
            "loading_config",
            "gateway_unavailable",
            error,
            false,
            &state,
        )
    })?;
    emit_operation_progress(
        &app,
        &input.operation_id,
        "validating_config",
        "Gatewayの設定状態を確認しています",
    );
    let api = moonbridge_api::ApiClient::new(state.client.clone(), address, token);
    emit_operation_progress(
        &app,
        &input.operation_id,
        "saving",
        "DeepSeek設定を保存しています",
    );
    let configured = deepseek::configure(&api, configure_input).await;

    match configured {
        Ok(status) => {
            emit_operation_progress(
                &app,
                &input.operation_id,
                "verifying",
                "保存後の設定を確認しています",
            );
            let mut warning = None;
            if started_by_us {
                emit_operation_progress(
                    &app,
                    &input.operation_id,
                    "stopping_gateway",
                    "保存前の状態に戻しています",
                );
                if let Err(error) = stop_gateway_inner(&state).await {
                    warning = Some(format!(
                        "設定は保存されましたが、Gatewayを停止できませんでした: {error}"
                    ));
                }
            }
            let gateway_snapshot = snapshot(&state).map_err(|error| {
                command_error("save", "complete", "state_unavailable", error, true, &state)
            })?;
            let gateway_left_running = gateway_snapshot.pid.is_some();
            emit_operation_progress(
                &app,
                &input.operation_id,
                "complete",
                "DeepSeek設定を保存しました",
            );
            Ok(DeepSeekSaveResult {
                operation_id: input.operation_id,
                status,
                gateway_snapshot,
                gateway_left_running,
                warning,
            })
        }
        Err(error) => {
            let message = format_api_error(error);
            let mutation_started = !message.contains("API key is required");
            let should_restore = started_by_us && !mutation_started;
            if should_restore {
                emit_operation_progress(
                    &app,
                    &input.operation_id,
                    "stopping_gateway",
                    "保存前の状態に戻しています",
                );
                let _ = stop_gateway_inner(&state).await;
            }
            let (stage, code) = if message.contains("final_state_mismatch") {
                ("verifying", "final_state_mismatch")
            } else {
                ("saving", "config_save_failed")
            };
            Err(command_error(
                "save",
                stage,
                code,
                message,
                mutation_started,
                &state,
            ))
        }
    }
}

#[tauri::command]
async fn deepseek_test_connection(
    app: tauri::AppHandle,
    state: State<'_, GatewayStateStore>,
    input: OperationInput,
) -> Result<DeepSeekConnectionTestResult, CommandError> {
    let _guard = state.operation.lock().await;
    let before = snapshot(&state).map_err(|error| {
        command_error(
            "connection_test",
            "validating_input",
            "state_unavailable",
            error,
            false,
            &state,
        )
    })?;
    emit_operation_progress_for(
        &app,
        &input.operation_id,
        "connection_test",
        "validating_input",
        "保存済み設定を検証しています",
    );
    let started_by_us = !matches!(before.state, GatewayState::Running);
    if started_by_us {
        emit_operation_progress_for(
            &app,
            &input.operation_id,
            "connection_test",
            "starting_gateway",
            "Gatewayを一時起動しています",
        );
        if let Err(error) = start_gateway_inner(&app, &state).await {
            return Err(command_error(
                "connection_test",
                "starting_gateway",
                "gateway_start_failed",
                error,
                false,
                &state,
            ));
        }
    }
    emit_operation_progress_for(
        &app,
        &input.operation_id,
        "connection_test",
        "loading_config",
        "保存済み設定を読み込んでいます",
    );
    let (address, token) = match management_connection(&state) {
        Ok(connection) => connection,
        Err(error) => {
            if started_by_us {
                let _ = stop_gateway_inner(&state).await;
            }
            return Err(command_error(
                "connection_test",
                "loading_config",
                "gateway_unavailable",
                error,
                false,
                &state,
            ));
        }
    };
    emit_operation_progress_for(
        &app,
        &input.operation_id,
        "connection_test",
        "testing",
        "DeepSeekへ接続しています",
    );
    let api = moonbridge_api::ApiClient::new(state.client.clone(), address, token);
    let test_result = match deepseek::test_connection(&api).await {
        Ok(result) => result,
        Err(error) => {
            let message = format_api_error(error);
            if started_by_us {
                emit_operation_progress_for(
                    &app,
                    &input.operation_id,
                    "connection_test",
                    "stopping_gateway",
                    "保存前の状態に戻しています",
                );
                let _ = stop_gateway_inner(&state).await;
            }
            let code = if message.contains("network failure") {
                "network_failure"
            } else if message.contains("invalid connection-test response") {
                "invalid_response"
            } else {
                "connection_test_failed"
            };
            return Err(command_error(
                "connection_test",
                "testing",
                code,
                message,
                false,
                &state,
            ));
        }
    };
    let mut warning = None;
    if started_by_us {
        emit_operation_progress_for(
            &app,
            &input.operation_id,
            "connection_test",
            "stopping_gateway",
            "保存前の状態に戻しています",
        );
        if let Err(error) = stop_gateway_inner(&state).await {
            warning = Some(format!(
                "接続テストは完了しましたが、Gatewayを停止できませんでした: {error}"
            ));
        }
    }
    let gateway_snapshot = snapshot(&state).map_err(|error| {
        command_error(
            "connection_test",
            "complete",
            "state_unavailable",
            error,
            false,
            &state,
        )
    })?;
    let gateway_left_running = gateway_snapshot.pid.is_some();
    emit_operation_progress_for(
        &app,
        &input.operation_id,
        "connection_test",
        "complete",
        &test_result.message,
    );
    Ok(DeepSeekConnectionTestResult {
        operation_id: input.operation_id,
        result: test_result,
        gateway_snapshot,
        gateway_left_running,
        warning,
    })
}

#[tauri::command]
async fn traffic_analysis_status(
    state: State<'_, GatewayStateStore>,
) -> Result<traffic_analysis::TrafficAnalysisStatus, String> {
    let result = traffic_analysis::status(&state).await;
    if let Ok(status) = &result {
        state
            .traffic_analysis_active
            .store(status.relay_active, Ordering::Release);
    }
    result
}

#[tauri::command]
async fn traffic_analysis_start(
    app: tauri::AppHandle,
    state: State<'_, GatewayStateStore>,
    input: TrafficAnalysisInput,
) -> Result<traffic_analysis::TrafficAnalysisResult, traffic_analysis::TrafficCommandError> {
    let invocation_id = traffic_analysis::next_journal_id(&state).unwrap_or(0);
    traffic_analysis::journal_command(
        &state,
        invocation_id,
        None,
        "traffic_analysis_start",
        "invoke",
        None,
    );
    let _guard = state.operation.lock().await;
    traffic_analysis::journal_command(
        &state,
        invocation_id,
        None,
        "traffic_analysis_start",
        "acquired",
        None,
    );
    let result = traffic_analysis::start(&app, &state, &input.operation_id).await;
    if let Ok(result) = &result {
        state
            .traffic_analysis_active
            .store(result.status.relay_active, Ordering::Release);
    }
    let end_session_id = state.session_id_snapshot();
    traffic_analysis::journal_command(
        &state,
        invocation_id,
        end_session_id.as_deref(),
        "traffic_analysis_start",
        "end",
        Some(traffic_analysis::normalize_result(&result)),
    );
    result
}

#[tauri::command]
async fn traffic_analysis_restart_capture(
    app: tauri::AppHandle,
    state: State<'_, GatewayStateStore>,
    input: TrafficAnalysisInput,
) -> Result<traffic_analysis::TrafficAnalysisResult, traffic_analysis::TrafficCommandError> {
    let _guard = state.operation.lock().await;
    let result = traffic_analysis::restart_capture(&app, &state, &input.operation_id).await;
    if let Ok(result) = &result {
        state
            .traffic_analysis_active
            .store(result.status.relay_active, Ordering::Release);
    }
    result
}

#[tauri::command]
async fn traffic_analysis_stop(
    app: tauri::AppHandle,
    state: State<'_, GatewayStateStore>,
    input: TrafficAnalysisInput,
) -> Result<traffic_analysis::TrafficAnalysisResult, traffic_analysis::TrafficCommandError> {
    let session_id = state.session_id_snapshot();
    let invocation_id = traffic_analysis::next_journal_id(&state).unwrap_or(0);
    traffic_analysis::journal_command(
        &state,
        invocation_id,
        session_id.as_deref(),
        "traffic_analysis_stop",
        "invoke",
        None,
    );
    let _guard = state.operation.lock().await;
    traffic_analysis::journal_command(
        &state,
        invocation_id,
        session_id.as_deref(),
        "traffic_analysis_stop",
        "acquired",
        None,
    );
    let result = traffic_analysis::stop(&app, &state, &input.operation_id).await;
    if let Ok(result) = &result {
        state
            .traffic_analysis_active
            .store(result.status.relay_active, Ordering::Release);
    }
    traffic_analysis::journal_command(
        &state,
        invocation_id,
        session_id.as_deref(),
        "traffic_analysis_stop",
        "end",
        Some(traffic_analysis::normalize_result(&result)),
    );
    result
}

#[tauri::command]
async fn traffic_analysis_finish_relay(
    app: tauri::AppHandle,
    state: State<'_, GatewayStateStore>,
    input: TrafficAnalysisInput,
) -> Result<traffic_analysis::TrafficAnalysisResult, traffic_analysis::TrafficCommandError> {
    let session_id = state.session_id_snapshot();
    let invocation_id = traffic_analysis::next_journal_id(&state).unwrap_or(0);
    traffic_analysis::journal_command(
        &state,
        invocation_id,
        session_id.as_deref(),
        "traffic_analysis_finish_relay",
        "invoke",
        None,
    );
    let _guard = state.operation.lock().await;
    traffic_analysis::journal_command(
        &state,
        invocation_id,
        session_id.as_deref(),
        "traffic_analysis_finish_relay",
        "acquired",
        None,
    );
    let result =
        traffic_analysis::finish_relay(&app, &state, &input.operation_id, input.discard_unsaved)
            .await;
    if let Ok(result) = &result {
        state
            .traffic_analysis_active
            .store(result.status.relay_active, Ordering::Release);
    }
    traffic_analysis::journal_command(
        &state,
        invocation_id,
        session_id.as_deref(),
        "traffic_analysis_finish_relay",
        "end",
        Some(traffic_analysis::normalize_result(&result)),
    );
    result
}

#[tauri::command]
async fn traffic_analysis_clear(
    state: State<'_, GatewayStateStore>,
    input: TrafficAnalysisInput,
) -> Result<traffic_analysis::TrafficAnalysisStatus, traffic_analysis::TrafficCommandError> {
    let _guard = state.operation.lock().await;
    traffic_analysis::clear(&state, &input.operation_id).await
}

#[tauri::command]
async fn traffic_analysis_retry_autosave(
    state: State<'_, GatewayStateStore>,
    input: TrafficAnalysisInput,
) -> Result<traffic_analysis::TrafficAnalysisStatus, traffic_analysis::TrafficCommandError> {
    let _guard = state.operation.lock().await;
    traffic_analysis::retry_autosave(&state, &input.operation_id).await
}

#[tauri::command]
async fn traffic_analysis_observations(
    state: State<'_, GatewayStateStore>,
    input: TrafficObservationInput,
) -> Result<traffic_analysis::TrafficObservationPage, String> {
    traffic_analysis::observations(&state, input.after).await
}

#[tauri::command]
async fn traffic_analysis_export(
    state: State<'_, GatewayStateStore>,
    app: tauri::AppHandle,
    input: TrafficExportInput,
) -> Result<traffic_analysis::TrafficExportResult, traffic_analysis::TrafficCommandError> {
    let _guard = state.operation.lock().await;
    traffic_analysis::export(&app, &state, &input.operation_id, &input.destination).await
}

#[tauri::command]
async fn traffic_analysis_reveal_export(
    state: State<'_, GatewayStateStore>,
    input: TrafficRevealExportInput,
) -> Result<traffic_analysis::TrafficRevealResult, traffic_analysis::TrafficCommandError> {
    let _guard = state.operation.lock().await;
    traffic_analysis::reveal_export(&state, &input.operation_id, &input.destination).await
}

#[tauri::command]
async fn traffic_analysis_open_log_folder(
    state: State<'_, GatewayStateStore>,
) -> Result<(), traffic_analysis::TrafficCommandError> {
    let _guard = state.operation.lock().await;
    traffic_analysis::open_log_folder(&state)
}

#[tauri::command]
async fn traffic_analysis_restore_config(
    app: tauri::AppHandle,
    state: State<'_, GatewayStateStore>,
    input: traffic_analysis::RestoreInput,
) -> Result<traffic_analysis::TrafficAnalysisResult, traffic_analysis::TrafficCommandError> {
    let _guard = state.operation.lock().await;
    let result = traffic_analysis::restore_config(&app, &state, &input).await;
    if let Ok(result) = &result {
        state
            .traffic_analysis_active
            .store(result.status.relay_active, Ordering::Release);
    }
    result
}

fn emit_codex_progress(app: &tauri::AppHandle, operation_id: &str, stage: &str, message: &str) {
    let _ = app.emit(
        "codex-operation-progress",
        codex::CodexOperationProgress {
            operation_id: operation_id.to_string(),
            operation: "launch".to_string(),
            stage: stage.to_string(),
            message: message.to_string(),
        },
    );
}

fn error_code<'a>(error: &'a str, fallback: &'a str) -> &'a str {
    error
        .split_once(':')
        .map(|(code, _)| code)
        .unwrap_or(fallback)
}

fn error_message(error: &str) -> String {
    error
        .split_once(':')
        .map(|(_, message)| message.trim().to_string())
        .unwrap_or_else(|| error.to_string())
}

fn codex_error(
    input: &codex::CodexLaunchInput,
    stage: &str,
    code: &str,
    message: impl Into<String>,
    gateway_started_by_operation: bool,
    state: &GatewayStateStore,
) -> codex::CodexCommandError {
    let gateway_snapshot = snapshot(state).ok();
    let gateway_left_running = gateway_snapshot
        .as_ref()
        .and_then(|snapshot| snapshot.pid)
        .is_some();
    codex::CodexCommandError {
        operation: "launch".to_string(),
        operation_id: input.operation_id.clone(),
        stage: stage.to_string(),
        code: code.to_string(),
        message: message.into(),
        field: if code == "invalid_project" {
            Some("projectDirectory".to_string())
        } else {
            None
        },
        retryable: matches!(
            code,
            "gateway_unavailable"
                | "route_not_configured"
                | "config_generation_failed"
                | "config_publish_failed"
                | "terminal_launch_failed"
                | "state_unavailable"
        ),
        gateway_started_by_operation,
        gateway_left_running,
        gateway_snapshot,
    }
}

async fn codex_failure(
    input: &codex::CodexLaunchInput,
    stage: &str,
    code: &str,
    message: impl Into<String>,
    gateway_started_by_operation: bool,
    state: &GatewayStateStore,
) -> codex::CodexCommandError {
    let mut message = message.into();
    if gateway_started_by_operation {
        if stop_gateway_inner(state).await.is_err() {
            message.push_str(" Gatewayを元の停止状態へ戻せませんでした。");
        }
    }
    codex_error(
        input,
        stage,
        code,
        message,
        gateway_started_by_operation,
        state,
    )
}

fn management_connection(state: &GatewayStateStore) -> Result<(String, String), String> {
    let current = snapshot(state)?;
    if !matches!(current.state, GatewayState::Running) {
        return Err("Moon Bridge gateway is not running".to_string());
    }
    let token = state
        .token
        .lock()
        .map_err(|error| error.to_string())?
        .clone()
        .ok_or_else(|| "Moon Bridge desktop token is unavailable".to_string())?;
    Ok((current.address, token))
}

fn command_error(
    operation: &str,
    stage: &str,
    code: &str,
    message: impl Into<String>,
    mutation_started: bool,
    state: &GatewayStateStore,
) -> CommandError {
    let gateway_snapshot = snapshot(state).ok();
    let gateway_left_running = gateway_snapshot
        .as_ref()
        .and_then(|snapshot| snapshot.pid)
        .is_some();
    CommandError {
        operation: operation.to_string(),
        stage: stage.to_string(),
        code: code.to_string(),
        message: message.into(),
        field: None,
        retryable: matches!(code, "final_state_mismatch" | "connection_test_failed"),
        mutation_started,
        gateway_left_running,
        gateway_snapshot,
    }
}

fn format_api_error(error: moonbridge_api::ApiError) -> String {
    match error.status {
        Some(401) | Some(403) => "Moon Bridge authorization failed".to_string(),
        Some(409) => "Moon Bridge configuration changed; retry the operation".to_string(),
        _ => error.message,
    }
}

fn set_snapshot(
    state: &GatewayStateStore,
    status: GatewayState,
    address: String,
    pid: Option<u32>,
    instance_id: Option<String>,
    error: Option<String>,
) -> Result<(), String> {
    let mut snapshot = state.snapshot.lock().map_err(|e| e.to_string())?;
    snapshot.state = status;
    snapshot.address = address;
    snapshot.pid = pid;
    snapshot.instance_id = instance_id;
    snapshot.error = error;
    Ok(())
}

fn snapshot(state: &GatewayStateStore) -> Result<GatewaySnapshot, String> {
    Ok(state.snapshot.lock().map_err(|e| e.to_string())?.clone())
}

fn fail(state: &GatewayStateStore, error: String) -> Result<GatewaySnapshot, String> {
    let _ = set_snapshot(
        state,
        GatewayState::Error,
        "127.0.0.1:38440".to_string(),
        None,
        None,
        Some(error.clone()),
    );
    Err(error)
}

fn error_or(value: &str, fallback: &str) -> String {
    if value.is_empty() {
        fallback.to_string()
    } else {
        value.to_string()
    }
}

fn emit_log(app: &tauri::AppHandle, stream: &str, line: String) {
    let _ = app.emit(
        "gateway-log",
        GatewayLog {
            stream: stream.to_string(),
            line,
            timestamp: Utc::now(),
        },
    );
}

fn emit_operation_progress(app: &tauri::AppHandle, operation_id: &str, stage: &str, message: &str) {
    emit_operation_progress_for(app, operation_id, "save", stage, message);
}

fn emit_operation_progress_for(
    app: &tauri::AppHandle,
    operation_id: &str,
    operation: &str,
    stage: &str,
    message: &str,
) {
    let _ = app.emit(
        "deepseek-operation-progress",
        OperationProgress {
            operation_id: operation_id.to_string(),
            operation: operation.to_string(),
            stage: stage.to_string(),
            message: message.to_string(),
        },
    );
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_shell::init())
        .manage(GatewayStateStore::new())
        .setup(|app| {
            let local_data_dir = app
                .path()
                .local_data_dir()
                .map_err(|error| std::io::Error::other(error.to_string()))?;
            app.state::<GatewayStateStore>()
                .set_app_paths(paths::AppPaths::from_local_data_dir(local_data_dir))
                .map_err(std::io::Error::other)?;
            let journal_path =
                traffic_analysis::command_journal_path(&app.state::<GatewayStateStore>())
                    .unwrap_or_default();
            if let Ok(mut journal) = app.state::<GatewayStateStore>().command_journal.lock() {
                journal.configure(journal_path);
            }
            // Reconcile only persisted metadata and the current Codex file.
            // This never starts Gateway or republishes openai_base_url.
            let _ = traffic_analysis::reconcile_startup(&app.state::<GatewayStateStore>());
            spawn_gateway_monitor(app.handle().clone());
            Ok(())
        })
        .on_window_event(|window, event| {
            if let WindowEvent::CloseRequested { api, .. } = event {
                let state = window.state::<GatewayStateStore>();
                let gateway_running = snapshot(&state)
                    .map(|snapshot| matches!(snapshot.state, GatewayState::Running))
                    .unwrap_or(false);
                let should_prompt = state.codex_launched_this_run.load(Ordering::Acquire)
                    && gateway_running
                    && !state.exit_approved.load(Ordering::Acquire);
                let traffic_active = state.traffic_analysis_active.load(Ordering::Acquire)
                    && !state.exit_approved.load(Ordering::Acquire);
                if traffic_active || should_prompt {
                    api.prevent_close();
                    if !state.exit_prompt_open.swap(true, Ordering::AcqRel) {
                        let event = if traffic_active {
                            "traffic_analysis"
                        } else {
                            "codex"
                        };
                        let _ = window.emit("desktop-exit-confirmation-requested", event);
                    }
                }
            }
        })
        .invoke_handler(tauri::generate_handler![
            gateway_status,
            gateway_start,
            gateway_stop,
            open_config_folder,
            deepseek_metadata,
            deepseek_status,
            deepseek_configure,
            deepseek_save,
            deepseek_test_connection,
            traffic_analysis_status,
            traffic_analysis_start,
            traffic_analysis_restart_capture,
            traffic_analysis_stop,
            traffic_analysis_finish_relay,
            traffic_analysis_clear,
            traffic_analysis_retry_autosave,
            traffic_analysis_observations,
            traffic_analysis_export,
            traffic_analysis_reveal_export,
            traffic_analysis_open_log_folder,
            traffic_analysis_restore_config,
            codex_status,
            codex_launch,
            desktop_cancel_exit,
            desktop_confirm_exit
        ])
        .run(tauri::generate_context!())
        .expect("error while running Moon Bridge Desktop");
}
