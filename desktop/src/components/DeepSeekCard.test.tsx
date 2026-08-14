// @vitest-environment jsdom
import { act } from "react";
import { createRoot } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
import { DeepSeekCard } from "./DeepSeekCard";
import type { GatewaySnapshot } from "../types/gateway";
import type { DeepSeekStatus } from "../types/deepseek";

const stopped: GatewaySnapshot = {
  state: "stopped",
  address: "127.0.0.1:38440",
  configPath: "",
  pid: null,
  instanceId: null,
  error: null,
};

const running: GatewaySnapshot = { ...stopped, state: "running" };

const unconfigured: DeepSeekStatus = {
  gatewayRunning: true,
  providerExists: false,
  apiKeySet: false,
  credentialSource: "none",
  credentialState: "missing",
  configured: false,
  active: false,
  selectedModel: null,
  reasoningEffort: "high",
  reasoningExplicitlyConfigured: false,
  allowedReasoningEfforts: ["high", "max"],
  routeAlias: "moonbridge",
  defaultModel: "",
  pro: { modelId: "deepseek-v4-pro", reasoning: "high", supported: ["high", "max"] },
  flash: { modelId: "deepseek-v4-flash", reasoning: "high", supported: ["low", "high", "max"] },
};

const configured: DeepSeekStatus = { ...unconfigured, providerExists: true, apiKeySet: true, credentialSource: "stored", credentialState: "available", configured: true, active: true, selectedModel: "deepseek-v4-flash" };

function routingStub() {
  return {
    routing: { gatewayRunning: true, activeProfileId: "deepseek", profiles: [] },
    profiles: [],
    activeProfileId: "deepseek",
    gatewayRunning: true,
    activatingProfileId: null,
    saving: false,
    busy: false,
    error: null,
    commandError: null,
    refresh: () => Promise.resolve(),
    activateProfile: () => Promise.resolve(true),
    saveProfile: () => Promise.resolve(true),
  } as never;
}

function deepseekStub(status: DeepSeekStatus | null = null, overrides: Record<string, unknown> = {}) {
  return {
    status,
    metadata: null,
    model: "deepseek-v4-pro",
    setModel: () => undefined,
    reasoningEffort: "high",
    setReasoningEffort: () => undefined,
    reasoningOptions: ["high", "max"],
    saving: false,
    error: null,
    progress: null,
    operationId: null,
    commandError: null,
    connectionTest: null,
    testingConnection: false,
    hasUnsavedChanges: false,
    refresh: () => Promise.resolve(),
    configure: () => Promise.resolve(true),
    testConnection: () => Promise.resolve(null),
    clearKey: () => Promise.resolve(true),
    ...overrides,
  } as never;
}

// React 19 controls input.value via an internal value tracker; assigning the
// property directly is ignored. Use the native setter + a bubbling input event.
function setInputValue(element: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
  setter.call(element, value);
  element.dispatchEvent(new Event("input", { bubbles: true }));
}

// React maps onBlur to the delegated focusout event.
function blurInput(element: HTMLInputElement) {
  element.dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
}

function keyInput(container: HTMLElement): HTMLInputElement {
  return container.querySelector<HTMLInputElement>(".deepseek-key-input")!;
}

function envInput(container: HTMLElement): HTMLInputElement {
  return container.querySelector<HTMLInputElement>(".deepseek-env-input")!;
}

function saveKeyButton(container: HTMLElement): HTMLButtonElement {
  return container.querySelector<HTMLButtonElement>(".deepseek-key-field button")!;
}

function connectionButton(container: HTMLElement): HTMLButtonElement {
  return container.querySelector<HTMLButtonElement>(".deepseek-summary-actions button")!;
}

async function renderCard(snapshot: GatewaySnapshot, deepseek: ReturnType<typeof deepseekStub>, routing = routingStub()) {
  const container = document.createElement("div");
  const root = createRoot(container);
  await act(async () => {
    root.render(<DeepSeekCard snapshot={snapshot} deepseek={deepseek} routing={routing} />);
  });
  return { container, root };
}

