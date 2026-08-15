import type { GatewaySnapshot } from "../types/gateway";
import type { useGateway } from "../hooks/useGateway";
import type { useDeepSeek } from "../hooks/useDeepSeek";
import type { useRoutingProfiles } from "../hooks/useRoutingProfiles";
import type { useTrafficAnalysis } from "../hooks/useTrafficAnalysis";
import type { AppPage } from "./Header";
import { GatewayStatusPanel } from "./GatewayStatusPanel";
import { RoutingProfilePanel } from "./RoutingProfilePanel";
import { ProcessLogPanel } from "./ProcessLogPanel";
import { DeepSeekCard } from "./DeepSeekCard";
import { TrafficAnalysisCard } from "./TrafficAnalysisCard";

type Props = {
  page: AppPage;
  snapshot: GatewaySnapshot;
  gateway: ReturnType<typeof useGateway>;
  deepseek: ReturnType<typeof useDeepSeek>;
  routing: ReturnType<typeof useRoutingProfiles>;
  traffic: ReturnType<typeof useTrafficAnalysis>;
};

export function AppPages({ page, snapshot, gateway, deepseek, routing, traffic }: Props) {
  if (page === "settings") {
    return (
      <main className="settings-page">
        <div className="settings-header"><h2 className="settings-header-title">設定</h2></div>
        <section className="settings-block provider-settings-block" aria-labelledby="api-keys-title">
          <div className="settings-block-header">
            <h2 id="api-keys-title">APIキー</h2>
          </div>
          <div className="provider-settings-header" aria-hidden="true">
            <span />
            <span>Provider</span>
            <span>Env Var</span>
            <span>Status</span>
          </div>
          <div className="provider-settings-list">
            <DeepSeekCard snapshot={snapshot} deepseek={deepseek} routing={routing} />
            {[
              ["MiniMax", "MINIMAX_API_KEY"],
              ["Kimi", "MOONSHOT_API_KEY"],
              ["MiMo", "XIAOMI_API_KEY"],
              ["OpenRouter", "OPENROUTER_API_KEY"],
            ].map(([provider, envVar]) => (
              <div className="provider-settings-placeholder" key={provider}>
                <span className="provider-summary-name">{provider}</span>
                <span className="provider-summary-env">{envVar}</span>
                <span className="deepseek-state muted">未設定</span>
              </div>
            ))}
          </div>
        </section>
      </main>
    );
  }
  if (page === "traffic") {
    return (
      <main className="settings-page">
        <TrafficAnalysisCard traffic={traffic} />
      </main>
    );
  }
  return (
    <main className="app-main dashboard">
      <div className="dashboard-content">
        <RoutingProfilePanel routing={routing} runtime={snapshot.runtimeConfiguration} />
        <GatewayStatusPanel snapshot={snapshot} error={gateway.error} />
      </div>
      <ProcessLogPanel logs={gateway.logs} trafficEvents={traffic.runtimeEvents} />
    </main>
  );
}
