// @vitest-environment jsdom
import { renderToStaticMarkup } from "react-dom/server";
import { createRoot } from "react-dom/client";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  command: vi.fn(),
  onEvent: vi.fn<(name: string, listener: (payload: unknown) => void) => () => void>(() => () => undefined),
}));

vi.mock("./platform/desktop", () => ({
  command: mocks.command,
  onEvent: mocks.onEvent,
  closeWindow: vi.fn(async () => undefined),
}));

import { hasRecoveryAvailable, shouldFinishRelay, toExitPrompt, toTrafficStatus, useTrafficAnalysis } from "./hooks/useTrafficAnalysis";
import type { ExitConfirmationPayload } from "./types/trafficAnalysis";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function renderTrafficHook(): () => ReturnType<typeof useTrafficAnalysis> {
  let result: ReturnType<typeof useTrafficAnalysis> | undefined;
  function Harness() {
    result = useTrafficAnalysis();
    return null;
  }
  renderToStaticMarkup(<Harness />);
  return () => result!;
}

async function mountTrafficHook(): Promise<{ get: () => ReturnType<typeof useTrafficAnalysis>; unmount: () => void }> {
  let result: ReturnType<typeof useTrafficAnalysis> | undefined;
  function Harness() {
    result = useTrafficAnalysis();
    return null;
  }
  const container = document.createElement("div");
  const root = createRoot(container);
  await act(async () => {
    root.render(<Harness />);
  });
  return {
    get: () => result!,
    unmount: () => {
      act(() => root.unmount());
    },
  };
}

function exitListener(): (payload: ExitConfirmationPayload | undefined) => void {
  const call = mocks.onEvent.mock.calls.find(([name]) => name === "desktop-exit-confirmation-requested");
  return call?.[1] as (payload: ExitConfirmationPayload | undefined) => void;
}

describe("hasRecoveryAvailable", () => {
  it("returns false when the snapshot carries no recovery object", () => {
    expect(hasRecoveryAvailable(undefined)).toBe(false);
    expect(hasRecoveryAvailable(null)).toBe(false);
  });

  it("returns false for a resolved/inactive phase", () => {
    expect(hasRecoveryAvailable({ phase: "inactive", reconciliationStatus: null, recoveryRequired: false, restoreRequired: false, conflict: false })).toBe(false);
  });

  it("returns true when recoveryRequired is set (reconciliation expected / confirmation)", () => {
    expect(hasRecoveryAvailable({ phase: "reconciliation_required", reconciliationStatus: null, recoveryRequired: true, restoreRequired: true, conflict: false })).toBe(true);
    expect(hasRecoveryAvailable({ phase: "reconciliation_confirmation", reconciliationStatus: null, recoveryRequired: true, restoreRequired: true, conflict: false })).toBe(true);
    expect(hasRecoveryAvailable({ phase: "reconciliation_conflict", reconciliationStatus: "config_conflict", recoveryRequired: true, restoreRequired: true, conflict: true })).toBe(true);
  });

  it("returns true for restart_failed even without recoveryRequired", () => {
    expect(hasRecoveryAvailable({ phase: "restart_failed", reconciliationStatus: null, recoveryRequired: false, restoreRequired: false, conflict: false })).toBe(true);
  });
});

