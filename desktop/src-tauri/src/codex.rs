use serde::Serialize;
use std::ffi::OsStr;
use std::fs::{self, OpenOptions};
use std::future::Future;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::time::Duration;
use tauri::AppHandle;
use tauri_plugin_shell::ShellExt;
use tokio::process::Command;
use tokio::time::timeout;
use uuid::Uuid;

const CODEX_ROUTE_ALIAS: &str = "moonbridge";
const CODEX_BASE_URL: &str = "http://127.0.0.1:38440/v1";
const VERSION_TIMEOUT: Duration = Duration::from_secs(5);

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CodexStatus {
    pub installed: bool,
    pub executable_path: Option<String>,
    pub version: Option<String>,
    pub codex_home: String,
    pub config_path: String,
    pub config_exists: bool,
    pub route_alias: String,
}

#[derive(Clone, Debug)]
pub struct CodexInstallation {
    pub executable_path: PathBuf,
    pub version: String,
}

#[derive(Clone, Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodexLaunchInput {
    pub operation_id: String,
    pub project_directory: String,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CodexLaunchResult {
    pub operation_id: String,
    pub terminal_pid: u32,
    pub project_directory: String,
    pub codex_home: String,
    pub config_path: String,
    pub codex_version: String,
    pub gateway_started_by_operation: bool,
    pub gateway_snapshot: crate::GatewaySnapshot,
    pub warning: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CodexCommandError {
    pub operation: String,
    pub operation_id: String,
    pub stage: String,
    pub code: String,
    pub message: String,
    pub field: Option<String>,
    pub retryable: bool,
    pub gateway_started_by_operation: bool,
    pub gateway_left_running: bool,
    pub gateway_snapshot: Option<crate::GatewaySnapshot>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CodexOperationProgress {
    pub operation_id: String,
    pub operation: String,
    pub stage: String,
    pub message: String,
}

pub fn validate_project_directory(input: &str) -> Result<PathBuf, String> {
    let trimmed = input.trim();
    if trimmed.is_empty() {
        return Err("invalid_project: project directory is required".to_string());
    }

    let path = PathBuf::from(trimmed);
    if !path.is_absolute() {
        return Err("invalid_project: project directory must be absolute".to_string());
    }
    let metadata = fs::metadata(&path).map_err(|error| {
        if error.kind() == std::io::ErrorKind::NotFound {
            "project_not_found: project directory does not exist".to_string()
        } else {
            format!("invalid_project: cannot inspect project directory: {error}")
        }
    })?;
    if !metadata.is_dir() {
        return Err("project_not_directory: selected project path is not a directory".to_string());
    }
    fs::canonicalize(&path)
        .map_err(|error| format!("invalid_project: cannot canonicalize project directory: {error}"))
}

pub fn parse_where_output(output: &str) -> Vec<PathBuf> {
    output
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty())
        .map(PathBuf::from)
        .filter(|path| is_codex_candidate(path))
        .collect()
}

pub fn is_codex_candidate(path: &Path) -> bool {
    if !path.is_file() {
        return false;
    }
    matches!(
        path.extension()
            .and_then(OsStr::to_str)
            .map(|extension| extension.to_ascii_lowercase())
            .as_deref(),
        Some("exe" | "cmd" | "bat")
    )
}

pub async fn discover_codex() -> Result<CodexInstallation, String> {
    let output = Command::new("where.exe")
        .arg("codex")
        .output()
        .await
        .map_err(|error| format!("codex_not_found: cannot run where.exe: {error}"))?;
    if !output.status.success() {
        return Err("codex_not_found: Codex CLI was not found in PATH".to_string());
    }

    let candidates = parse_where_output(&String::from_utf8_lossy(&output.stdout));
    find_installation(candidates, |candidate| Box::pin(probe_version(candidate))).await
}

type ProbeFuture = Pin<Box<dyn Future<Output = Result<String, String>> + Send>>;

async fn find_installation<F>(
    candidates: Vec<PathBuf>,
    mut probe: F,
) -> Result<CodexInstallation, String>
where
    F: FnMut(PathBuf) -> ProbeFuture,
{
    if candidates.is_empty() {
        return Err("codex_not_found: no valid Codex executable was found".to_string());
    }

    let mut last_error = None;
    for candidate in candidates {
        match probe(candidate.clone()).await {
            Ok(version) => {
                return Ok(CodexInstallation {
                    executable_path: candidate,
                    version,
                })
            }
            Err(error) => last_error = Some(error),
        }
    }
    Err(last_error
        .unwrap_or_else(|| "codex_version_failed: all Codex version checks failed".to_string()))
}

pub fn version_command_args(candidate: &Path) -> Option<Vec<String>> {
    let extension = candidate
        .extension()
        .and_then(OsStr::to_str)
        .map(|value| value.to_ascii_lowercase())?;
    if extension == "cmd" || extension == "bat" {
        let command = format!("\"{}\" --version", candidate.display());
        Some(vec!["/D".to_string(), "/C".to_string(), command])
    } else if extension == "exe" {
        Some(vec!["--version".to_string()])
    } else {
        None
    }
}

pub fn powershell_launch_command(candidate: &Path) -> Result<String, String> {
    if !is_codex_candidate(candidate) {
        return Err(
            "codex_not_found: selected Codex executable is no longer available".to_string(),
        );
    }
    let escaped = candidate.to_string_lossy().replace('\'', "''");
    Ok(format!("& '{escaped}'"))
}

#[cfg(windows)]
pub fn spawn_visible_terminal(
    candidate: &Path,
    project: &Path,
    codex_home: &Path,
) -> Result<u32, String> {
    use std::os::windows::process::CommandExt;

    let command_line = powershell_launch_command(candidate)?;
    let child = std::process::Command::new("powershell.exe")
        .args([
            "-NoLogo",
            "-NoProfile",
            "-NoExit",
            "-Command",
            &command_line,
        ])
        .current_dir(project)
        .env("CODEX_HOME", codex_home)
        .creation_flags(0x00000010)
        .spawn()
        .map_err(|error| format!("terminal_launch_failed: cannot start PowerShell: {error}"))?;
    Ok(child.id())
}

#[cfg(not(windows))]
pub fn spawn_visible_terminal(
    _candidate: &Path,
    _project: &Path,
    _codex_home: &Path,
) -> Result<u32, String> {
    Err("terminal_launch_failed: Codex launch is supported on Windows only".to_string())
}

async fn probe_version(candidate: PathBuf) -> Result<String, String> {
    let args = version_command_args(&candidate)
        .ok_or_else(|| "codex_version_failed: unsupported Codex executable type".to_string())?;
    let mut command = if matches!(
        candidate
            .extension()
            .and_then(OsStr::to_str)
            .map(|value| value.to_ascii_lowercase())
            .as_deref(),
        Some("cmd" | "bat")
    ) {
        let mut command = Command::new("cmd.exe");
        command.args(args);
        command
    } else {
        let mut command = Command::new(candidate);
        command.args(args);
        command
    };

    let output = timeout(VERSION_TIMEOUT, command.output())
        .await
        .map_err(|_| "codex_version_failed: Codex version check timed out".to_string())?
        .map_err(|error| format!("codex_version_failed: cannot run Codex: {error}"))?;
    if !output.status.success() {
        return Err("codex_version_failed: Codex version check failed".to_string());
    }
    String::from_utf8_lossy(&output.stdout)
        .lines()
        .map(str::trim)
        .find(|line| !line.is_empty())
        .map(ToOwned::to_owned)
        .ok_or_else(|| "codex_version_failed: Codex returned an empty version".to_string())
}

pub async fn status() -> Result<CodexStatus, String> {
    let home = crate::paths::codex_home()?;
    let config_path = crate::paths::codex_config_path()?;
    let installation = discover_codex().await.ok();
    Ok(CodexStatus {
        installed: installation.is_some(),
        executable_path: installation
            .as_ref()
            .map(|value| value.executable_path.display().to_string()),
        version: installation.as_ref().map(|value| value.version.clone()),
        codex_home: home.display().to_string(),
        config_path: config_path.display().to_string(),
        config_exists: config_path.is_file(),
        route_alias: CODEX_ROUTE_ALIAS.to_string(),
    })
}

pub async fn generate_config(
    app: &AppHandle,
    config_path: &Path,
    codex_home: &Path,
) -> Result<Vec<u8>, String> {
    fs::create_dir_all(codex_home)
        .map_err(|error| format!("config_generation_failed: cannot create Codex home: {error}"))?;
    let config_path = config_path.to_string_lossy().into_owned();
    let codex_home = codex_home.to_string_lossy().into_owned();
    let args = vec![
        "-config".to_string(),
        config_path,
        "-print-codex-config".to_string(),
        CODEX_ROUTE_ALIAS.to_string(),
        "-codex-base-url".to_string(),
        CODEX_BASE_URL.to_string(),
        "-codex-home".to_string(),
        codex_home,
    ];
    let output = app
        .shell()
        .sidecar("moonbridge")
        .map_err(|error| format!("config_generation_failed: sidecar is not configured: {error}"))?
        .args(args)
        .output()
        .await
        .map_err(|error| format!("config_generation_failed: sidecar execution failed: {error}"))?;
    if !output.status.success() {
        return Err("config_generation_failed: sidecar config generation failed".to_string());
    }
    if output.stdout.is_empty() {
        return Err("config_generation_failed: sidecar returned empty config".to_string());
    }
    Ok(output.stdout)
}

pub fn publish_config(codex_home: &Path, content: &[u8]) -> Result<PathBuf, String> {
    fs::create_dir_all(codex_home)
        .map_err(|error| format!("config_publish_failed: cannot create Codex home: {error}"))?;
    if content.is_empty() {
        return Err("config_publish_failed: generated config is empty".to_string());
    }

    let destination = codex_home.join("config.toml");
    let temporary = codex_home.join(format!(".config.toml.{}.tmp", Uuid::new_v4().simple()));
    let result = (|| {
        let mut file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temporary)
            .map_err(|error| format!("config_publish_failed: create temporary config: {error}"))?;
        file.write_all(content)
            .map_err(|error| format!("config_publish_failed: write temporary config: {error}"))?;
        file.sync_all()
            .map_err(|error| format!("config_publish_failed: sync temporary config: {error}"))?;
        drop(file);
        replace_file(&temporary, &destination)
            .map_err(|error| format!("config_publish_failed: replace config: {error}"))?;
        Ok(destination.clone())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result
}

#[cfg(unix)]
fn replace_file(source: &Path, destination: &Path) -> std::io::Result<()> {
    fs::rename(source, destination)
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
    fs::rename(source, destination)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::File;

    fn temp_dir(name: &str) -> PathBuf {
        let path = std::env::temp_dir().join(format!(
            "moon-bridge-codex-{name}-{}",
            Uuid::new_v4().simple()
        ));
        fs::create_dir_all(&path).expect("create temp directory");
        path
    }

    #[test]
    fn validates_absolute_existing_directory_with_spaces_and_unicode() {
        let root = temp_dir("日本語 path");
        let result = validate_project_directory(&root.to_string_lossy()).expect("valid directory");
        assert_eq!(result, fs::canonicalize(&root).expect("canonical path"));
        fs::remove_dir_all(root).expect("remove temp directory");
    }

    #[test]
    fn rejects_empty_relative_missing_and_file_paths() {
        assert!(validate_project_directory("  ")
            .unwrap_err()
            .starts_with("invalid_project"));
        assert!(validate_project_directory("relative-project")
            .unwrap_err()
            .starts_with("invalid_project"));
        assert!(
            validate_project_directory("C:\\does-not-exist\\moon-bridge")
                .unwrap_err()
                .starts_with("project_not_found")
        );

        let root = temp_dir("file");
        let file = root.join("project.txt");
        File::create(&file).expect("create file");
        assert!(validate_project_directory(&file.to_string_lossy())
            .unwrap_err()
            .starts_with("project_not_directory"));
        fs::remove_dir_all(root).expect("remove temp directory");
    }

    #[test]
    fn parses_first_existing_candidate_in_path_order() {
        let root = temp_dir("candidates");
        let first = root.join("first.cmd");
        let second = root.join("second.exe");
        File::create(&first).expect("create first candidate");
        File::create(&second).expect("create second candidate");
        let output = format!(
            "{}\r\n{}\r\n{}\r\n",
            first.display(),
            root.join("missing.exe").display(),
            second.display()
        );
        let candidates = parse_where_output(&output);
        assert_eq!(candidates, vec![first, second]);
        fs::remove_dir_all(root).expect("remove temp directory");
    }

    #[test]
    fn filters_extensionless_and_powershell_candidates() {
        let root = temp_dir("unsupported-candidates");
        let extensionless = root.join("codex");
        let powershell = root.join("codex.ps1");
        let command = root.join("codex.cmd");
        File::create(&extensionless).expect("create extensionless shim");
        File::create(&powershell).expect("create PowerShell shim");
        File::create(&command).expect("create command shim");
        let output = format!(
            "{}\r\n{}\r\n{}\r\n",
            extensionless.display(),
            powershell.display(),
            command.display()
        );
        assert_eq!(parse_where_output(&output), vec![command]);
        fs::remove_dir_all(root).expect("remove temp directory");
    }

    #[test]
    fn preserves_supported_candidate_path_order() {
        let root = temp_dir("supported-order");
        let executable = root.join("codex.exe");
        let command = root.join("codex.cmd");
        File::create(&executable).expect("create executable candidate");
        File::create(&command).expect("create command candidate");
        let output = format!("{}\r\n{}\r\n", executable.display(), command.display());
        assert_eq!(parse_where_output(&output), vec![executable, command]);
        fs::remove_dir_all(root).expect("remove temp directory");
    }

    #[test]
    fn skips_a_supported_candidate_when_version_probe_fails() {
        let root = temp_dir("probe-fallback");
        let executable = root.join("codex.exe");
        let command = root.join("codex.cmd");
        File::create(&executable).expect("create executable candidate");
        File::create(&command).expect("create command candidate");
        let result = tauri::async_runtime::block_on(find_installation(
            vec![executable.clone(), command.clone()],
            |candidate| {
                Box::pin(async move {
                    if candidate.extension().and_then(OsStr::to_str) == Some("exe") {
                        Err("codex_version_failed: access denied".to_string())
                    } else {
                        Ok("codex-cli 0.142.0".to_string())
                    }
                })
            },
        ))
        .expect("fallback candidate should be selected");
        assert_eq!(result.executable_path, command);
        assert_eq!(result.version, "codex-cli 0.142.0");
        fs::remove_dir_all(root).expect("remove temp directory");
    }

    #[test]
    fn empty_where_output_has_no_candidates() {
        assert!(parse_where_output("\r\n  \n").is_empty());
    }

    #[test]
    fn builds_shim_version_command_without_losing_spaces() {
        let candidate = PathBuf::from(r"C:\Program Files\Codex\codex.cmd");
        assert_eq!(
            version_command_args(&candidate),
            Some(vec![
                "/D".to_string(),
                "/C".to_string(),
                r#""C:\Program Files\Codex\codex.cmd" --version"#.to_string(),
            ])
        );
    }

    #[test]
    fn builds_terminal_command_without_project_path_or_secrets() {
        let root = temp_dir("terminal");
        let candidate = root.join("codex.cmd");
        File::create(&candidate).expect("create candidate");
        let command = powershell_launch_command(&candidate).expect("build command");
        assert!(command.contains("codex.cmd"));
        assert!(!command.contains("project"));
        assert!(!command.contains("api_key"));
        fs::remove_dir_all(root).expect("remove temp directory");
    }

    #[test]
    fn publishes_exact_content_and_preserves_old_config_on_empty_content() {
        let root = temp_dir("publish");
        let destination = root.join("config.toml");
        fs::write(&destination, b"old config\n").expect("write old config");
        assert_eq!(
            publish_config(&root, b"model = \"moonbridge\"\n").expect("publish"),
            destination
        );
        assert_eq!(
            fs::read(&destination).expect("read config"),
            b"model = \"moonbridge\"\n"
        );
        assert!(publish_config(&root, b"").is_err());
        assert_eq!(
            fs::read(&destination).expect("read preserved config"),
            b"model = \"moonbridge\"\n"
        );
        fs::remove_dir_all(root).expect("remove temp directory");
    }

    #[test]
    fn codex_paths_are_desktop_owned_not_user_codex_home() {
        let root = std::env::var_os("APPDATA")
            .or_else(|| std::env::var_os("USERPROFILE"))
            .expect("Windows profile");
        let home = crate::paths::codex_home().expect("codex home");
        assert_eq!(
            home,
            PathBuf::from(root)
                .join("Moon Bridge Desktop")
                .join("codex-home")
        );
        assert!(!home.ends_with(".codex"));
    }
}
