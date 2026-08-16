// @vitest-environment jsdom
import { act } from "react";
import { createRoot } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import { RoutingProfileEditor } from "./RoutingProfileEditor";
import type { RoutingProfileSnapshot } from "../types/routingProfile";
import type { useRoutingProfiles } from "../hooks/useRoutingProfiles";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// React 19 controls input.value via an internal value tracker; assigning the
// property directly is ignored. Use the native setter + a bubbling input event.
function setNativeValue(element: HTMLInputElement | HTMLSelectElement, value: string) {
  const proto = element instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
  const setter = Object.getOwnPropertyDescriptor(proto, "value")!.set!;
  setter.call(element, value);
  // React 19's onChange on <input> fires from the native "input" event, while
  // <select> fires from the native "change" event.
  const eventName = element instanceof HTMLSelectElement ? "change" : "input";
  element.dispatchEvent(new Event(eventName, { bubbles: true }));
}

const snapshot: RoutingProfileSnapshot = {
  gatewayRunning: true,
  activeProfileId: "deepseek",
  profiles: [
    {
      id: "deepseek",
      displayName: "DeepSeek",
      active: true,
      configured: true,
      slots: [
        { id: "sol", displayName: "Sol", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash", reasoning: "max" },
        { id: "terra", displayName: "Terra", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash", reasoning: "high" },
        { id: "luna", displayName: "Luna", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash" },
      ],
    },
    {
      id: "openrouter",
      displayName: "OpenRouter",
      active: false,
      configured: true,
      slots: [
        { id: "sol", displayName: "Sol", providerId: "openrouter", providerLabel: "OpenRouter", upstreamModel: "openrouter/model-a", reasoning: "low" },
        { id: "terra", displayName: "Terra", providerId: "openrouter", providerLabel: "OpenRouter", upstreamModel: "openrouter/model-a", reasoning: "high" },
        { id: "luna", displayName: "Luna", providerId: "openrouter", providerLabel: "OpenRouter", upstreamModel: "openrouter/model-b" },
      ],
    },
  ],
};

function routingState(overrides: Record<string, unknown> = {}): Omit<ReturnType<typeof useRoutingProfiles>, "saveProfileDetailed"> & { capabilities: { modelId: string; supportedReasoning: string[]; defaultReasoning?: string }[] } {
  return {
    routing: snapshot,
    profiles: snapshot.profiles,
    activeProfileId: snapshot.activeProfileId,
    gatewayRunning: snapshot.gatewayRunning,
    activatingProfileId: null,
    saving: false,
    busy: false,
    error: null,
	commandError: null,
	saveStatus: null,
    refresh: () => Promise.resolve(),
    activateProfile: () => Promise.resolve(true),
    saveProfile: () => Promise.resolve(true),
    saveBaseline: () => Promise.resolve(false),
    capabilities: [
      { modelId: "deepseek-v4-flash", supportedReasoning: ["low", "high", "max"], defaultReasoning: "high" },
      { modelId: "openrouter/model-a", supportedReasoning: ["low", "high", "max"], defaultReasoning: "high" },
      { modelId: "openrouter/model-b", supportedReasoning: [] },
    ],
    ...overrides,
  };
}

function findSelectByAriaLabel(container: HTMLElement, label: string): HTMLSelectElement | undefined {
  return container.querySelector<HTMLSelectElement>(`select[aria-label="${label}"]`) ?? undefined;
}

function findProfileSelect(container: HTMLElement): HTMLSelectElement | undefined {
  return container.querySelector<HTMLSelectElement>(".routing-editor-profile-select select") ?? undefined;
}

function saveProfileMock() {
  return vi.fn(async () => true) as unknown as ReturnType<typeof useRoutingProfiles>["saveProfile"];
}

describe("RoutingProfileEditor", () => {
  it("renders the 3-row editor with backend-confirmed model and reasoning values", () => {
    const markup = renderToStaticMarkup(<RoutingProfileEditor routing={routingState()} />);
    expect(markup).toContain("モデル設定");
    expect(markup).toContain("Sol");
    expect(markup).toContain("Terra");
    expect(markup).toContain("Luna");
    expect(markup).toContain("deepseek-v4-flash");
    expect(markup).toContain("deepseek-v4-pro");
    expect(markup).toContain('value="max"');
    expect(markup).toContain('value="high"');
    expect(markup).toContain("Default");
    expect(markup).toContain("Thinking");
    expect(markup).toContain("通常");
    expect(markup).toContain("推論強度:");
    expect(markup).toContain('aria-label="Sol Reasoning"');
    expect(markup).toContain('aria-label="Terra Reasoning"');
    expect(markup).not.toContain('aria-label="Luna Reasoning"');
    expect(markup).toContain("モード");
    expect(markup).toContain("Thinking");
    expect(markup).toContain("通常");
    expect(markup).not.toContain("上流割当");
    expect(markup).not.toContain("プロバイダ");
  });

  it("hides the profile selector and shows the display name when only one profile exists", () => {
    const single = { ...snapshot, profiles: [snapshot.profiles[0]] };
    const markup = renderToStaticMarkup(<RoutingProfileEditor routing={routingState({ routing: single, profiles: single.profiles })} />);
    expect(markup).toContain("DeepSeek");
    expect(markup).not.toContain("プロファイル");
  });

  it("does not render secret-shaped fields", () => {
    const markup = renderToStaticMarkup(<RoutingProfileEditor routing={routingState()} />);
    expect(markup).not.toContain("apiKey");
    expect(markup).not.toContain("Authorization");
    expect(markup).not.toContain("sk-");
  });

  it("does not auto-save on mount", async () => {
    const saveProfile = saveProfileMock();
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfile })} />);
    });
    await act(async () => {});
    expect(saveProfile).not.toHaveBeenCalled();
    act(() => root.unmount());
  });

  it("auto-saves while the Gateway is stopped", async () => {
    const saveProfile = saveProfileMock();
    const stopped = routingState({ saveProfile, gatewayRunning: false, routing: { ...snapshot, gatewayRunning: false } });
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={stopped} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});

    expect(saveProfile).toHaveBeenCalledTimes(1);
    expect(saveProfile).toHaveBeenCalledWith(expect.objectContaining({
      slots: expect.objectContaining({ sol: expect.objectContaining({ upstreamModel: "deepseek-v4-pro" }) }),
    }));
    expect(container.textContent).toContain("Gateway停止中");
    act(() => root.unmount());
  });

  it("does not defer auto-save until the Gateway starts", async () => {
    const saveProfile = saveProfileMock();
    let current = routingState({ saveProfile, gatewayRunning: false, routing: { ...snapshot, gatewayRunning: false } });
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});
    expect(saveProfile).toHaveBeenCalledTimes(1);

    current = routingState({ saveProfile });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {});

    expect(saveProfile).toHaveBeenCalledTimes(1);
    expect(saveProfile).toHaveBeenCalledWith(expect.objectContaining({
      slots: expect.objectContaining({ sol: expect.objectContaining({ upstreamModel: "deepseek-v4-pro" }) }),
    }));
    act(() => root.unmount());
  });

  it("saves an edited Sol model change through the shared saveProfile", async () => {
    const saveProfile = saveProfileMock();
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfile })} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});

    expect(saveProfile).toHaveBeenCalledWith(expect.objectContaining({
      id: "deepseek",
      displayName: "DeepSeek",
      slots: expect.objectContaining({
        sol: expect.objectContaining({ provider: "deepseek", upstreamModel: "deepseek-v4-pro", reasoning: "max" }),
      }),
    }));
    act(() => root.unmount());
  });

  it("saves an edited Terra reasoning change through the shared saveProfile", async () => {
    const saveProfile = saveProfileMock();
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfile })} />);
    });

    const terraReasoning = findSelectByAriaLabel(container, "Terra Reasoning");
    expect(terraReasoning!.value).toBe("high");
    await act(async () => {
      setNativeValue(terraReasoning!, "max");
    });
    await act(async () => {});

    expect(saveProfile).toHaveBeenCalledWith(expect.objectContaining({
      slots: expect.objectContaining({
        terra: expect.objectContaining({ reasoning: "max" }),
      }),
    }));
    act(() => root.unmount());
  });

  it("saves Luna with reasoning null when override is Default", async () => {
    const saveProfile = saveProfileMock();
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfile })} />);
    });

    // Luna starts with no reasoning override ("" in draft). Change a different
    // field to make the editor dirty; Luna's reasoning should be null.
    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});

    expect(saveProfile).toHaveBeenCalledWith(expect.objectContaining({
      slots: expect.objectContaining({
        luna: expect.objectContaining({ reasoning: null }),
      }),
    }));
    act(() => root.unmount());
  });

  it("does not call activateProfile when saving", async () => {
    const activateProfile = vi.fn(async () => true) as unknown as ReturnType<typeof useRoutingProfiles>["activateProfile"];
    const saveProfile = saveProfileMock();
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfile, activateProfile })} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});

    expect(saveProfile).toHaveBeenCalledTimes(1);
    expect(activateProfile).not.toHaveBeenCalled();
    act(() => root.unmount());
  });

  it("allows saving a non-active profile without changing active_profile", async () => {
    const activateProfile = vi.fn(async () => true) as unknown as ReturnType<typeof useRoutingProfiles>["activateProfile"];
    const saveProfile = saveProfileMock();
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfile, activateProfile })} />);
    });

    // Select the non-active "openrouter" profile from the dropdown.
    const profileSelect = findProfileSelect(container);
    await act(async () => {
      setNativeValue(profileSelect!, "openrouter");
    });
    await act(async () => {});

    // OpenRouter's model values sit outside the DeepSeek catalog; the editor
    // must preserve them (fallback) and source provider from the snapshot.
    const solReasoning = findSelectByAriaLabel(container, "Sol Reasoning");
    expect(solReasoning!.value).toBe("low");
    await act(async () => {
      setNativeValue(solReasoning!, "max");
    });
    await act(async () => {});

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    expect(solModel!.value).toBe("openrouter/model-a");

    expect(saveProfile).toHaveBeenCalledWith(expect.objectContaining({
      id: "openrouter",
      displayName: "OpenRouter",
      slots: expect.objectContaining({
        sol: expect.objectContaining({ provider: "openrouter", upstreamModel: "openrouter/model-a", reasoning: "max" }),
        terra: expect.objectContaining({ provider: "openrouter", upstreamModel: "openrouter/model-a", reasoning: "high" }),
        luna: expect.objectContaining({ provider: "openrouter", upstreamModel: "openrouter/model-b", reasoning: null }),
      }),
    }));
    expect(activateProfile).not.toHaveBeenCalled();
    act(() => root.unmount());
  });

  it("blocks saving when the snapshot cannot provide a provider for every slot", async () => {
    // The deepseek profile is missing its Luna slot, so no provider can be
    // resolved for it. Save must fail closed (no auto-save call).
    const brokenSnapshot: RoutingProfileSnapshot = {
      ...snapshot,
      profiles: [{
        ...snapshot.profiles[0],
        slots: [
          { id: "sol", displayName: "Sol", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash", reasoning: "max" },
          { id: "terra", displayName: "Terra", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash", reasoning: "high" },
        ],
      }, snapshot.profiles[1]],
    };
    const saveProfile = saveProfileMock();
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfile, routing: brokenSnapshot, profiles: brokenSnapshot.profiles })} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});

    expect(saveProfile).not.toHaveBeenCalled();
    expect(container.textContent).toContain("プロファイル設定を読み込めないため保存できません。");
    act(() => root.unmount());
  });

  it("does not resend the same content after a failed save", async () => {
    let resolveSave!: (v: boolean) => void;
    const saveProfile = vi.fn(() => new Promise<boolean>((resolve) => { resolveSave = resolve; })) as unknown as ReturnType<typeof useRoutingProfiles>["saveProfile"];
    let current = routingState({ saveProfile });
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});
    expect(saveProfile).toHaveBeenCalledTimes(1);

    // Save is in flight, then fails.
    current = routingState({ saveProfile, saving: true });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {
      resolveSave!(false);
    });
    current = routingState({ saveProfile });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {});

    // The same content was already attempted; it must not be resent.
    expect(saveProfile).toHaveBeenCalledTimes(1);
    act(() => root.unmount());
  });

  it("saves a new serialized after a subsequent edit", async () => {
    let resolveSave!: (v: boolean) => void;
    const saveProfile = vi.fn(() => new Promise<boolean>((resolve) => { resolveSave = resolve; })) as unknown as ReturnType<typeof useRoutingProfiles>["saveProfile"];
    let current = routingState({ saveProfile });
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});
    expect(saveProfile).toHaveBeenCalledTimes(1);

    // First save fails.
    current = routingState({ saveProfile, saving: true });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {
      resolveSave!(false);
    });
    current = routingState({ saveProfile });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {});

    // A new value (Terra reasoning) must be saved even though the previous
    // attempt failed.
    const terraReasoning = findSelectByAriaLabel(container, "Terra Reasoning");
    await act(async () => {
      setNativeValue(terraReasoning!, "max");
    });
    await act(async () => {});

    expect(saveProfile).toHaveBeenCalledTimes(2);
    expect(saveProfile).toHaveBeenLastCalledWith(expect.objectContaining({
      slots: expect.objectContaining({ terra: expect.objectContaining({ reasoning: "max" }) }),
    }));
    act(() => root.unmount());
  });

  it("saves the latest diff when an edit lands during an in-flight save", async () => {
    let resolveSave!: (v: boolean) => void;
    const saveProfile = vi.fn(() => new Promise<boolean>((resolve) => { resolveSave = resolve; })) as unknown as ReturnType<typeof useRoutingProfiles>["saveProfile"];
    let current = routingState({ saveProfile });
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});
    expect(saveProfile).toHaveBeenCalledTimes(1);

    // The save is in flight; a second edit lands before it resolves.
    current = routingState({ saveProfile, saving: true });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    const terraReasoning = findSelectByAriaLabel(container, "Terra Reasoning");
    await act(async () => {
      setNativeValue(terraReasoning!, "max");
    });
    await act(async () => {});
    expect(saveProfile).toHaveBeenCalledTimes(1);

    await act(async () => {
      resolveSave!(true);
    });
    current = routingState({ saveProfile });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {});

    // The second edit is saved once saving clears.
    expect(saveProfile).toHaveBeenCalledTimes(2);
    expect(saveProfile).toHaveBeenLastCalledWith(expect.objectContaining({
      slots: expect.objectContaining({ terra: expect.objectContaining({ reasoning: "max" }) }),
    }));
    act(() => root.unmount());
  });

  it("keeps the edited draft authoritative after a successful save", async () => {
    const saveProfile = saveProfileMock();
    let current = routingState({ saveProfile });
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});
    expect(saveProfile).toHaveBeenCalledTimes(1);

    // The hook pushes back a backend snapshot confirming the change.
    const updatedSnapshot: RoutingProfileSnapshot = {
      ...snapshot,
      profiles: [{
        ...snapshot.profiles[0],
        slots: [
          { id: "sol", displayName: "Sol", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-pro", reasoning: "max" },
          { id: "terra", displayName: "Terra", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash", reasoning: "high" },
          { id: "luna", displayName: "Luna", providerId: "deepseek", providerLabel: "DeepSeek", upstreamModel: "deepseek-v4-flash" },
        ],
      }, snapshot.profiles[1]],
    };
    current = routingState({ saveProfile, routing: updatedSnapshot, profiles: updatedSnapshot.profiles });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {});

    // The draft (and select) keeps the edited value; no redundant re-save.
    const refreshedSol = findSelectByAriaLabel(container, "Sol 上流モデル");
    expect(refreshedSol!.value).toBe("deepseek-v4-pro");
    expect(saveProfile).toHaveBeenCalledTimes(1);
    act(() => root.unmount());
  });

  it("does not leak secret-shaped fields in save payloads", async () => {
    const saveProfileFn = vi.fn(async () => true);
    const saveProfile = saveProfileFn as unknown as ReturnType<typeof useRoutingProfiles>["saveProfile"];
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfile })} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});

    expect(saveProfileFn).toHaveBeenCalledTimes(1);
    const payloadStr = JSON.stringify(saveProfileFn.mock.calls[0]);
    expect(payloadStr).not.toContain("apiKey");
    expect(payloadStr).not.toContain("Authorization");
    expect(payloadStr).not.toContain("sk-");
    act(() => root.unmount());
  });
});

