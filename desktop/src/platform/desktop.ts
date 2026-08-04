export type CommandError = {
  operation: string;
  stage: string;
  code: string;
  message: string;
  field: string | null;
  retryable: boolean;
  mutationStarted: boolean;
  gatewayLeftRunning: boolean;
  gatewaySnapshot: unknown;
  details?: Record<string, unknown>;
};

type NativeCommandResult<T> = {
  ok: boolean;
  value?: T;
  error?: CommandError;
};

type EventListener<T> = (payload: T) => void;
type Unsubscribe = () => void;

export type PlatformAdapter = {
  command(name: string, args?: unknown): Promise<NativeCommandResult<unknown>>;
  onEvent(name: string, listener: EventListener<unknown>): Unsubscribe | Promise<Unsubscribe>;
  openDialog(options?: unknown): Promise<string | string[] | null>;
  saveDialog(options?: unknown): Promise<string | null>;
  closeWindow(): Promise<void>;
};

type WailsRuntime = {
  EventsOn?: (name: string, callback: (payload: unknown) => void) => Unsubscribe;
  EventsOff?: (name: string) => void;
};

type WailsApp = Record<string, (args?: unknown) => unknown>;

type TauriRuntime = {
  invoke: <T>(name: string, args?: unknown) => Promise<T>;
  listen: <T>(name: string, callback: (event: { payload: T }) => void) => Promise<Unsubscribe>;
};

type WindowWithRuntimes = Window & {
  go?: { main?: { App?: WailsApp } };
  runtime?: WailsRuntime;
  __MOON_BRIDGE_PLATFORM__?: PlatformAdapter;
  __TAURI_INTERNALS__?: unknown;
};

const unsupportedError = (operation: string): CommandError => ({
  operation,
  stage: "platform",
  code: "unsupported_platform",
  message: "Desktop runtime is not available",
  field: null,
  retryable: false,
  mutationStarted: false,
  gatewayLeftRunning: false,
  gatewaySnapshot: null,
});

const invalidResponseError = (operation: string): CommandError => ({
  operation,
  stage: "platform",
  code: "invalid_command_response",
  message: "Desktop command returned an invalid response",
  field: null,
  retryable: false,
  mutationStarted: false,
  gatewayLeftRunning: false,
  gatewaySnapshot: null,
});

const notImplementedError = (operation: string): CommandError => ({
  ...unsupportedError(operation),
  code: "not_implemented",
  message: `${operation} is not implemented for the Wails runtime`,
});

function currentWindow(): WindowWithRuntimes {
  return globalThis as unknown as WindowWithRuntimes;
}

function normalizeCommandError(raw: Partial<CommandError> | undefined, operation: string): CommandError {
  return {
    operation: raw?.operation ?? operation,
    stage: raw?.stage ?? "native",
    code: raw?.code ?? "native_command_failed",
    message: raw?.message ?? "Desktop command failed",
    field: raw?.field ?? null,
    retryable: raw?.retryable ?? false,
    mutationStarted: raw?.mutationStarted ?? false,
    gatewayLeftRunning: raw?.gatewayLeftRunning ?? false,
    gatewaySnapshot: raw?.gatewaySnapshot ?? null,
    ...(raw?.details !== undefined ? { details: raw.details } : {}),
  };
}

function normalizeError(reason: unknown, operation: string): CommandError {
  if (typeof reason === "object" && reason !== null && "code" in reason && "message" in reason) {
    return normalizeCommandError(reason as Partial<CommandError>, operation);
  }
  return normalizeCommandError({ code: "native_command_failed", message: String(reason) }, operation);
}

