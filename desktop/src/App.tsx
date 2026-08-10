import "./App.css";
import { useEffect, useState } from "react";
import { Header, type AppPage } from "./components/Header";
import { AppPages } from "./components/AppPages";
import { TrafficExitDialog } from "./components/TrafficAnalysisCard";
import { useGateway } from "./hooks/useGateway";
import { useTrafficAnalysis } from "./hooks/useTrafficAnalysis";
import { useDeepSeek } from "./hooks/useDeepSeek";
import { useRoutingProfiles } from "./hooks/useRoutingProfiles";

export default function App() {
  const [page, setPage] = useState<AppPage>("dashboard");
  const [gatewayWarning, setGatewayWarning] = useState<string | null>(null);
  const gateway = useGateway();
  const snapshot = gateway.snapshot ?? {
    state: "stopped" as const, address: "127.0.0.1:38440", configPath: "", pid: null, instanceId: null, error: null,
  };
  const deepseek = useDeepSeek(snapshot);
  const routing = useRoutingProfiles(snapshot);
  const traffic = useTrafficAnalysis();

  // DeepSeekカードが設定ページに移っても、Gatewayが残存する失敗の警告を
  // ヘッダーへ出し続ける（hook は全ページで生きている）。
  useEffect(() => {
    setGatewayWarning(deepseek.commandError?.gatewayLeftRunning ? deepseek.commandError.message : null);
  }, [deepseek.commandError]);

  return (
    <div className="app">
      <Header snapshot={snapshot} busy={gateway.busy} page={page} onNavigate={setPage} onStart={() => void gateway.start()} onStop={() => {
        if (traffic.status?.integrationActive) setGatewayWarning("Traffic Analysis実行中です。先に分析を停止してください。");
        else if (traffic.status?.relayActive) setGatewayWarning("Capture Proxyが中継中です。先に中継を終了してください。");
        else void gateway.stop();
      }} gatewayWarning={gatewayWarning} />
      <AppPages page={page} snapshot={snapshot} gateway={gateway} deepseek={deepseek} routing={routing} traffic={traffic} />
      {traffic.exitPrompt && <TrafficExitDialog traffic={traffic} payload={traffic.exitPrompt} />}
    </div>
  );
}