describe("toTrafficStatus", () => {
  it("maps the value.trafficAnalysis field to the Traffic UI status", () => {
    const status = toTrafficStatus({
      trafficAnalysis: {
        mode: "mirror",
        captureState: "capturing",
        relayActive: true,
        integrationActive: true,
        httpRequests: 5,
        sseStreams: 2,
        websocketConnections: 1,
        observationCount: 12,
        observationCapacity: 2000,
        droppedObservations: 3,
        listening: true,
      },
    });

    expect(status.capture.state).toBe("capturing");
    expect(status.capture.httpRequests).toBe(5);
    expect(status.capture.sseStreams).toBe(2);
    expect(status.capture.websocketConnections).toBe(1);
    expect(status.capture.observationCount).toBe(12);
    expect(status.capture.observationCapacity).toBe(2000);
    expect(status.capture.droppedObservations).toBe(3);
    expect(status.relayActive).toBe(true);
    expect(status.integrationActive).toBe(true);
  });

  it("maps a config_conflict recovery snapshot to the recovery UI state (T1)", () => {
    const status = toTrafficStatus({
      trafficAnalysis: { mode: "idle", captureState: "stopped", relayActive: false, integrationActive: false, httpRequests: 0, sseStreams: 0, websocketConnections: 0, observationCount: 0, observationCapacity: 2000, droppedObservations: 0, listening: false },
      recovery: { phase: "reconciliation_conflict", reconciliationStatus: "config_conflict", recoveryRequired: true, restoreRequired: true, conflict: true },
    });

    expect(status.recoveryAvailable).toBe(true);
    expect(status.reconciliationStatus).toBe("config_conflict");
    expect(status.recoveryPhase).toBe("reconciliation_conflict");
  });

  it("keeps recoveryAvailable false for a resolved/inactive snapshot (T2)", () => {
    const resolved = toTrafficStatus({
      trafficAnalysis: { mode: "idle", captureState: "stopped", relayActive: false, integrationActive: false, httpRequests: 0, sseStreams: 0, websocketConnections: 0, observationCount: 0, observationCapacity: 2000, droppedObservations: 0, listening: false },
      recovery: { phase: "inactive", reconciliationStatus: null, recoveryRequired: false, restoreRequired: false, conflict: false },
    });

    expect(resolved.recoveryAvailable).toBe(false);
    expect(resolved.reconciliationStatus).toBeNull();

    const absent = toTrafficStatus({
      trafficAnalysis: { mode: "idle", captureState: "stopped", relayActive: false, integrationActive: false, httpRequests: 0, sseStreams: 0, websocketConnections: 0, observationCount: 0, observationCapacity: 2000, droppedObservations: 0, listening: false },
    });

    expect(absent.recoveryAvailable).toBe(false);
    expect(absent.reconciliationStatus).toBeNull();
  });

  it("detects restart_failed even without recoveryRequired (T3)", () => {
    const status = toTrafficStatus({
      trafficAnalysis: { mode: "idle", captureState: "stopped", relayActive: false, integrationActive: false, httpRequests: 0, sseStreams: 0, websocketConnections: 0, observationCount: 0, observationCapacity: 2000, droppedObservations: 0, listening: false },
      recovery: { phase: "restart_failed", reconciliationStatus: null, recoveryRequired: false, restoreRequired: false, conflict: false },
    });

    expect(status.recoveryAvailable).toBe(true);
  });
});

describe("toExitPrompt", () => {
  it("returns null when no payload is provided", () => {
    expect(toExitPrompt(undefined)).toBeNull();
  });

  it("forwards a trafficActive:false payload so the dialog opens for unsaved observations", () => {
    const prompt = toExitPrompt({ reason: "unsaved_observations", trafficActive: false, unsavedObservations: true });
    expect(prompt).not.toBeNull();
    expect(prompt?.reason).toBe("unsaved_observations");
    expect(prompt?.trafficActive).toBe(false);
  });

  it("forwards a traffic_active payload", () => {
    expect(toExitPrompt({ reason: "traffic_active", trafficActive: true })).not.toBeNull();
  });
});

describe("shouldFinishRelay", () => {
  it("requires relay finalization for traffic_active payloads", () => {
    expect(shouldFinishRelay({ reason: "traffic_active", trafficActive: true })).toBe(true);
  });

  it("does not finalize the relay for unsaved-observation payloads", () => {
    expect(shouldFinishRelay({ reason: "unsaved_observations", trafficActive: false, unsavedObservations: true })).toBe(false);
  });

  it("does not finalize for an unknown reason with traffic inactive", () => {
    expect(shouldFinishRelay({ reason: "some_future_reason", trafficActive: false })).toBe(false);
  });

  it("falls back to trafficActive for payloads without a reason", () => {
    expect(shouldFinishRelay({ trafficActive: true })).toBe(true);
    expect(shouldFinishRelay({ trafficActive: false })).toBe(false);
    expect(shouldFinishRelay(null)).toBe(false);
  });
});

describe("useTrafficAnalysis command dispatch", () => {
  beforeEach(() => {
    mocks.command.mockReset();
    mocks.command.mockResolvedValue({});
  });

  it("sends no wrapped input and no operationId for StartTrafficAnalysis", async () => {
    const getTraffic = renderTrafficHook();

    await getTraffic().start();

    expect(mocks.command).toHaveBeenCalledWith("StartTrafficAnalysis", {});
  });

  it("passes FinishTrafficRelay as a flat struct without an operationId", async () => {
    const getTraffic = renderTrafficHook();

    await getTraffic().finishRelay(true);

    expect(mocks.command).toHaveBeenCalledWith("FinishTrafficRelay", { discardUnsaved: true });
  });
});