describe("DeepSeekCard env var auto-save", () => {
  it("does not save before the status has loaded", async () => {
    const configure = vi.fn(() => Promise.resolve(true));
    const { container, root } = await renderCard(running, deepseekStub(null, { configure }));

    await act(async () => {
      setInputValue(envInput(container), "MY_ENV");
    });
    await act(async () => {
      blurInput(envInput(container));
    });
    await act(async () => {});

    expect(configure).not.toHaveBeenCalled();
    act(() => root.unmount());
  });

  it("does not save when only the env var name changes; the blur commits the save", async () => {
    const configure = vi.fn(() => Promise.resolve(true));
    const { container, root } = await renderCard(running, deepseekStub(configured, { configure }));

    await act(async () => {
      setInputValue(envInput(container), "NEW_ENV");
    });
    await act(async () => {});

    expect(configure).not.toHaveBeenCalled();

    await act(async () => {
      blurInput(envInput(container));
    });
    await act(async () => {});

    expect(configure).toHaveBeenCalledTimes(1);
    expect(configure).toHaveBeenCalledWith("", "NEW_ENV");
    act(() => root.unmount());
  });

  it("saves an env change with the existing key untouched", async () => {
    const configure = vi.fn(() => Promise.resolve(true));
    const { container, root } = await renderCard(running, deepseekStub(configured, { configure }));

    await act(async () => {
      setInputValue(keyInput(container), "sk-new");
    });
    await act(async () => {
      setInputValue(envInput(container), "OTHER_ENV");
    });
    await act(async () => {
      blurInput(envInput(container));
    });
    await act(async () => {});

    // The env save never carries the typed (unsaved) key.
    expect(configure).toHaveBeenCalledTimes(1);
    expect(configure).toHaveBeenCalledWith("", "OTHER_ENV");
    act(() => root.unmount());
  });

  it("clears the pending request when a first-time env blur cannot be saved", async () => {
    const configure = vi.fn(() => Promise.resolve(true));
    const { container, root } = await renderCard(running, deepseekStub(unconfigured, { configure }));

    await act(async () => {
      setInputValue(envInput(container), "MY_ENV");
    });
    await act(async () => {
      blurInput(envInput(container));
    });
    await act(async () => {});

    // First-time setup needs the key (manual save only), so the env-only blur
    // must not call configure.
    expect(configure).not.toHaveBeenCalled();

    // Typing a key afterward must NOT auto-save; only the button commits.
    await act(async () => {
      setInputValue(keyInput(container), "sk-first");
    });
    await act(async () => {});

    expect(configure).not.toHaveBeenCalled();

    await act(async () => {
      saveKeyButton(container).click();
    });
    await act(async () => {});

    expect(configure).toHaveBeenCalledTimes(1);
    expect(configure).toHaveBeenCalledWith("sk-first", "MY_ENV");
    act(() => root.unmount());
  });

  it("saves an env blur while the Gateway is stopped", async () => {
    const configure = vi.fn(() => Promise.resolve(true));
    const { container, root } = await renderCard(stopped, deepseekStub(configured, { configure }));

    await act(async () => {
      setInputValue(envInput(container), "HELD_ENV");
    });
    await act(async () => {
      blurInput(envInput(container));
    });
    await act(async () => {});

    expect(configure).toHaveBeenCalledTimes(1);
    expect(configure).toHaveBeenCalledWith("", "HELD_ENV");
    expect(container.textContent).not.toContain("Gateway開始後に保存されます。");
    act(() => root.unmount());
  });

  it("does not defer an env blur until the Gateway starts", async () => {
    const configure = vi.fn(() => Promise.resolve(true));
    const { container, root } = await renderCard(stopped, deepseekStub(configured, { configure }));

    await act(async () => {
      setInputValue(envInput(container), "HELD_ENV");
    });
    await act(async () => {
      blurInput(envInput(container));
    });
    await act(async () => {});
    expect(configure).toHaveBeenCalledTimes(1);
    expect(configure).toHaveBeenCalledWith("", "HELD_ENV");

    await act(async () => {
      root.render(<DeepSeekCard snapshot={running} deepseek={deepseekStub(configured, { configure })} routing={routingStub()} />);
    });
    await act(async () => {});

    expect(configure).toHaveBeenCalledTimes(1);
    act(() => root.unmount());
  });
});

