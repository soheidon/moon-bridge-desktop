// @vitest-environment jsdom
import { createRoot } from "react-dom/client";
import { act } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  command: vi.fn(),
  onEvent: vi.fn<(name: string, listener: (payload: unknown) => void) => () => void>(() => () => undefined),
}));

vi.mock("../platform/desktop", () => ({
  command: mocks.command,
  onEvent: mocks.onEvent,
}));

import type { GatewaySnapshot } from "../types/gateway";
import type { RoutingProfileSnapshot } from "../types/routingProfile";
import { useRoutingProfiles } from "./useRoutingProfiles";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const runningSnapshot: GatewaySnapshot = {
  state: "running",
  address: "127.0.0.1:38440",
  configPath: "C:/gateway",
  pid: 25596,
  instanceId: "inst-1",
  error: null,
};

const stoppedSnapshot: GatewaySnapshot = { ...runningSnapshot, state: "stopped" };

// Mirrors the backend DesktopSnapshot.routingProfiles payload for the deepseek
// profile (sol -> max, terra -> high, luna -> no override). The Luna slot must
// not carry a reasoning key on the wire.
const routingPayload = () => ({
  routingProfiles: {
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
  },
});

const emptyPayload = () => ({
  routingProfiles: { gatewayRunning: true, activeProfileId: "", profiles: [] },
});

