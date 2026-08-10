import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  command: vi.fn(),
}));

vi.mock("./platform/desktop", () => ({
  command: mocks.command,
  onEvent: vi.fn(() => () => undefined),
}));

import type { GatewayLog } from "./types/gateway";
import { appendLog, resolveStartError, resolveStopError, toGatewaySnapshot, useGateway } from "./hooks/useGateway";

function renderGatewayHook(): () => ReturnType<typeof useGateway> {
  let result: ReturnType<typeof useGateway> | undefined;
  function Harness() {
    result = useGateway();
    return null;
  }
  renderToStaticMarkup(<Harness />);
  return () => result!;
}

describe("appendLog", () => {
  const log = (line: string): GatewayLog => ({ stream: "stderr", line, timestamp: "2026-08-07T06:13:54.426+09:00" });

  it("appends the next log to the end of the current list", () => {
    expect(appendLog([log("a"), log("b")], log("c"))).toEqual([log("a"), log("b"), log("c")]);
  });

  it("caps the list at 500 entries, dropping the oldest", () => {
    const current = Array.from({ length: 500 }, (_, i) => log(`line-${i}`));
    const next = appendLog(current, log("line-500"));
    expect(next).toHaveLength(500);
    expect(next[0].line).toBe("line-1");
    expect(next[499].line).toBe("line-500");
  });

  it("returns an empty list when the cap is not positive", () => {
    expect(appendLog([log("a")], log("b"), 0)).toEqual([]);
  });
});

describe("toGatewaySnapshot", () => {
  it("maps a flat GatewayStatus value into the Gateway UI snapshot", () => {
    expect(
      toGatewaySnapshot({
        state: "running",
        address: "127.0.0.1:38440",
        configPath: "C:/gateway",
        pid: 25596,
        instanceId: "inst-1",
        error: null,
      }),
    ).toEqual({
      state: "running",
      address: "127.0.0.1:38440",
      configPath: "C:/gateway",
      pid: 25596,
      instanceId: "inst-1",
      error: null,
    });
  });

  it("defaults to stopped and empty fields when the flat value omits them", () => {
    expect(toGatewaySnapshot({})).toEqual({
      state: "stopped",
      address: "",
      configPath: "",
      pid: null,
      instanceId: null,
      error: null,
    });
  });

  it("reads state from the flat value, not a nested gateway wrapper", () => {
    expect(toGatewaySnapshot({ state: "running" }).state).toBe("running");
  });
});

describe("resolveStartError", () => {
  it("surfaces the start error when the gateway is stopped", () => {
    expect(resolveStartError({ code: "gateway_config_load_failed", message: "unable to load config" }, "stopped")).toEqual(
      expect.objectContaining({ code: "gateway_config_load_failed", message: "unable to load config" }),
    );
  });

  it("treats a failed start as success when the gateway is running", () => {
    expect(resolveStartError({ code: "gateway_config_load_failed" }, "running")).toBeNull();
  });

  it("reports a state mismatch when the command succeeded but the gateway is not running", () => {
    expect(resolveStartError(null, "stopped")).toMatchObject({ code: "gateway_start_state_mismatch" });
  });

  it("treats a successful start with a running gateway as success", () => {
    expect(resolveStartError(null, "running")).toBeNull();
  });
});

describe("resolveStopError", () => {
  it("surfaces the stop error when the gateway is still running", () => {
    expect(resolveStopError({ code: "gateway_stop_failed", message: "stop failed" }, "running")).toEqual(
      expect.objectContaining({ code: "gateway_stop_failed", message: "stop failed" }),
    );
  });

  it("treats a failed stop as success when the gateway is stopped", () => {
    expect(resolveStopError({ code: "gateway_stop_failed" }, "stopped")).toBeNull();
  });

  it("reports a state mismatch when the command succeeded but the gateway is still running", () => {
    expect(resolveStopError(null, "running")).toMatchObject({ code: "gateway_stop_state_mismatch" });
  });

  it("treats a successful stop with a stopped gateway as success", () => {
    expect(resolveStopError(null, "stopped")).toBeNull();
  });
});

describe("useGateway command dispatch", () => {
  beforeEach(() => {
    mocks.command.mockReset();
    mocks.command.mockResolvedValue({});
  });

  it("refreshes GatewayStatus after StartGateway so the UI reflects the authoritative snapshot", async () => {
    const getGateway = renderGatewayHook();
    mocks.command
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({
        state: "running",
        address: "127.0.0.1:38440",
        configPath: "C:/gateway",
        pid: 25596,
        instanceId: "inst-1",
        error: null,
      });

    await getGateway().start();

    expect(mocks.command).toHaveBeenNthCalledWith(1, "StartGateway", { configPath: "" });
    expect(mocks.command).toHaveBeenNthCalledWith(2, "GatewayStatus");
  });

  it("passes StopGateway as a flat empty struct and refreshes status", async () => {
    const getGateway = renderGatewayHook();

    await getGateway().stop();

    expect(mocks.command).toHaveBeenNthCalledWith(1, "StopGateway", {});
    expect(mocks.command).toHaveBeenNthCalledWith(2, "GatewayStatus");
  });

  it("re-checks the authoritative status when StartGateway fails instead of swallowing the failure", async () => {
    const getGateway = renderGatewayHook();
    mocks.command
      .mockRejectedValueOnce({ code: "gateway_config_load_failed", message: "unable to load config" })
      .mockResolvedValueOnce({ state: "stopped" });

    await getGateway().start();

    expect(mocks.command).toHaveBeenNthCalledWith(1, "StartGateway", { configPath: "" });
    expect(mocks.command).toHaveBeenNthCalledWith(2, "GatewayStatus");
  });

  it("invokes GatewayStatus with no arguments on refresh", async () => {
    const getGateway = renderGatewayHook();

    await getGateway().refresh();

    expect(mocks.command).toHaveBeenCalledWith("GatewayStatus");
    expect(mocks.command.mock.calls[0]).toHaveLength(1);
  });
});
