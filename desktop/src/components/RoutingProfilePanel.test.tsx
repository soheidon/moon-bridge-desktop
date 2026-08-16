// @vitest-environment jsdom
import { act } from "react";
import { createRoot } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import { RoutingProfilePanel } from "./RoutingProfilePanel";
import type { RoutingProfileSnapshot } from "../types/routingProfile";
import type { RuntimeConfigurationSnapshot } from "../types/gateway";
import type { useRoutingProfiles } from "../hooks/useRoutingProfiles";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function routingState(overrides: Record<string, unknown> = {}): ReturnType<typeof useRoutingProfiles> {
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
    ],
  };
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
		saveProfileDetailed: () => Promise.resolve({ ok: false, snapshot: null, status: "save_failed", error: null }),
    saveBaseline: () => Promise.resolve(false),
    ...overrides,
  };
}

describe("RoutingProfilePanel", () => {
  it("uses the Anthro Bridge heading 使用するLLMプロバイダ", () => {
    const markup = renderToStaticMarkup(<RoutingProfilePanel routing={routingState()} />);
    expect(markup).toContain("使用するLLMプロバイダ");
  });

  it("renders each profile card with the 3-slot route summary", () => {
    const markup = renderToStaticMarkup(<RoutingProfilePanel routing={routingState()} />);
    expect(markup).toContain("DeepSeek");
    expect(markup).toContain("Sol");
    expect(markup).toContain("Terra");
    expect(markup).toContain("Luna");
    expect(markup).toContain("Sol → deepseek-v4-flash + thinking: Max");
    expect(markup).toContain("Terra → deepseek-v4-flash + thinking: High");
    expect(markup).toContain("Luna → deepseek-v4-flash");
    expect(markup).toContain("選択中");
    expect(markup).not.toContain("このプロバイダに切替");
    expect(markup).not.toContain("利用中");
  });

  it("marks exactly the backend-confirmed active profile card", () => {
    const markup = renderToStaticMarkup(<RoutingProfilePanel routing={routingState()} />);
    expect(markup.match(/選択中/g)?.length).toBe(1);
  });

  it("disables cards while the Gateway is unavailable", () => {
    const markup = renderToStaticMarkup(<RoutingProfilePanel routing={routingState({ gatewayRunning: false, activeProfileId: "", routing: { gatewayRunning: false, activeProfileId: "", profiles: routingState().profiles } })} />);
    expect(markup.match(/disabled=""/g)?.length).toBe(1);
    expect(markup).toContain("Gateway開始後に切替できます");
  });

  it("shows 切替中… only for the profile being activated", () => {
    const markup = renderToStaticMarkup(<RoutingProfilePanel routing={routingState({ activatingProfileId: "deepseek", busy: true })} />);
    expect(markup).toContain("切替中…");
    expect(markup.match(/切替中…/g)?.length).toBe(1);
  });

  it("does not render secret-shaped fields", () => {
    const markup = renderToStaticMarkup(<RoutingProfilePanel routing={routingState()} />);
    expect(markup).not.toContain("apiKey");
    expect(markup).not.toContain("Authorization");
    expect(markup).not.toContain("sk-");
  });

  it("keeps configured controls while showing the authoritative effective runtime state", () => {
    const runtime: RuntimeConfigurationSnapshot = {
      state: "ready",
      serverInstance: "server#1",
      resolverGeneration: 2,
      installSource: "profile_refresh",
      configSource: "persisted_store",
      resolverPresent: true,
      routingExtensionState: "valid",
      activeProfileState: "present_valid",
      readySlotCount: 3,
      credentialState: "available",
      slots: {
        sol: { state: "ready", provider: "deepseek", upstreamModel: "deepseek-v4-flash", mode: "thinking", configuredEffort: "max", credentialState: "available" },
        terra: { state: "ready", provider: "deepseek", upstreamModel: "deepseek-v4-flash", mode: "thinking", configuredEffort: "high", credentialState: "available" },
        luna: { state: "ready", provider: "deepseek", upstreamModel: "deepseek-v4-flash", mode: "normal", configuredEffort: "none", credentialState: "available" },
      },
    };
    const markup = renderToStaticMarkup(<RoutingProfilePanel routing={routingState()} runtime={runtime} />);
    expect(markup).toContain("Gateway実効設定 / Effective configuration");
    expect(markup).toContain("Ready · 3/3 ready");
    expect(markup).toContain("Sol → deepseek / deepseek-v4-flash / Thinking / max");
    expect(markup).toContain("使用するLLMプロバイダ");
    expect(markup).toContain("選択中");
  });

  it("does not substitute configured values when effective runtime is invalid", () => {
    const runtime: RuntimeConfigurationSnapshot = {
      state: "invalid",
      serverInstance: "server#1",
      resolverGeneration: 1,
      installSource: "startup",
      configSource: "persisted_store",
      resolverPresent: true,
      routingExtensionState: "invalid",
      activeProfileState: "missing",
      readySlotCount: 0,
      credentialState: "unknown",
      slots: {
        sol: { state: "invalid" },
        terra: { state: "invalid" },
        luna: { state: "invalid" },
      },
    };
    const markup = renderToStaticMarkup(<RoutingProfilePanel routing={routingState()} runtime={runtime} />);
    expect(markup).toContain("Invalid · 0/3 ready");
    expect(markup).toContain("Sol → invalid");
    expect(markup).toContain("Routing: invalid · Active profile: missing");
  });

  it("delegates card click to the shared activateProfile with the default sol slot", async () => {
    const activateProfile = vi.fn(async () => true) as unknown as ReturnType<typeof useRoutingProfiles>["activateProfile"];
    const inactive = routingState();
    const next = {
      ...inactive,
      activeProfileId: null as string | null,
      routing: {
        ...inactive.routing!,
        activeProfileId: "",
        profiles: [{ ...inactive.routing!.profiles[0], active: false }],
      },
      profiles: [{ ...inactive.routing!.profiles[0], active: false }],
      activateProfile,
      busy: false,
    };
    const container = document.createElement("div");
    const root = createRoot(container);
    await act(async () => {
      root.render(<RoutingProfilePanel routing={next} />);
    });

    const cards = Array.from(container.querySelectorAll<HTMLButtonElement>("button.provider-card"));
    expect(cards).toHaveLength(1);
    await act(async () => {
      cards[0]!.click();
    });

    expect(activateProfile).toHaveBeenCalledWith("deepseek");
    act(() => root.unmount());
  });
});