describe("DeepSeekCard manual API key save", () => {
  it("does not save when only a key is typed", async () => {
    const configure = vi.fn(() => Promise.resolve(true));
    const { container, root } = await renderCard(running, deepseekStub(configured, { configure }));

    await act(async () => {
      setInputValue(keyInput(container), "sk-typed");
    });
    await act(async () => {});

    expect(configure).not.toHaveBeenCalled();
    act(() => root.unmount());
  });

  it("saves the key when the button is clicked", async () => {
    const configure = vi.fn(() => Promise.resolve(true));
    const { container, root } = await renderCard(running, deepseekStub(configured, { configure }));

    await act(async () => {
      setInputValue(keyInput(container), "sk-typed");
    });
    await act(async () => {
      saveKeyButton(container).click();
    });
    await act(async () => {});

    expect(configure).toHaveBeenCalledTimes(1);
    expect(configure).toHaveBeenCalledWith("sk-typed", undefined);
    act(() => root.unmount());
  });

  it("saves an env change alongside the key when both are dirty", async () => {
    const configure = vi.fn(() => Promise.resolve(true));
    const { container, root } = await renderCard(running, deepseekStub(configured, { configure }));

    await act(async () => {
      setInputValue(keyInput(container), "sk-both");
    });
    await act(async () => {
      setInputValue(envInput(container), "BOTH_ENV");
    });
    await act(async () => {
      saveKeyButton(container).click();
    });
    await act(async () => {});

    expect(configure).toHaveBeenCalledTimes(1);
    expect(configure).toHaveBeenCalledWith("sk-both", "BOTH_ENV");
    act(() => root.unmount());
  });

  it("saves the key while the Gateway is stopped", async () => {
    const configure = vi.fn(() => Promise.resolve(true));
    const { container, root } = await renderCard(stopped, deepseekStub(configured, { configure }));

    await act(async () => {
      setInputValue(keyInput(container), "sk-held");
    });
    await act(async () => {
      saveKeyButton(container).click();
    });
    await act(async () => {});

    expect(saveKeyButton(container).disabled).toBe(true);
    expect(configure).toHaveBeenCalledTimes(1);
    expect(configure).toHaveBeenCalledWith("sk-held", undefined);
    act(() => root.unmount());
  });

  it("disables the key save button when the input is empty", async () => {
    const { container, root } = await renderCard(running, deepseekStub(configured));
    expect(saveKeyButton(container).disabled).toBe(true);
    act(() => root.unmount());
  });

  it("enables the key save button once a key is entered", async () => {
    const { container, root } = await renderCard(running, deepseekStub(configured));

    await act(async () => {
      setInputValue(keyInput(container), "sk-entered");
    });

    expect(saveKeyButton(container).disabled).toBe(false);
    act(() => root.unmount());
  });

  it("does not auto-retry after a failed save; a later click retries", async () => {
    let resolveConfigure!: (v: boolean) => void;
    const configure = vi.fn(() => new Promise<boolean>((resolve) => { resolveConfigure = resolve; }));
    const { container, root } = await renderCard(running, deepseekStub(configured, { configure }));

    await act(async () => {
      setInputValue(keyInput(container), "sk-fail");
    });
    await act(async () => {
      saveKeyButton(container).click();
    });
    await act(async () => {});
    expect(configure).toHaveBeenCalledTimes(1);

    await act(async () => {
      resolveConfigure(false);
    });
    await act(async () => {});

    // Manual save: no effect-driven second attempt.
    expect(configure).toHaveBeenCalledTimes(1);
    expect(keyInput(container).value).toBe("sk-fail");

    await act(async () => {
      saveKeyButton(container).click();
    });
    await act(async () => {});

    expect(configure).toHaveBeenCalledTimes(2);
    expect(configure).toHaveBeenLastCalledWith("sk-fail", undefined);
    act(() => root.unmount());
  });

  it("clears the key field after a successful save", async () => {
    const configure = vi.fn(() => Promise.resolve(true));
    const { container, root } = await renderCard(running, deepseekStub(configured, { configure }));

    await act(async () => {
      setInputValue(keyInput(container), "sk-ok");
    });
    await act(async () => {
      saveKeyButton(container).click();
    });
    await act(async () => {});

    expect(keyInput(container).value).toBe("");
    act(() => root.unmount());
  });
});

describe("DeepSeekCard summary bar connection test", () => {
  it("places the connection test in the summary bar, disabled while stopped with a reason", () => {
    const markup = renderToStaticMarkup(<DeepSeekCard snapshot={stopped} deepseek={deepseekStub()} routing={routingStub()} />);
    expect(markup).toContain('class="deepseek-summary-bar"');
    expect(markup).toMatch(/<div class="deepseek-summary-actions">.*<button type="button" class="btn btn-secondary"[^>]*disabled=""[^>]*>接続を確認<\/button><\/div>/);
    expect(markup).toContain('title="Gateway実行中のみ利用できます"');
    expect(markup).not.toContain("接続を確認中");
  });

  it("enables the connection test while running", () => {
    const markup = renderToStaticMarkup(<DeepSeekCard snapshot={running} deepseek={deepseekStub(configured)} routing={routingStub()} />);
    expect(markup).toMatch(/<button type="button" class="btn btn-secondary">接続を確認<\/button>/);
  });

  it("keeps the connection test visible when the card is collapsed", async () => {
    const { container, root } = await renderCard(running, deepseekStub(configured));

    const summary = container.querySelector<HTMLButtonElement>(".deepseek-provider-summary");
    expect(connectionButton(container)).toBeDefined();

    await act(async () => {
      summary!.click();
    });

    expect(summary!.getAttribute("aria-expanded")).toBe("false");
    expect(container.querySelector("#deepseek-provider-content")).toBeNull();
    expect(connectionButton(container)).toBeDefined();
    act(() => root.unmount());
  });
});

