import type { TrafficEventCode } from "../types/trafficAnalysis";

export type RuntimeLogLocale = "ja" | "en";

const messages = {
  ja: {
    title: "ログ",
    empty: "まだログはありません。",
    copy: "コピー",
    copied: "コピーしました",
    copyFailed: "コピーできませんでした",
    lines: (count: number) => `${count} 行`,
    event: {
      traffic_backup_created: "バックアップを作成しました",
      traffic_route_applied: "Codexの接続先を分析用に切り替えました",
      traffic_analysis_started: "分析を開始しました",
      traffic_backup_removed: "バックアップを安全に削除しました",
      traffic_route_restored: "Codexの接続先を元に戻しました",
      traffic_analysis_stopped: "分析を停止しました",
      traffic_backup_create_failed: "バックアップを作成できませんでした",
      traffic_restore_failed: "設定を元に戻せませんでした",
      traffic_cleanup_pending: "バックアップの削除を再試行する必要があります",
      traffic_recovery_required: "復旧操作が必要です",
    },
  },
  en: {
    title: "Log",
    empty: "No logs yet.",
    copy: "Copy",
    copied: "Copied",
    copyFailed: "Copy failed",
    lines: (count: number) => `${count} lines`,
    event: {
      traffic_backup_created: "Created a backup",
      traffic_route_applied: "Switched Codex to the analysis endpoint",
      traffic_analysis_started: "Started analysis",
      traffic_backup_removed: "Safely removed the backup",
      traffic_route_restored: "Restored the original Codex endpoint",
      traffic_analysis_stopped: "Stopped analysis",
      traffic_backup_create_failed: "Could not create a backup",
      traffic_restore_failed: "Could not restore the Codex configuration",
      traffic_cleanup_pending: "Backup cleanup must be retried",
      traffic_recovery_required: "Recovery action is required",
    },
  },
} as const;

export function resolveRuntimeLogLocale(input?: string): RuntimeLogLocale {
  const raw = input ?? (typeof document !== "undefined" ? document.documentElement.lang : "ja");
  return raw.toLowerCase().startsWith("en") ? "en" : "ja";
}

export function runtimeLogMessages(locale?: string) {
  return messages[resolveRuntimeLogLocale(locale)];
}

export function runtimeLogEventMessage(code: TrafficEventCode, locale?: string): string {
  return runtimeLogMessages(locale).event[code];
}
