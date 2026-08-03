import type { TrafficOperation } from "./types/trafficAnalysis";

export function trafficActionDisabled(
  observations: number,
  pending: Partial<Record<TrafficOperation, boolean>>,
) {
  return {
    start: pending.starting === true,
    restart: pending.restarting === true,
    stop: pending.stopping === true,
    export: pending.exporting === true || observations === 0,
    clear: pending.clearing === true || observations === 0,
    restore: pending.restoring === true,
    reveal: pending.revealing === true,
    finalize:
      pending.stopping === true ||
      pending.finalizing === true,
  };
}
