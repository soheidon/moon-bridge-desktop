import { describe, expect, it } from "vitest";
import { trafficActionDisabled } from "./trafficAnalysisActions";

describe("traffic action availability", () => {
  it("keeps stop enabled while capture has no observations", () => {
    expect(trafficActionDisabled(0, {})).toMatchObject({ stop: false, clear: true });
  });

  it("enables clear during capture when observations exist", () => {
    expect(trafficActionDisabled(1, {})).toMatchObject({ stop: false, clear: false });
  });

  it("disables only the operation that is currently running", () => {
    expect(trafficActionDisabled(1, { clearing: true })).toMatchObject({ clear: true });
    expect(trafficActionDisabled(1, { stopping: true })).toMatchObject({ stop: true, clear: false });
    expect(trafficActionDisabled(1, { finalizing: true })).toMatchObject({ finalize: true, stop: false, clear: false });
  });

  it("disables finalize while stop is still running", () => {
    expect(trafficActionDisabled(1, { stopping: true })).toMatchObject({ finalize: true });
    expect(trafficActionDisabled(1, { stopping: true, finalizing: true })).toMatchObject({ finalize: true });
    expect(trafficActionDisabled(1, {})).toMatchObject({ finalize: false });
    expect(trafficActionDisabled(1, { finalizing: true })).toMatchObject({ finalize: true });
  });
});