async function mountHook(snapshot: GatewaySnapshot): Promise<{
  get: () => ReturnType<typeof useRoutingProfiles>;
  unmount: () => void;
}> {
  let result: ReturnType<typeof useRoutingProfiles> | undefined;
  function Harness() {
    result = useRoutingProfiles(snapshot);
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

describe("useRoutingProfiles", () => {
  beforeEach(() => {
    mocks.command.mockReset();
  });

  it("refreshes on mount and exposes backend-confirmed snapshot state", async () => {
    mocks.command.mockResolvedValueOnce(routingPayload());
    const { get, unmount } = await mountHook(runningSnapshot);

    expect(mocks.command).toHaveBeenCalledWith("LoadRoutingProfiles");
    expect(mocks.command.mock.calls[0]).toHaveLength(1);
    expect(get().activeProfileId).toBe("deepseek");
    expect(get().gatewayRunning).toBe(true);
    expect(get().profiles).toHaveLength(1);
    expect(get().profiles[0].slots.map((s) => s.id)).toEqual(["sol", "terra", "luna"]);
    expect(get().profiles[0].slots[2].reasoning).toBeUndefined();

    unmount();
  });

  it("normalizes an empty activeProfileId to null", async () => {
    mocks.command.mockResolvedValueOnce(emptyPayload());
    const { get, unmount } = await mountHook(runningSnapshot);

    expect(get().activeProfileId).toBeNull();
    expect(get().profiles).toEqual([]);

    unmount();
  });

  it("loads routing profiles even when gateway is stopped (persisted config)", async () => {
    const stoppedPayload = {
      routingProfiles: {
        gatewayRunning: false,
        activeProfileId: "deepseek",
        profiles: [{ id: "deepseek", displayName: "DeepSeek", active: true, configured: true, slots: [] }],
      },
    };
    mocks.command.mockResolvedValueOnce(stoppedPayload);
    const { get, unmount } = await mountHook(stoppedSnapshot);

    expect(mocks.command).toHaveBeenCalledWith("LoadRoutingProfiles");
    expect(get().profiles).toHaveLength(1);
    expect(get().gatewayRunning).toBe(false);
    expect(get().activeProfileId).toBe("deepseek");
    expect(get().error).toBeNull();

    unmount();
  });

  it("activates a profile through the request struct and replaces the snapshot on success", async () => {
    mocks.command.mockResolvedValueOnce(routingPayload());
    const { get, unmount } = await mountHook(runningSnapshot);

    mocks.command.mockResolvedValueOnce(routingPayload());

    let ok: boolean | undefined;
    await act(async () => {
      ok = await get().activateProfile("deepseek");
    });

    expect(ok).toBe(true);
    expect(mocks.command).toHaveBeenCalledWith("ActivateProfile", {
      profileId: "deepseek",
    });
    expect(get().activatingProfileId).toBeNull();
    expect(get().busy).toBe(false);

    unmount();
  });

  it("reports activation errors and keeps the previous snapshot", async () => {
    mocks.command.mockResolvedValueOnce(routingPayload());
    const { get, unmount } = await mountHook(runningSnapshot);
    const before = get().routing;

    mocks.command.mockRejectedValueOnce({
      code: "routing_profile_gateway_not_running",
      message: "Gateway is not running",
    });

    let ok: boolean | undefined;
    await act(async () => {
      ok = await get().activateProfile("deepseek");
    });

    expect(ok).toBe(false);
    expect(get().routing).toBe(before);
    expect(get().activatingProfileId).toBeNull();
    expect(get().commandError).toMatchObject({ code: "routing_profile_gateway_not_running" });
    expect(get().error).toContain("routing_profile_gateway_not_running");

    unmount();
  });

  it("saves a profile and reflects the returned snapshot", async () => {
    mocks.command.mockResolvedValueOnce(routingPayload());
    const { get, unmount } = await mountHook(runningSnapshot);

    mocks.command.mockResolvedValueOnce(routingPayload());

    const input = {
      id: "deepseek",
      displayName: "DeepSeek v2",
      slots: {
        sol: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: "max" as const },
        terra: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: "high" as const },
        luna: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: null },
      },
    };

    let ok: boolean | undefined;
    await act(async () => {
      ok = await get().saveProfile(input);
    });

    expect(ok).toBe(true);
    expect(mocks.command).toHaveBeenCalledWith("SaveRoutingProfile", { profile: input });

    unmount();
  });

  it("saveProfileDetailed returns the canonical backend snapshot", async () => {
    mocks.command.mockResolvedValueOnce(routingPayload());
    const { get, unmount } = await mountHook(runningSnapshot);

    mocks.command.mockResolvedValueOnce(routingPayload());

    const input = {
      id: "deepseek",
      displayName: "DeepSeek v2",
      slots: {
        sol: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: "max" as const },
        terra: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: "high" as const },
        luna: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: null },
      },
    };

    let result: { ok: boolean; snapshot: RoutingProfileSnapshot | null } | undefined;
    await act(async () => {
      result = await get().saveProfileDetailed(input);
    });

    expect(result!.ok).toBe(true);
    expect(result!.snapshot).toEqual(routingPayload().routingProfiles);
    expect(mocks.command).toHaveBeenCalledWith("SaveRoutingProfile", { profile: input });

    unmount();
  });

  it("saveProfileDetailed reports failure with a null snapshot", async () => {
    mocks.command.mockResolvedValueOnce(routingPayload());
    const { get, unmount } = await mountHook(runningSnapshot);

    mocks.command.mockRejectedValueOnce({ code: "routing_profile_save_failed", message: "revision conflict" });

    const input = {
      id: "deepseek",
      displayName: "DeepSeek v2",
      slots: {
        sol: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: "max" as const },
        terra: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: "high" as const },
        luna: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: null },
      },
    };

    let result: { ok: boolean; snapshot: RoutingProfileSnapshot | null } | undefined;
    await act(async () => {
      result = await get().saveProfileDetailed(input);
    });

    expect(result!.ok).toBe(false);
    expect(result!.snapshot).toBeNull();
    expect(get().commandError).toMatchObject({ code: "routing_profile_save_failed" });

    unmount();
  });

  it("keeps the previous snapshot when save fails", async () => {
    mocks.command.mockResolvedValueOnce(routingPayload());
    const { get, unmount } = await mountHook(runningSnapshot);
    const before = get().routing;

    mocks.command.mockRejectedValueOnce({ code: "routing_profile_save_failed", message: "revision conflict" });

    let ok: boolean | undefined;
    await act(async () => {
      ok = await get().saveProfile({
        id: "deepseek",
        displayName: "DeepSeek v2",
        slots: {
          sol: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: "max" },
          terra: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: "high" },
          luna: { provider: "deepseek", upstreamModel: "deepseek-v4-flash", reasoning: null },
        },
      });
    });

    expect(ok).toBe(false);
    expect(get().routing).toBe(before);
    expect(get().commandError).toMatchObject({ code: "routing_profile_save_failed" });

    unmount();
  });
});
