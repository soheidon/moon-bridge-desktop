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
  getWindowSize?: () => Promise<{ w: number; h: number } | null>;
  setWindowSize?: (width: number, height: number) => Promise<void>;
};

// Wails binds Go methods with a fixed arity and rejects calls that pass extra
// arguments — even `undefined` — with "received N arguments ... expected 0".
// These generated bindings take no arguments and must be invoked as method(),
// never method(args). Kept in sync with desktop/wailsjs/go/main/App.
export const ZERO_ARG_COMMANDS: ReadonlySet<string> = new Set([
  "CancelExit",
  "CodexConfigBackups",
  "CodexStatus",
  "GatewayStatus",
  "LoadCodexConfig",
  "LoadDeepSeekSettings",
  "LoadRoutingProfiles",
  "RecoveryStatus",
  "RefreshRecoveryStatus",
  "StartTrafficAnalysis",
  "StopTrafficAnalysis",
  "TrafficAnalysisObservations",
  "TrafficAnalysisOpenLogFolder",
  "TrafficAnalysisOpenLogFile",
  "TrafficAnalysisStatus",
]);

type WailsRuntime = {
  EventsOn?: (name: string, callback: (payload: unknown) => void) => Unsubscribe;
  EventsOff?: (name: string) => void;
  WindowGetSize?: () => Promise<{ w: number; h: number }>;
  WindowSetSize?: (width: number, height: number) => void;
};

type WailsApp = Record<string, (args?: unknown) => unknown>;

type WindowWithRuntimes = Window & {
  go?: { main?: { App?: WailsApp } };
  runtime?: WailsRuntime;
  __MOON_BRIDGE_PLATFORM__?: PlatformAdapter;
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
  // Do not expose arbitrary native/transport error text to the frontend. It
  // may contain paths, URLs, tokens, or implementation details.
  return normalizeCommandError(
    { code: "native_command_failed", message: "Desktop command failed" },
    operation,
  );
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
      try {
        // Zero-argument bindings must be called with no arguments at all;
        // passing undefined/null/{} makes Wails reject the call.
        const result = ZERO_ARG_COMMANDS.has(name) ? method() : method(args);
        return (await Promise.resolve(result)) as NativeCommandResult<unknown>;
      } catch (reason) {
        return { ok: false, error: normalizeError(reason, name) };
      }
    },
    onEvent(name: string, listener: EventListener<unknown>): Unsubscribe {
      return runtime.EventsOn!(name, listener);
    },
    async openDialog() {
      throw notImplementedError("openDialog");
    },
    async saveDialog(options?: unknown): Promise<string | null> {
      const method = app.SaveFileDialog;
      if (typeof method !== "function") return null;
      const opts = (options ?? {}) as {
        title?: string;
        defaultPath?: string;
        filters?: Array<{ name?: string; extensions?: string[] }>;
      };
      // defaultPath may be a bare filename (the export flow) or a full path;
      // map the former to DefaultFilename and split the latter into directory
      // + filename. Tauri-style filters become Wails displayName/pattern pairs.
      let defaultDirectory = "";
      let defaultFilename = opts.defaultPath ?? "";
      if (opts.defaultPath) {
        const lastSep = Math.max(opts.defaultPath.lastIndexOf("/"), opts.defaultPath.lastIndexOf("\\"));
        if (lastSep >= 0) {
          defaultDirectory = opts.defaultPath.slice(0, lastSep);
          defaultFilename = opts.defaultPath.slice(lastSep + 1);
        }
      }
      const filters = (opts.filters ?? []).map((filter) => ({
        displayName: filter.name ?? "",
        pattern: (filter.extensions ?? []).map((ext) => `*.${ext}`).join(";"),
      }));
      const result = await Promise.resolve(
        method({ title: opts.title ?? "", defaultDirectory, defaultFilename, filters }),
      );
      if (isCommandResult(result) && result.ok && result.value !== undefined) {
        const saved = (result.value as { saveDialog?: { path?: string; canceled?: boolean } }).saveDialog;
        if (!saved || saved.canceled) return null;
        return saved.path ?? null;
      }
      return null;
    },
    async closeWindow() {
      throw notImplementedError("closeWindow");
    },
    async getWindowSize() {
      if (typeof runtime.WindowGetSize !== "function") return null;
      return runtime.WindowGetSize();
    },
    async setWindowSize(width: number, height: number) {
      if (typeof runtime.WindowSetSize !== "function") throw notImplementedError("setWindowSize");
      runtime.WindowSetSize(width, height);
    },
  };
}

async function resolveAdapter(): Promise<PlatformAdapter> {
  const target = currentWindow();
  if (target.__MOON_BRIDGE_PLATFORM__) return target.__MOON_BRIDGE_PLATFORM__;
  const wails = wailsAdapter();
  if (wails) return wails;
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
    async getWindowSize() {
      return null;
    },
    async setWindowSize() {
      throw unsupportedError("setWindowSize");
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
  if (result.ok) {
    // Every binding populates value on success; a missing value means the
    // response shape is wrong, and silently returning undefined would surface
    // as phantom "stopped"/empty state in the UI.
    if (result.value === undefined) throw invalidResponseError(name);
    return result.value as T;
  }
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

export async function getWindowSize(): Promise<{ w: number; h: number } | null> {
  return (await resolveAdapter()).getWindowSize?.() ?? null;
}

export async function setWindowSize(width: number, height: number): Promise<void> {
  await (await resolveAdapter()).setWindowSize?.(width, height);
}
