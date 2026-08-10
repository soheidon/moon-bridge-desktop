import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  ZERO_ARG_COMMANDS,
  closeWindow,
  command,
  onEvent,
  openDialog,
  saveDialog,
  type PlatformAdapter,
} from "./desktop";

function installPlatform(adapter: PlatformAdapter) {
  Object.defineProperty(globalThis, "__MOON_BRIDGE_PLATFORM__", { configurable: true, value: adapter });
}

function stubAdapter(overrides: Partial<PlatformAdapter> = {}): PlatformAdapter {
  return {
    command: async () => ({ ok: true }),
    onEvent: () => () => undefined,
    openDialog: async () => null,
    saveDialog: async () => null,
    closeWindow: async () => undefined,
    ...overrides,
  };
}

function installWailsRuntime(app: unknown, runtime: unknown) {
  Object.defineProperty(globalThis, "go", { configurable: true, value: app });
  Object.defineProperty(globalThis, "runtime", { configurable: true, value: runtime });
}

function removeWailsRuntime() {
  delete (globalThis as { go?: unknown }).go;
  delete (globalThis as { runtime?: unknown }).runtime;
}

describe("desktop platform", () => {
  beforeEach(() => {
    delete (globalThis as { __MOON_BRIDGE_PLATFORM__?: unknown }).__MOON_BRIDGE_PLATFORM__;
    removeWailsRuntime();
  });

  afterEach(() => {
    removeWailsRuntime();
    delete (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
  });

  it("unwraps a successful command result", async () => {
    installPlatform(
      stubAdapter({
        command: async () => ({ ok: true, value: { payload: "hello" } }),
      }),
    );

    await expect(command<{ payload: string }>("RoundTrip", { payload: "hello" })).resolves.toEqual({
      payload: "hello",
    });
  });

  it("unwraps the flat GatewaySnapshot value from GatewayStatus", async () => {
    const value = {
      state: "running",
      address: "127.0.0.1:38440",
      configPath: "C:/gateway",
      pid: 25596,
      instanceId: "inst-1",
      error: null,
    };
    installPlatform(
      stubAdapter({
        command: async () => ({ ok: true, value }),
      }),
    );

    await expect(command("GatewayStatus")).resolves.toEqual(value);
  });

  it("rejects ok without a value as a contract violation", async () => {
    installPlatform(stubAdapter());

    await expect(command("VoidOp")).rejects.toMatchObject({ code: "invalid_command_response" });
  });

  it("preserves structured command errors", async () => {
    const error = {
      operation: "RoundTrip",
      stage: "validation",
      code: "invalid_payload",
      message: "payload must not be empty",
      field: null,
      retryable: false,
      mutationStarted: false,
      gatewayLeftRunning: false,
      gatewaySnapshot: null,
    };
    installPlatform(
      stubAdapter({
        command: async () => ({ ok: false, error }),
      }),
    );

    await expect(command("RoundTrip", "")).rejects.toEqual(error);
  });

  it.each([
    ["null envelope", null],
    ["string envelope", "oops"],
    ["non-boolean ok", { ok: "yes" }],
    ["ok=false without error", { ok: false }],
  ])("rejects malformed response — %s — with invalid_command_response", async (_label, envelope) => {
    installPlatform(
      stubAdapter({
        command: async () => envelope as never,
      }),
    );

    await expect(command("RoundTrip")).rejects.toMatchObject({ code: "invalid_command_response" });
  });

  it("rejects with unsupported_platform when no runtime is available", async () => {
    await expect(command("RoundTrip")).rejects.toMatchObject({ code: "unsupported_platform" });
  });

  it("rejects with not_implemented for Wails openDialog and closeWindow", async () => {
    installWailsRuntime({ main: { App: {} } }, { EventsOn: () => () => undefined });

    await expect(openDialog()).rejects.toMatchObject({ code: "not_implemented" });
    await expect(closeWindow()).rejects.toMatchObject({ code: "not_implemented" });
  });

  describe("saveDialog over the Wails SaveFileDialog binding", () => {
    it("maps a bare defaultPath to DefaultFilename and unwraps the chosen path", async () => {
      const SaveFileDialog = vi.fn().mockResolvedValue({
        ok: true,
        value: { saveDialog: { path: "C:/logs/export.log", canceled: false } },
      });
      installWailsRuntime({ main: { App: { SaveFileDialog } } }, { EventsOn: () => () => undefined });

      const path = await saveDialog({
        title: "ログを保存",
        defaultPath: "traffic-analysis-export.log",
        filters: [{ name: "Log", extensions: ["log"] }],
      });

      expect(path).toBe("C:/logs/export.log");
      expect(SaveFileDialog).toHaveBeenCalledWith({
        title: "ログを保存",
        defaultDirectory: "",
        defaultFilename: "traffic-analysis-export.log",
        filters: [{ displayName: "Log", pattern: "*.log" }],
      });
    });

    it("splits a full defaultPath into directory and filename", async () => {
      const SaveFileDialog = vi.fn().mockResolvedValue({
        ok: true,
        value: { saveDialog: { path: "C:/logs/export.log", canceled: false } },
      });
      installWailsRuntime({ main: { App: { SaveFileDialog } } }, { EventsOn: () => () => undefined });

      await saveDialog({ defaultPath: "C:\\Users\\test\\Desktop\\export.log" });

      expect(SaveFileDialog).toHaveBeenCalledWith({
        title: "",
        defaultDirectory: "C:\\Users\\test\\Desktop",
        defaultFilename: "export.log",
        filters: [],
      });
    });

    it("joins multiple extensions into a single Wails pattern", async () => {
      const SaveFileDialog = vi.fn().mockResolvedValue({
        ok: true,
        value: { saveDialog: { path: "out.log", canceled: false } },
      });
      installWailsRuntime({ main: { App: { SaveFileDialog } } }, { EventsOn: () => () => undefined });

      await saveDialog({ filters: [{ name: "Logs", extensions: ["log", "txt"] }] });

      expect(SaveFileDialog).toHaveBeenCalledWith(
        expect.objectContaining({ filters: [{ displayName: "Logs", pattern: "*.log;*.txt" }] }),
      );
    });

    it("returns null when the user cancels", async () => {
      const SaveFileDialog = vi.fn().mockResolvedValue({
        ok: true,
        value: { saveDialog: { path: "", canceled: true } },
      });
      installWailsRuntime({ main: { App: { SaveFileDialog } } }, { EventsOn: () => () => undefined });

      await expect(saveDialog({ defaultPath: "export.log" })).resolves.toBeNull();
    });

    it("returns null when the response lacks a saveDialog value", async () => {
      const SaveFileDialog = vi.fn().mockResolvedValue({ ok: true, value: {} });
      installWailsRuntime({ main: { App: { SaveFileDialog } } }, { EventsOn: () => () => undefined });

      await expect(saveDialog()).resolves.toBeNull();
    });

    it("returns null instead of throwing when the binding is missing", async () => {
      installWailsRuntime({ main: { App: {} } }, { EventsOn: () => () => undefined });

      await expect(saveDialog()).resolves.toBeNull();
    });

    it("returns null when the binding reports an error", async () => {
      const SaveFileDialog = vi.fn().mockResolvedValue({ ok: false, error: { code: "save_dialog_failed" } });
      installWailsRuntime({ main: { App: { SaveFileDialog } } }, { EventsOn: () => () => undefined });

      await expect(saveDialog()).resolves.toBeNull();
    });
  });

  it("uses the Wails adapter and calls the bound method with args", async () => {
    const RoundTrip = vi.fn().mockResolvedValue({ ok: true, value: { payload: "hello" } });
    installWailsRuntime({ main: { App: { RoundTrip } } }, { EventsOn: () => () => undefined });

    const result = await command<{ payload: string }>("RoundTrip", { payload: "hello" });

    expect(RoundTrip).toHaveBeenCalledWith({ payload: "hello" });
    expect(result).toEqual({ payload: "hello" });
  });

  it.each([...ZERO_ARG_COMMANDS])(
    "invokes zero-argument binding %s with no arguments",
    async (name) => {
      const method = vi.fn().mockResolvedValue({ ok: true, value: {} });
      installWailsRuntime({ main: { App: { [name]: method } } }, { EventsOn: () => () => undefined });

      await command(name);

      expect(method).toHaveBeenCalledOnce();
      expect(method.mock.calls[0]).toHaveLength(0);
    },
  );

  it("invokes a zero-argument method with no arguments even when args are supplied", async () => {
    const StartTrafficAnalysis = vi.fn().mockResolvedValue({ ok: true, value: {} });
    installWailsRuntime({ main: { App: { StartTrafficAnalysis } } }, { EventsOn: () => () => undefined });

    await command("StartTrafficAnalysis", { input: { operationId: "op-1" } });

    expect(StartTrafficAnalysis.mock.calls[0]).toHaveLength(0);
  });

  it("passes a flat argument to a one-argument Wails method without wrapping it", async () => {
    const FinishTrafficRelay = vi.fn().mockResolvedValue({ ok: true, value: {} });
    installWailsRuntime({ main: { App: { FinishTrafficRelay } } }, { EventsOn: () => () => undefined });

    await command("FinishTrafficRelay", { discardUnsaved: true });

    expect(FinishTrafficRelay).toHaveBeenCalledWith({ discardUnsaved: true });
  });

  it("rejects with unsupported_platform when a Wails command method is missing", async () => {
    installWailsRuntime({ main: { App: {} } }, { EventsOn: () => () => undefined });

    await expect(command("Missing")).rejects.toMatchObject({ code: "unsupported_platform" });
  });

  it("preserves Wails business errors without retrying through another runtime", async () => {
    const error = {
      operation: "RestoreRecovery",
      stage: "validation",
      code: "recovery_confirmation_required",
      message: "Confirmation is required",
      field: null,
      retryable: false,
      mutationStarted: false,
      gatewayLeftRunning: false,
      gatewaySnapshot: null,
    };
    const RestoreRecovery = vi.fn().mockResolvedValue({ ok: false, error });
    installWailsRuntime({ main: { App: { RestoreRecovery } } }, { EventsOn: () => () => undefined });
    Object.defineProperty(globalThis, "__TAURI_INTERNALS__", { configurable: true, value: {} });

    await expect(command("RestoreRecovery", { confirm: false })).rejects.toEqual(error);
    expect(RestoreRecovery).toHaveBeenCalledOnce();
  });

  it("normalizes Wails transport rejection without a fallback dispatch", async () => {
    const RoundTrip = vi.fn().mockRejectedValue(new Error("transport sentinel"));
    installWailsRuntime({ main: { App: { RoundTrip } } }, { EventsOn: () => () => undefined });

    await expect(command("RoundTrip")).rejects.toMatchObject({
      code: "native_command_failed",
      message: "Desktop command failed",
    });
    expect(RoundTrip).toHaveBeenCalledOnce();
  });

  it("uses runtime.EventsOn unsubscribe for Wails events", async () => {
    const unsubscribe = vi.fn();
    const EventsOn = vi.fn().mockReturnValue(unsubscribe);
    installWailsRuntime({ main: { App: { RoundTrip: vi.fn() } } }, { EventsOn });

    const remove = onEvent("desktop:roundtrip", () => undefined);
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(EventsOn).toHaveBeenCalledWith("desktop:roundtrip", expect.any(Function));
    remove();
    expect(unsubscribe).toHaveBeenCalledOnce();
  });

  it("passes event registration failures to onError without an unhandled rejection", async () => {
    const reason = new Error("registration failed");
    installPlatform(
      stubAdapter({
        onEvent: () => Promise.reject(reason),
      }),
    );
    const onError = vi.fn();

    onEvent("desktop:roundtrip", () => undefined, onError);
    await new Promise((resolve) => setTimeout(resolve, 0));
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(onError).toHaveBeenCalledWith(reason);
  });

  it("keeps same-event listeners independent", async () => {
    const listeners = new Set<(payload: string) => void>();
    installPlatform(
      stubAdapter({
        onEvent: (_name, listener: (payload: unknown) => void) => {
          const typedListener = listener as (payload: string) => void;
          listeners.add(typedListener);
          return () => {
            listeners.delete(typedListener);
          };
        },
      }),
    );
    const first = vi.fn();
    const second = vi.fn();
    const removeFirst = onEvent("desktop:roundtrip", first);
    const removeSecond = onEvent("desktop:roundtrip", second);
    await new Promise((resolve) => setTimeout(resolve, 0));
    listeners.forEach((listener) => listener("one"));
    removeFirst();
    listeners.forEach((listener) => listener("two"));
    removeSecond();

    expect(first).toHaveBeenCalledOnce();
    expect(second).toHaveBeenCalledWith("one");
    expect(second).toHaveBeenCalledWith("two");
    expect(listeners).toHaveLength(0);
  });

  it("unsubscribes when disposed before async registration resolves", async () => {
    let resolveRegistration: ((value: () => void) => void) | undefined;
    const registration = new Promise<() => void>((resolve) => {
      resolveRegistration = resolve;
    });
    installPlatform(
      stubAdapter({
        onEvent: () => registration,
      }),
    );
    const remove = onEvent("desktop:roundtrip", () => undefined);
    remove();
    const nativeUnsubscribe = vi.fn();
    resolveRegistration?.(nativeUnsubscribe);
    await new Promise((resolve) => setTimeout(resolve, 0));
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(nativeUnsubscribe).toHaveBeenCalledOnce();
  });
});