describe("finishRelayResolvingConflict (Plan 4U)", () => {
  const conflictStatus = {
    trafficAnalysis: { mode: "desktop_managed", captureState: "passthrough", relayActive: true, integrationActive: true, httpRequests: 0, sseStreams: 0, websocketConnections: 0, observationCount: 0, observationCapacity: 2000, droppedObservations: 0, listening: true },
    recovery: { phase: "reconciliation_conflict", reconciliationStatus: "config_conflict", recoveryRequired: true, restoreRequired: true, conflict: true },
  };
  const resolvedStatus = {
    trafficAnalysis: { mode: "desktop_managed", captureState: "passthrough", relayActive: true, integrationActive: true, httpRequests: 0, sseStreams: 0, websocketConnections: 0, observationCount: 0, observationCapacity: 2000, droppedObservations: 0, listening: true },
    recovery: { phase: "inactive", reconciliationStatus: null, recoveryRequired: false, restoreRequired: false, conflict: false },
  };

  beforeEach(() => {
    mocks.command.mockReset();
    mocks.command.mockResolvedValue({});
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("restores a known conflict then finishes the relay in one step when confirmed", async () => {
    mocks.command.mockImplementation((method: string) =>
      method === "TrafficAnalysisStatus" ? Promise.resolve(conflictStatus) : Promise.resolve({}),
    );
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      await get().finishRelayResolvingConflict();
    });

    expect(confirm).toHaveBeenCalledWith("Codex設定に競合があります。分析開始前の設定へ復元して終了しますか？");
    const calls = mocks.command.mock.calls.map(([method]) => String(method));
    expect(calls.indexOf("RestoreRecovery")).toBeGreaterThanOrEqual(0);
    expect(calls.indexOf("RestoreRecovery")).toBeLessThan(calls.indexOf("FinishTrafficRelay"));
    expect(mocks.command).toHaveBeenCalledWith("RestoreRecovery", { confirmConflict: true });
    expect(mocks.command).toHaveBeenCalledWith("FinishTrafficRelay", { discardUnsaved: false });
    unmount();
  });

  it("does nothing when the conflict confirmation is declined", async () => {
    mocks.command.mockImplementation((method: string) =>
      method === "TrafficAnalysisStatus" ? Promise.resolve(conflictStatus) : Promise.resolve({}),
    );
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      await get().finishRelayResolvingConflict();
    });

    expect(confirm).toHaveBeenCalled();
    expect(mocks.command).not.toHaveBeenCalledWith("RestoreRecovery", expect.anything());
    expect(mocks.command).not.toHaveBeenCalledWith("FinishTrafficRelay", expect.anything());
    unmount();
  });

  it("keeps the relay alive when the restore fails", async () => {
    mocks.command.mockImplementation((method: string) => {
      if (method === "TrafficAnalysisStatus") return Promise.resolve(conflictStatus);
      if (method === "RestoreRecovery") return Promise.reject({ code: "recovery_config_conflict", message: "confirmation is required" });
      return Promise.resolve({});
    });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      await get().finishRelayResolvingConflict();
    });

    expect(get().error?.code).toBe("recovery_config_conflict");
    expect(mocks.command).not.toHaveBeenCalledWith("FinishTrafficRelay", expect.anything());
    unmount();
  });

  it("keeps the restored-and-relaying state retryable after a finish failure, and skips restore on the next call", async () => {
    let reconciled = false;
    let finishAttempts = 0;
    mocks.command.mockImplementation((method: string) => {
      if (method === "TrafficAnalysisStatus") {
        return Promise.resolve(reconciled ? resolvedStatus : conflictStatus);
      }
      if (method === "RestoreRecovery") {
        reconciled = true;
        return Promise.resolve({});
      }
      if (method === "FinishTrafficRelay") {
        finishAttempts += 1;
        return Promise.reject({ code: "finish_precondition", message: "capture relay is not finishable" });
      }
      return Promise.resolve({});
    });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      await get().finishRelayResolvingConflict();
    });
    // Restore succeeded, relay stays alive, finish failed: retryable state.
    expect(get().status?.reconciliationStatus).not.toBe("config_conflict");
    expect(get().status?.relayActive).toBe(true);
    expect(get().error?.code).toBe("finish_precondition");
    expect(mocks.command).toHaveBeenCalledWith("RestoreRecovery", { confirmConflict: true });

    await act(async () => {
      await get().finishRelayResolvingConflict();
    });
    // Conflict already resolved: restore must not run again, only finish retries.
    expect(finishAttempts).toBe(2);
    const restoreCalls = mocks.command.mock.calls.filter(([method]) => String(method) === "RestoreRecovery");
    expect(restoreCalls).toHaveLength(1);
    unmount();
  });

  it("finishes directly without confirmation when no conflict is known", async () => {
    mocks.command.mockImplementation((method: string) =>
      method === "TrafficAnalysisStatus" ? Promise.resolve(resolvedStatus) : Promise.resolve({}),
    );
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      await get().finishRelayResolvingConflict();
    });

    expect(confirm).not.toHaveBeenCalled();
    expect(mocks.command).not.toHaveBeenCalledWith("RestoreRecovery", expect.anything());
    expect(mocks.command).toHaveBeenCalledWith("FinishTrafficRelay", { discardUnsaved: false });
    unmount();
  });
});