function wailsAdapter(): PlatformAdapter | null {
  const target = currentWindow();
  const app = target.go?.main?.App;
  const runtime = target.runtime;
  if (!app || typeof app !== "object" || typeof runtime?.EventsOn !== "function") return null;
  return {
    async command(name: string, args?: unknown): Promise<NativeCommandResult<unknown>> {
      const method = app[name];
      if (typeof method !== "function") return { ok: false, error: unsupportedError(name) };
      return method(args) as NativeCommandResult<unknown>;
    },
    onEvent(name: string, listener: EventListener<unknown>): Unsubscribe {
      return runtime.EventsOn!(name, listener);
    },
    async openDialog() {
      throw notImplementedError("openDialog");
    },
    async saveDialog() {
      throw notImplementedError("saveDialog");
    },
    async closeWindow() {
      throw notImplementedError("closeWindow");
    },
  };
}

async function tauriAdapter(): Promise<PlatformAdapter | null> {
  const target = currentWindow();
  if (!target.__TAURI_INTERNALS__) return null;
  const [core, event] = await Promise.all([
    import("@tauri-apps/api/core"),
    import("@tauri-apps/api/event"),
  ]);
  return {
    async command(name: string, args?: unknown): Promise<NativeCommandResult<unknown>> {
      try {
        return { ok: true, value: await core.invoke<unknown>(name, args as never) };
      } catch (reason) {
        return { ok: false, error: normalizeError(reason, name) };
      }
    },
    onEvent(name: string, listener: EventListener<unknown>): Promise<Unsubscribe> {
      return event.listen(name, (received) => listener(received.payload));
    },
    async openDialog(options) {
      const dialog = await import("@tauri-apps/plugin-dialog");
      return dialog.open(options as never) as Promise<string | string[] | null>;
    },
    async saveDialog(options) {
      const dialog = await import("@tauri-apps/plugin-dialog");
      return dialog.save(options as never);
    },
    async closeWindow() {
      const { getCurrentWindow } = await import("@tauri-apps/api/window");
      await getCurrentWindow().close();
    },
  };
}

async function resolveAdapter(): Promise<PlatformAdapter> {
  const target = currentWindow();
  if (target.__MOON_BRIDGE_PLATFORM__) return target.__MOON_BRIDGE_PLATFORM__;
  const wails = wailsAdapter();
  if (wails) return wails;
  const tauri = await tauriAdapter();
  if (tauri) return tauri;
  return {
    async command(name: string): Promise<NativeCommandResult<unknown>> {
      return { ok: false, error: unsupportedError(name) };
    },
    onEvent(_name: string, _listener: EventListener<unknown>): Unsubscribe {
      return () => undefined;
    },
    async openDialog() {
      return null;
    },
    async saveDialog() {
      return null;
    },
    async closeWindow() {
      throw unsupportedError("closeWindow");
    },
  };
}

function isCommandResult(value: unknown): value is NativeCommandResult<unknown> {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as NativeCommandResult<unknown>).ok === "boolean"
  );
}

export async function command<T>(name: string, args?: unknown): Promise<T> {
  const result = await (await resolveAdapter()).command(name, args);
  if (!isCommandResult(result)) throw invalidResponseError(name);
  if (result.ok) return result.value as T;
  if (result.error) throw normalizeCommandError(result.error, name);
  throw invalidResponseError(name);
}

export function onEvent<T>(
  name: string,
  listener: EventListener<T>,
  onError?: (reason: unknown) => void,
): () => void {
  let disposed = false;
  let nativeUnsubscribe: Unsubscribe | undefined;
  void resolveAdapter()
    .then((adapter) => adapter.onEvent(name, listener as EventListener<unknown>))
    .then((unsubscribe) => {
      if (disposed) unsubscribe();
      else nativeUnsubscribe = unsubscribe;
    })
    .catch((reason: unknown) => {
      if (disposed) return;
      if (onError) onError(reason);
      else console.error(`[desktop] failed to register listener for "${name}"`, reason);
    });
  return () => {
    disposed = true;
    nativeUnsubscribe?.();
  };
}

export async function openDialog(options?: unknown): Promise<string | string[] | null> {
  return (await resolveAdapter()).openDialog(options);
}

export async function saveDialog(options?: unknown): Promise<string | null> {
  return (await resolveAdapter()).saveDialog(options);
}

export async function closeWindow(): Promise<void> {
  return (await resolveAdapter()).closeWindow();
}