describe("DeepSeekCard connection test results", () => {
  function resultMarkup(result: { ok: boolean; code: string; message: string; model: string }) {
    const connectionTest = {
      operationId: "op-1",
      result,
      gatewaySnapshot: running,
      gatewayLeftRunning: true,
      warning: null,
    };
    return renderToStaticMarkup(<DeepSeekCard snapshot={running} deepseek={deepseekStub(configured, { connectionTest })} routing={routingStub()} />);
  }

  it("maps ok to 接続成功", () => {
    const markup = resultMarkup({ ok: true, code: "ok", message: "connection succeeded", model: "deepseek-v4-pro" });
    expect(markup).toContain("接続成功（ok）");
    expect(markup).toContain("success-text");
  });

  it("maps credential_unavailable to an actionable re-entry prompt", () => {
    const markup = resultMarkup({ ok: false, code: "credential_unavailable", message: "no usable API key", model: "" });
    expect(markup).toContain("APIキーが利用できません。保存済みキーを再入力するか、環境変数を確認してください（credential_unavailable）");
    expect(markup).toContain("error-text");
  });

  it("maps auth_failed to a key-check prompt", () => {
    const markup = resultMarkup({ ok: false, code: "auth_failed", message: "authentication failed", model: "" });
    expect(markup).toContain("認証に失敗しました。APIキーが正しいか確認してください（auth_failed）");
  });

  it("maps rate_limited to a wait prompt", () => {
    const markup = resultMarkup({ ok: false, code: "rate_limited", message: "rate limited", model: "" });
    expect(markup).toContain("レート制限中です。しばらく待ってから再試行してください（rate_limited）");
  });

  it("maps timeout to a gateway-check prompt", () => {
    const markup = resultMarkup({ ok: false, code: "timeout", message: "timed out", model: "" });
    expect(markup).toContain("接続がタイムアウトしました。Gatewayの状態を確認して再試行してください（timeout）");
  });

  it("maps network_error to a connection-check prompt", () => {
    const markup = resultMarkup({ ok: false, code: "network_error", message: "network failure", model: "" });
    expect(markup).toContain("ネットワーク接続に失敗しました。接続を確認してください（network_error）");
  });

  it("maps model_unavailable to a model-config prompt", () => {
    const markup = resultMarkup({ ok: false, code: "model_unavailable", message: "no model", model: "" });
    expect(markup).toContain("プローブに使えるモデルがありません。モデル設定を確認してください（model_unavailable）");
  });

  it("maps general to a retry prompt", () => {
    const markup = resultMarkup({ ok: false, code: "general", message: "upstream error", model: "" });
    expect(markup).toContain("接続確認に失敗しました。しばらくして再試行してください（general）");
  });

  it("uses generic safe wording for an unknown code", () => {
    const markup = resultMarkup({ ok: false, code: "something_new", message: "safe server wording", model: "" });
    expect(markup).toContain("接続確認に失敗しました。しばらくして再試行してください（something_new）");
    expect(markup).not.toContain("safe server wording");
  });

  it("does not surface a raw upstream error body", () => {
    const markup = resultMarkup({ ok: false, code: "auth_failed", message: "invalid api key xyz", model: "" });
    expect(markup).not.toContain("invalid api key xyz");
    expect(markup).toContain("認証に失敗しました。APIキーが正しいか確認してください");
  });
});

