import type { TrafficOperation } from "./types/trafficAnalysis";

export function trafficActionDisabled(
  observations: number,
  pending: Partial<Record<TrafficOperation, boolean>>,
) {
  return {
    start: pending.starting === true,
    restart: pending.restarting === true,
    stop: pending.stopping === true,
    clear: pending.clearing === true || observations === 0,
    restore: pending.restoring === true,
    finalize:
      pending.stopping === true ||
      pending.finalizing === true,
  };
}