// Phase 6 fixes: async selection, delete fallback, non-dirty resync, manual
// retry, and the saveProfileDetailed canonical snapshot. Kept as an independent
// group so the draft-sovereignty / no-resend tests above remain untouched.
describe("RoutingProfileEditor phase 6", () => {
  it("selects the active profile when profiles arrive asynchronously", async () => {
    const container = document.createElement("div");
    const root = createRoot(container);
    const empty: RoutingProfileSnapshot = { ...snapshot, activeProfileId: "", profiles: [] };
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ routing: empty, profiles: [] })} />);
    });
    expect(container.textContent).toContain("プロファイルがありません。");

    // The async load lands: the active profile must be selected automatically.
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState()} />);
    });
    await act(async () => {});

    const profileSelect = findProfileSelect(container);
    expect(profileSelect!.value).toBe("deepseek"); // active profile
    expect(findSelectByAriaLabel(container, "Sol 上流モデル")!.value).toBe("deepseek-v4-flash");
    act(() => root.unmount());
  });

  it("falls back to the active profile when the selected profile is deleted", async () => {
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState()} />);
    });

    const profileSelect = findProfileSelect(container);
    await act(async () => {
      setNativeValue(profileSelect!, "openrouter");
    });
    await act(async () => {});
    expect(profileSelect!.value).toBe("openrouter");

    // Backend deletes openrouter; selection falls back to the active profile.
    const reduced: RoutingProfileSnapshot = { ...snapshot, profiles: [snapshot.profiles[0]] };
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ routing: reduced, profiles: reduced.profiles })} />);
    });
    await act(async () => {});

    // Single profile: selector is hidden, but the editor now shows DeepSeek.
    expect(findProfileSelect(container)).toBeUndefined();
    expect(findSelectByAriaLabel(container, "Sol 上流モデル")!.value).toBe("deepseek-v4-flash");
    act(() => root.unmount());
  });

  it("re-syncs a clean draft from the backend snapshot on refresh", async () => {
    const saveProfile = saveProfileMock();
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfile })} />);
    });
    await act(async () => {});
    expect(findSelectByAriaLabel(container, "Terra 上流モデル")!.value).toBe("deepseek-v4-flash");

    // Backend changes Terra while the draft is clean; a refresh adopts it.
    const refreshed: RoutingProfileSnapshot = {
      ...snapshot,
      profiles: [{
        ...snapshot.profiles[0],
        slots: [
          snapshot.profiles[0].slots[0],
          { ...snapshot.profiles[0].slots[1], upstreamModel: "deepseek-v4-pro" },
          snapshot.profiles[0].slots[2],
        ],
      }, snapshot.profiles[1]],
    };
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfile, routing: refreshed, profiles: refreshed.profiles })} />);
    });
    await act(async () => {});

    expect(findSelectByAriaLabel(container, "Terra 上流モデル")!.value).toBe("deepseek-v4-pro");
    expect(saveProfile).not.toHaveBeenCalled();
    act(() => root.unmount());
  });

  it("does not overwrite a dirty draft when a refresh arrives", async () => {
    let resolveSave!: (v: boolean) => void;
    const saveProfile = vi.fn(() => new Promise<boolean>((resolve) => { resolveSave = resolve; })) as unknown as ReturnType<typeof useRoutingProfiles>["saveProfile"];
    let current = routingState({ saveProfile });
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});
    expect(saveProfile).toHaveBeenCalledTimes(1);

    // The save fails, leaving the draft dirty.
    current = routingState({ saveProfile, saving: true });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {
      resolveSave!(false);
    });
    current = routingState({ saveProfile });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {});

    // A refresh with a different backend Sol value must not clobber the draft.
    const external: RoutingProfileSnapshot = {
      ...snapshot,
      profiles: [{
        ...snapshot.profiles[0],
        slots: [
          { ...snapshot.profiles[0].slots[0], upstreamModel: "external-model" },
          snapshot.profiles[0].slots[1],
          snapshot.profiles[0].slots[2],
        ],
      }, snapshot.profiles[1]],
    };
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfile, routing: external, profiles: external.profiles })} />);
    });
    await act(async () => {});

    expect(findSelectByAriaLabel(container, "Sol 上流モデル")!.value).toBe("deepseek-v4-pro");
    expect(container.textContent).toContain("保存を再試行");
    act(() => root.unmount());
  });

  it("resends the failed save once when the retry button is clicked", async () => {
    let resolveSave!: (v: boolean) => void;
    const saveProfile = vi.fn(() => new Promise<boolean>((resolve) => { resolveSave = resolve; })) as unknown as ReturnType<typeof useRoutingProfiles>["saveProfile"];
    let current = routingState({ saveProfile });
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});
    expect(saveProfile).toHaveBeenCalledTimes(1);

    current = routingState({ saveProfile, saving: true });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {
      resolveSave!(false);
    });
    current = routingState({ saveProfile });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {});

    expect(saveProfile).toHaveBeenCalledTimes(1);
    const retry = container.querySelector<HTMLButtonElement>(".routing-editor-retry");
    expect(retry).not.toBeNull();

    // A manual retry resends exactly once; there is no automatic retry loop.
    await act(async () => {
      retry!.click();
    });
    await act(async () => {});
    expect(saveProfile).toHaveBeenCalledTimes(2);
    await act(async () => {});
    expect(saveProfile).toHaveBeenCalledTimes(2);
    act(() => root.unmount());
  });

  it("rebases the draft on the canonical snapshot after a successful detailed save", async () => {
    const canonical: RoutingProfileSnapshot = {
      ...snapshot,
      profiles: [{
        ...snapshot.profiles[0],
        slots: [
          { ...snapshot.profiles[0].slots[0], upstreamModel: "deepseek-v4-pro" },
          snapshot.profiles[0].slots[1],
          snapshot.profiles[0].slots[2],
        ],
      }, snapshot.profiles[1]],
    };
    const saveProfileDetailed = vi.fn(async () => ({ ok: true, snapshot: canonical })) as unknown as ReturnType<typeof useRoutingProfiles>["saveProfileDetailed"];
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfileDetailed })} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});
    await act(async () => {});

    expect(saveProfileDetailed).toHaveBeenCalledTimes(1);
    expect(findSelectByAriaLabel(container, "Sol 上流モデル")!.value).toBe("deepseek-v4-pro");

    // The draft is clean against the canonical baseline: a refresh carrying the
    // same canonical profile does not re-save.
    const refreshSave = vi.fn(async () => ({ ok: true, snapshot: canonical })) as unknown as ReturnType<typeof useRoutingProfiles>["saveProfileDetailed"];
    await act(async () => {
      root.render(<RoutingProfileEditor routing={routingState({ saveProfileDetailed: refreshSave, routing: canonical, profiles: canonical.profiles })} />);
    });
    await act(async () => {});
    expect(refreshSave).not.toHaveBeenCalled();
    act(() => root.unmount());
  });

  it("shows the retry button when a detailed save fails", async () => {
    let resolveSave!: (v: { ok: boolean; snapshot: RoutingProfileSnapshot | null }) => void;
    const saveProfileDetailed = vi.fn(() => new Promise<{ ok: boolean; snapshot: RoutingProfileSnapshot | null }>((resolve) => { resolveSave = resolve; })) as unknown as ReturnType<typeof useRoutingProfiles>["saveProfileDetailed"];
    let current = routingState({ saveProfileDetailed });
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });

    const solModel = findSelectByAriaLabel(container, "Sol 上流モデル");
    await act(async () => {
      setNativeValue(solModel!, "deepseek-v4-pro");
    });
    await act(async () => {});
    expect(saveProfileDetailed).toHaveBeenCalledTimes(1);

    current = routingState({ saveProfileDetailed, saving: true });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {
      resolveSave!({ ok: false, snapshot: null });
    });
    current = routingState({ saveProfileDetailed });
    await act(async () => {
      root.render(<RoutingProfileEditor routing={current} />);
    });
    await act(async () => {});

    expect(container.querySelector<HTMLButtonElement>(".routing-editor-retry")).not.toBeNull();
    expect(saveProfileDetailed).toHaveBeenCalledTimes(1);
    act(() => root.unmount());
  });
});
