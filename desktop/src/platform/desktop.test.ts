import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { closeWindow, command, onEvent, openDialog, saveDialog, type PlatformAdapter } from "./desktop";

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
    delete (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
    removeWailsRuntime();
  });

  afterEach(() => {
    removeWailsRuntime();
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

  it("resolves undefined when ok is true without a value", async () => {
    installPlatform(stubAdapter());

    await expect(command("VoidOp")).resolves.toBeUndefined();
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

  it("rejects with not_implemented for Wails dialogs and closeWindow", async () => {
    installWailsRuntime({ main: { App: {} } }, { EventsOn: () => () => undefined });

    await expect(openDialog()).rejects.toMatchObject({ code: "not_implemented" });
    await expect(saveDialog()).rejects.toMatchObject({ code: "not_implemented" });
    await expect(closeWindow()).rejects.toMatchObject({ code: "not_implemented" });
  });

  it("uses the Wails adapter and calls the bound method with args", async () => {
    const RoundTrip = vi.fn().mockResolvedValue({ ok: true, value: { payload: "hello" } });
    installWailsRuntime({ main: { App: { RoundTrip } } }, { EventsOn: () => () => undefined });

    const result = await command<{ payload: string }>("RoundTrip", { payload: "hello" });

    expect(RoundTrip).toHaveBeenCalledWith({ payload: "hello" });
    expect(result).toEqual({ payload: "hello" });
  });

  it("rejects with unsupported_platform when a Wails command method is missing", async () => {
    installWailsRuntime({ main: { App: {} } }, { EventsOn: () => () => undefined });

    await expect(command("Missing")).rejects.toMatchObject({ code: "unsupported_platform" });
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

  it("falls back to the Tauri adapter when __TAURI_INTERNALS__ is present", async () => {
    vi.mock("@tauri-apps/api/core", () => ({
      invoke: vi.fn().mockResolvedValue({ payload: "hello" }),
    }));
    vi.mock("@tauri-apps/api/event", () => ({
      listen: vi.fn().mockResolvedValue(() => undefined),
    }));
    Object.defineProperty(globalThis, "__TAURI_INTERNALS__", { configurable: true, value: {} });
    const { invoke } = await import("@tauri-apps/api/core");

    const result = await command<{ payload: string }>("RoundTrip", { payload: "hello" });

    expect(invoke).toHaveBeenCalledWith("RoundTrip", { payload: "hello" });
    expect(result).toEqual({ payload: "hello" });
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