describe("DeepSeekCard gateway lifecycle contract", () => {
  it("requires a first-time API key", () => {
    const markup = renderToStaticMarkup(<DeepSeekCard snapshot={running} deepseek={deepseekStub(unconfigured)} routing={routingStub()} />);
    expect(markup).toContain("初回設定ではAPI keyを入力してください。");
  });

  it("shows a loading hint before the status has loaded", () => {
    const markup = renderToStaticMarkup(<DeepSeekCard snapshot={running} deepseek={deepseekStub()} routing={routingStub()} />);
    expect(markup).toContain("設定を読み込んでいます。");
  });

  it("renders the provider summary, key save button, and nested routing editor", () => {
    const markup = renderToStaticMarkup(<DeepSeekCard snapshot={running} deepseek={deepseekStub(configured)} routing={routingStub()} />);

    expect(markup).toContain("DEEPSEEK_API_KEY");
    expect(markup).toContain('aria-expanded="true"');
    expect(markup).toContain('aria-controls="deepseek-provider-content"');
    expect(markup).toContain("キーを保存");
    expect(markup).toContain('class="routing-editor-embedded"');
    expect(markup).not.toContain("モデル設定");
    expect(markup).not.toContain('class="panel routing-editor"');
    expect(markup).toContain("環境変数名");
    expect(markup).toContain('class="deepseek-env-input"');
  });

  it("collapses and expands the full summary row", async () => {
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<DeepSeekCard snapshot={running} deepseek={deepseekStub(configured)} routing={routingStub()} />);
    });

    const summary = container.querySelector<HTMLButtonElement>(".deepseek-provider-summary");
    expect(summary).toBeDefined();
    expect(summary!.getAttribute("aria-expanded")).toBe("true");
    expect(container.querySelector("#deepseek-provider-content")).not.toBeNull();

    await act(async () => {
      summary!.click();
    });

    expect(summary!.getAttribute("aria-expanded")).toBe("false");
    expect(container.querySelector("#deepseek-provider-content")).toBeNull();

    await act(async () => {
      summary!.click();
    });

    expect(summary!.getAttribute("aria-expanded")).toBe("true");
    expect(container.querySelector("#deepseek-provider-content")).not.toBeNull();
    act(() => root.unmount());
  });
});

describe("DeepSeekCard credential state badge", () => {
  function badgeMarkup(status: DeepSeekStatus | null) {
    return renderToStaticMarkup(<DeepSeekCard snapshot={running} deepseek={deepseekStub(status)} routing={routingStub()} />);
  }

  it("shows 未確認 before the status has loaded", () => {
    const markup = badgeMarkup(null);
    expect(markup).toContain('class="deepseek-state unknown">未確認');
  });

  it("shows 保存済みキー for an available stored key", () => {
    expect(badgeMarkup(configured)).toContain('class="deepseek-state active">保存済みキー');
  });

  it("shows 設定済（{env}） for an available environment key", () => {
    const envStatus: DeepSeekStatus = { ...configured, apiKeySet: true, credentialSource: "environment", credentialState: "available", apiKeyEnv: "MY_DEEPSEEK_KEY" };
    expect(badgeMarkup(envStatus)).toContain('class="deepseek-state active">設定済（MY_DEEPSEEK_KEY）');
  });

  it("shows 未設定 for a missing credential", () => {
    expect(badgeMarkup(unconfigured)).toContain('class="deepseek-state inactive">未設定');
  });

  it("shows 保存済みキー while stopped with a stored key", () => {
    const unverified: DeepSeekStatus = { ...configured, apiKeySet: true, credentialSource: "stored", credentialState: "unverified" };
    expect(badgeMarkup(unverified)).toContain('class="deepseek-state unverified">保存済みキー');
  });

  it("does not put connection validity into the stored-key badge", () => {
    const unverified: DeepSeekStatus = { ...configured, apiKeySet: true, credentialSource: "stored", credentialState: "unverified" };
    const markup = badgeMarkup(unverified);
    expect(markup).not.toContain("Gateway起動後に確認");
  });
});

describe("DeepSeekCard main settings deletion UI", () => {
  it("does not render a stored-key deletion control", () => {
    const markup = renderToStaticMarkup(<DeepSeekCard snapshot={stopped} deepseek={deepseekStub(configured)} routing={routingStub()} />);
    expect(markup).not.toContain("保存済みキーを削除");
    expect(markup).not.toContain("deepseek-clear-key-btn");
  });
});

/*
  ClearDeepSeekKey remains a backend operation, but deletion is intentionally
  not exposed from the main provider settings card.
*/
  it("shows APIキーの再入力が必要です when the stored key cannot be decrypted", () => {
    const unavailable: DeepSeekStatus = { ...configured, apiKeySet: true, credentialSource: "stored", credentialState: "unavailable", credentialErrorCode: "decrypt_failed" };
    expect(renderToStaticMarkup(<DeepSeekCard snapshot={running} deepseek={deepseekStub(unavailable)} routing={routingStub()} />)).toContain('class="deepseek-state unavailable">APIキーの再入力が必要です');
  });

// ClearDeepSeekKey remains a backend operation, but deletion is intentionally
// not exposed from the main provider settings card.
