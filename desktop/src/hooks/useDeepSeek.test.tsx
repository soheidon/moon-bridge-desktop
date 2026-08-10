// @vitest-environment jsdom
import { act } from "react";
import { createRoot } from "react-dom/client";
import { beforeEach, describe, expect, it, vi } from "vitest";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const mocks = vi.hoisted(() => ({
  command: vi.fn(),
  onEvent: vi.fn(() => () => undefined),
}));

vi.mock("../platform/desktop", () => ({
  command: mocks.command,
  onEvent: mocks.onEvent,
}));

import type { GatewaySnapshot } from "../types/gateway";
import type { DeepSeekModel, DeepSeekStatus } from "../types/deepseek";
import { DEEPSEEK_FLASH, DEEPSEEK_PRO } from "../types/deepseek";
import { useDeepSeek } from "./useDeepSeek";

const running: GatewaySnapshot = {
  state: "running",
  address: "127.0.0.1:38440",
  configPath: "",
  pid: 1,
  instanceId: "inst-1",
  error: null,
};

function envStatus(selectedModel: DeepSeekModel, apiKeyEnv: string): DeepSeekStatus {
  return {
    gatewayRunning: true,
    providerExists: true,
    apiKeySet: true,
    credentialSource: "environment",
    credentialState: "available",
    configured: true,
    active: true,
    selectedModel,
    reasoningEffort: "high",
    reasoningExplicitlyConfigured: true,
    allowedReasoningEfforts: ["high", "max"],
    routeAlias: "moonbridge",
    defaultModel: selectedModel === DEEPSEEK_FLASH ? "flash" : "pro",
    apiKeyEnv,
    pro: { modelId: DEEPSEEK_PRO, reasoning: "high", supported: ["high", "max"] },
    flash: { modelId: DEEPSEEK_FLASH, reasoning: "high", supported: ["low", "high", "max"] },
  };
}

function renderUseDeepSeek() {
  let result: ReturnType<typeof useDeepSeek> | undefined;
  function Harness() {
    result = useDeepSeek(running);
    return null;
  }
  const container = document.createElement("div");
  const root = createRoot(container);
  act(() => {
    root.render(<Harness />);
  });
  return { result: () => result!, root };
}

// flushAsync drains pending microtask state updates (the mount refresh's async
// continuation) so the harness reflects the loaded snapshot.
async function flushAsync() {
  await act(async () => {
    await new Promise<void>((resolve) => {
      setTimeout(resolve, 0);
    });
  });
}

describe("useDeepSeek activateModel", () => {
  beforeEach(() => {
    mocks.command.mockReset();
    // The mocked command resolves to the unwrapped value (the real command
    // strips the {ok, value} envelope), matching the hook's typed call sites.
    mocks.command.mockImplementation(async (name: string) => {
      if (name === "LoadDeepSeekSettings") {
        return { deepseek: envStatus(DEEPSEEK_FLASH, "OLD_DEEPSEEK_KEY") };
      }
      if (name === "SaveDeepSeekSettings") {
        return { deepseek: envStatus(DEEPSEEK_PRO, "NEW_DEEPSEEK_KEY") };
      }
      throw new Error(`unexpected command: ${name}`);
    });
  });

  it("uses the latest apiKeyEnv after it changes, not a stale closure", async () => {
    const { result, root } = renderUseDeepSeek();
    // Flush the mount refresh so the loaded env var and model land.
    await flushAsync();

    expect(result().model).toBe(DEEPSEEK_FLASH);
    expect(result().apiKeyEnv).toBe("OLD_DEEPSEEK_KEY");

    // User edits the env-var name; model switch must send the new name.
    act(() => result().setApiKeyEnv("NEW_DEEPSEEK_KEY"));
    expect(result().apiKeyEnv).toBe("NEW_DEEPSEEK_KEY");
    let ok: boolean | undefined;
    await act(async () => {
      ok = await result().activateModel(DEEPSEEK_PRO);
    });
    root.unmount();

    expect(ok).toBe(true);
    const saveCalls = mocks.command.mock.calls.filter(([name]) => name === "SaveDeepSeekSettings");
    expect(saveCalls).toHaveLength(1);
    const input = saveCalls[0][1] as { apiKeyEnv?: string };
    expect(input.apiKeyEnv).toBe("NEW_DEEPSEEK_KEY");
  });
});