describe("useTrafficAnalysis removed manual export actions", () => {
  it("no longer exposes exportObservations, revealExport, or lastExport", async () => {
    const getTraffic = renderTrafficHook();
    expect("exportObservations" in getTraffic()).toBe(false);
    expect("revealExport" in getTraffic()).toBe(false);
    expect("lastExport" in getTraffic()).toBe(false);
  });
});

describe("desktop-exit-confirmation-requested subscription", () => {
  beforeEach(() => {
    mocks.command.mockReset();
    mocks.onEvent.mockReset();
    mocks.command.mockResolvedValue({});
  });

  it("opens the exit prompt even when trafficActive is false", async () => {
    const { get, unmount } = await mountTrafficHook();
    const listener = exitListener();

    expect(listener).toBeTypeOf("function");
    await act(async () => {
      listener({ reason: "unsaved_observations", trafficActive: false, unsavedObservations: true });
    });

    expect(get().exitPrompt).toEqual({ reason: "unsaved_observations", trafficActive: false, unsavedObservations: true });
    unmount();
  });

  it("clears the exit prompt via CancelExit", async () => {
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      exitListener()({ reason: "recovery_required", trafficActive: false, recoveryRequired: true });
    });
    expect(get().exitPrompt).not.toBeNull();

    await act(async () => {
      await get().cancelExit();
    });

    expect(get().exitPrompt).toBeNull();
    expect(mocks.command).toHaveBeenCalledWith("CancelExit");
    unmount();
  });

  it("goes straight to ConfirmExit for unsaved observations without stopping or finalizing the relay", async () => {
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      exitListener()({ reason: "unsaved_observations", trafficActive: false, unsavedObservations: true });
    });
    await act(async () => {
      await get().confirmExit(true);
    });

    expect(mocks.command).toHaveBeenCalledWith("ConfirmExit", { confirm: true, discardUnsaved: true });
    expect(mocks.command).not.toHaveBeenCalledWith("StopTrafficAnalysis", {});
    expect(mocks.command).not.toHaveBeenCalledWith("FinishTrafficRelay", expect.anything());
    unmount();
  });

  it("finalizes the relay before ConfirmExit for a traffic_active payload", async () => {
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      exitListener()({ reason: "traffic_active", trafficActive: true });
    });
    await act(async () => {
      await get().confirmExit();
    });

    expect(mocks.command).toHaveBeenCalledWith("FinishTrafficRelay", { discardUnsaved: false });
    expect(mocks.command).toHaveBeenCalledWith("ConfirmExit", { confirm: true, discardUnsaved: false });
    unmount();
  });

  it("resolves a live config conflict during exit via RestoreRecovery before finalizing (Plan 4t)", async () => {
    mocks.command.mockImplementation((method: string) => {
      if (method === "TrafficAnalysisStatus") {
        return Promise.resolve({
          trafficAnalysis: { mode: "desktop_managed", captureState: "passthrough", relayActive: true, integrationActive: true, httpRequests: 0, sseStreams: 0, websocketConnections: 0, observationCount: 0, observationCapacity: 2000, droppedObservations: 0, listening: true },
          recovery: { phase: "reconciliation_required", reconciliationStatus: "config_conflict", recoveryRequired: true, restoreRequired: true, conflict: true },
        });
      }
      return Promise.resolve({});
    });
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      exitListener()({ reason: "recovery_required", trafficActive: true, recoveryRequired: true });
    });
    await act(async () => {
      await get().confirmExit();
    });

    expect(mocks.command).toHaveBeenCalledWith("RestoreRecovery", { confirmConflict: true });
    const calls = mocks.command.mock.calls.map(([method]) => String(method));
    expect(calls.indexOf("RestoreRecovery")).toBeGreaterThanOrEqual(0);
    expect(calls.indexOf("RestoreRecovery")).toBeLessThan(calls.indexOf("FinishTrafficRelay"));
    expect(calls.indexOf("FinishTrafficRelay")).toBeLessThan(calls.indexOf("ConfirmExit"));
    expect(mocks.command).not.toHaveBeenCalledWith("StopTrafficAnalysis", {});
    unmount();
  });

  it("resolves a startup reconciliation exit via RestoreRecovery without finalizing the relay", async () => {
    mocks.command.mockImplementation((method: string) => {
      if (method === "TrafficAnalysisStatus") {
        return Promise.resolve({
          trafficAnalysis: { mode: "capture_only", captureState: "passthrough", relayActive: true, integrationActive: false, httpRequests: 0, sseStreams: 0, websocketConnections: 0, observationCount: 0, observationCapacity: 2000, droppedObservations: 0, listening: true },
          recovery: { phase: "reconciliation_required", reconciliationStatus: "pending_restore", recoveryRequired: true, restoreRequired: true, conflict: false },
        });
      }
      return Promise.resolve({});
    });
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      exitListener()({ reason: "recovery_required", trafficActive: false, recoveryRequired: true });
    });
    await act(async () => {
      await get().confirmExit();
    });

    expect(mocks.command).toHaveBeenCalledWith("RestoreRecovery", { confirmConflict: false });
    expect(mocks.command).toHaveBeenCalledWith("ConfirmExit", { confirm: true, discardUnsaved: false });
    expect(mocks.command).not.toHaveBeenCalledWith("FinishTrafficRelay", expect.anything());
    unmount();
  });

  it("keeps the exit prompt open when the restore fails during exit", async () => {
    mocks.command.mockImplementation((method: string) => {
      if (method === "TrafficAnalysisStatus") {
        return Promise.resolve({
          trafficAnalysis: { mode: "desktop_managed", captureState: "passthrough", relayActive: true, integrationActive: true, httpRequests: 0, sseStreams: 0, websocketConnections: 0, observationCount: 0, observationCapacity: 2000, droppedObservations: 0, listening: true },
          recovery: { phase: "reconciliation_required", reconciliationStatus: "config_conflict", recoveryRequired: true, restoreRequired: true, conflict: true },
        });
      }
      if (method === "RestoreRecovery") {
        return Promise.reject({ code: "recovery_config_conflict", message: "confirmation is required" });
      }
      return Promise.resolve({});
    });
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      exitListener()({ reason: "recovery_required", trafficActive: true, recoveryRequired: true });
    });
    await act(async () => {
      await get().confirmExit();
    });

    expect(get().exitPrompt).not.toBeNull();
    expect(get().error?.code).toBe("recovery_config_conflict");
    expect(mocks.command).not.toHaveBeenCalledWith("FinishTrafficRelay", expect.anything());
    expect(mocks.command).not.toHaveBeenCalledWith("ConfirmExit", expect.anything());
    unmount();
  });

  it("goes straight to ConfirmExit for a gateway_active payload without stopping or finalizing", async () => {
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      exitListener()({ reason: "gateway_active", trafficActive: false, gatewayActive: true });
    });
    await act(async () => {
      await get().confirmExit();
    });

    expect(mocks.command).toHaveBeenCalledWith("ConfirmExit", { confirm: true, discardUnsaved: false });
    expect(mocks.command).not.toHaveBeenCalledWith("StopTrafficAnalysis", {});
    expect(mocks.command).not.toHaveBeenCalledWith("FinishTrafficRelay", expect.anything());
    unmount();
  });

  it("keeps the exit prompt open when ConfirmExit fails", async () => {
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      exitListener()({ reason: "unsaved_observations", trafficActive: false, unsavedObservations: true });
    });
    mocks.command.mockRejectedValue({ code: "exit_confirmation_required", message: "state mismatch" });
    await act(async () => {
      await get().confirmExit(true);
    });

    expect(get().exitPrompt).not.toBeNull();
    expect(get().error?.code).toBe("exit_confirmation_required");
    unmount();
  });

  it("keeps the exit prompt open when CancelExit fails", async () => {
    const { get, unmount } = await mountTrafficHook();

    await act(async () => {
      exitListener()({ reason: "recovery_required", trafficActive: false, recoveryRequired: true });
    });
    mocks.command.mockRejectedValue({ code: "exit_confirmation_required", message: "state mismatch" });
    await act(async () => {
      await get().cancelExit();
    });

    expect(get().exitPrompt).not.toBeNull();
    unmount();
  });
});
