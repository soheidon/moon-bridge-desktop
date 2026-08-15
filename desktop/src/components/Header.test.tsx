import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { getNavigationTarget, Header, type AppPage } from "./Header";

const snapshot = { state: "stopped" as const, address: "", configPath: "", pid: null, instanceId: null, error: null };

function headerMarkup(page: AppPage) {
  return renderToStaticMarkup(
    <Header snapshot={snapshot} busy={false} onStart={() => undefined} onStop={() => undefined} page={page} onNavigate={() => undefined} />,
  );
}

describe("getNavigationTarget", () => {
  it("always returns dashboard for the dashboard target", () => {
    expect(getNavigationTarget("traffic", "dashboard")).toBe("dashboard");
    expect(getNavigationTarget("settings", "dashboard")).toBe("dashboard");
  });

  it("toggles back to dashboard when the active sub-page is clicked again", () => {
    expect(getNavigationTarget("traffic", "traffic")).toBe("dashboard");
    expect(getNavigationTarget("settings", "settings")).toBe("dashboard");
  });

  it("opens a sub-page from the dashboard", () => {
    expect(getNavigationTarget("dashboard", "traffic")).toBe("traffic");
    expect(getNavigationTarget("dashboard", "settings")).toBe("settings");
  });

  it("moves directly between sub-pages", () => {
    expect(getNavigationTarget("traffic", "settings")).toBe("settings");
    expect(getNavigationTarget("settings", "traffic")).toBe("traffic");
  });
});

describe("Header navigation", () => {
  it("renders the 設定 button and Codex Traffic Analysis tab without a title button", () => {
    const markup = headerMarkup("dashboard");
    expect(markup).not.toContain('class="app-title">Moon Bridge Desktop</button>');
    expect(markup).toContain('class="header-nav-tab">Codex Traffic Analysis</button>');
    expect(markup).toContain('class="btn btn-settings">設定</button>');
  });

  it("places the version info to the left of the Codex Traffic Analysis tab", () => {
    const markup = headerMarkup("dashboard");
    const versionIndex = markup.indexOf("v0.3.0");
    const tabIndex = markup.indexOf("Codex Traffic Analysis</button>");
    expect(versionIndex).toBeGreaterThanOrEqual(0);
    expect(versionIndex).toBeLessThan(tabIndex);
  });

  it("marks the settings button active and labels it 設定を閉じる on the settings page", () => {
    const markup = headerMarkup("settings");
    expect(markup).toContain('class="btn btn-settings active" aria-current="page">設定を閉じる</button>');
    expect(markup).toContain('class="header-nav-tab">Codex Traffic Analysis</button>');
    expect(markup).not.toContain('aria-current="page">Codex');
  });

  it("marks the traffic tab active only on the traffic page", () => {
    const markup = headerMarkup("traffic");
    expect(markup).toContain('class="header-nav-tab active" aria-current="page">Codex Traffic Analysis</button>');
    expect(markup).toContain('class="btn btn-settings">設定</button>');
    expect(markup).not.toContain('aria-current="page">設定');
  });

  it("leaves both controls inactive on the dashboard", () => {
    const markup = headerMarkup("dashboard");
    expect(markup).not.toContain("tab-active");
    expect(markup).not.toContain("btn-settings active");
    expect(markup).not.toContain("aria-current");
  });
});
