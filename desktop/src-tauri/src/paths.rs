use std::path::PathBuf;

#[derive(Clone, Debug)]
pub struct AppPaths {
    local_root: PathBuf,
}

impl AppPaths {
    pub fn from_local_data_dir(local_data_dir: PathBuf) -> Self {
        Self {
            local_root: local_data_dir.join("Moon Bridge"),
        }
    }

    pub fn from_environment() -> Result<Self, String> {
        let root = std::env::var_os("LOCALAPPDATA")
            .or_else(|| std::env::var_os("APPDATA"))
            .or_else(|| std::env::var_os("USERPROFILE"))
            .ok_or_else(|| "LOCALAPPDATA/APPDATA/USERPROFILE is not available".to_string())?;
        Ok(Self::from_local_data_dir(PathBuf::from(root)))
    }

    pub fn local_root(&self) -> &PathBuf {
        &self.local_root
    }

    pub fn recovery_dir(&self) -> PathBuf {
        self.local_root().join("recovery")
    }

    pub fn recovery_state_path(&self) -> PathBuf {
        self.recovery_dir().join("recovery-state-v2.json")
    }

    pub fn legacy_recovery_state_path(&self) -> Result<PathBuf, String> {
        let root = std::env::var_os("APPDATA")
            .or_else(|| std::env::var_os("USERPROFILE"))
            .ok_or_else(|| "APPDATA/USERPROFILE is not available".to_string())?;
        Ok(PathBuf::from(root)
            .join("Moon Bridge Desktop")
            .join("traffic-analysis")
            .join("integration-state.json"))
    }

    pub fn backup_dir(&self) -> PathBuf {
        self.local_root().join("backups").join("codex-config")
    }

    #[cfg_attr(debug_assertions, allow(dead_code))]
    pub fn traffic_log_dir(&self) -> PathBuf {
        self.local_root().join("logs").join("traffic-analysis")
    }

    // Only referenced from the release branch of command_journal_path.
    #[cfg_attr(debug_assertions, allow(dead_code))]
    pub fn command_journal_path(&self) -> PathBuf {
        self.local_root().join("logs").join("command-journal.jsonl")
    }
}

pub fn data_dir() -> Result<PathBuf, String> {
    let root = std::env::var_os("APPDATA")
        .or_else(|| std::env::var_os("USERPROFILE"))
        .ok_or_else(|| "APPDATA/USERPROFILE is not available".to_string())?;
    Ok(PathBuf::from(root).join("Moon Bridge Desktop"))
}

pub fn config_path() -> Result<PathBuf, String> {
    Ok(data_dir()?.join("config.yml"))
}

pub fn codex_home() -> Result<PathBuf, String> {
    Ok(data_dir()?.join("codex-home"))
}

pub fn codex_config_path() -> Result<PathBuf, String> {
    Ok(codex_home()?.join("config.toml"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn app_paths_keep_recovery_and_backup_categories_under_local_root() {
        let paths = AppPaths::from_local_data_dir(PathBuf::from(r"C:\Temp\moon-bridge"));
        assert_eq!(
            paths.recovery_state_path(),
            PathBuf::from(r"C:\Temp\moon-bridge\Moon Bridge\recovery\recovery-state-v2.json")
        );
        assert_eq!(
            paths.backup_dir(),
            PathBuf::from(r"C:\Temp\moon-bridge\Moon Bridge\backups\codex-config")
        );
        assert_eq!(
            paths.traffic_log_dir(),
            PathBuf::from(r"C:\Temp\moon-bridge\Moon Bridge\logs\traffic-analysis")
        );
    }
}
