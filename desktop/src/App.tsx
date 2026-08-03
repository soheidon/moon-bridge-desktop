import "./App.css";
import { Header } from "./components/Header";
import { GatewayStatusPanel } from "./components/GatewayStatusPanel";
import { ProcessLogPanel } from "./components/ProcessLogPanel";
import { DeepSeekCard } from "./components/DeepSeekCard";
import { TrafficAnalysisCard } from "./components/TrafficAnalysisCard";
import { useGateway } from "./hooks/useGateway";
import { useTrafficAnalysis } from "./hooks/useTrafficAnalysis";
import { useDeepSeek } from "./hooks/useDeepSeek";
import { useState } from "react";

export default function App() {
  const gateway = useGateway();
  const snapshot = gateway.snapshot ?? {
    state: "stopped" as const, address: "127.0.0.1:38440", configPath: "", pid: null, instanceId: null, error: null,
  };
  const deepseek = useDeepSeek(snapshot);
  const traffic = useTrafficAnalysis();
  const [gatewayWarning, setGatewayWarning] = useState<string | null>(null);
  return (
    <div className="app">
      <Header snapshot={snapshot} busy={gateway.busy} onStart={() => void gateway.start()} onStop={() => {
        if (traffic.status?.integrationActive) setGatewayWarning("Traffic Analysis実行中です。先に分析を停止してください。");
        else if (traffic.status?.relayActive) setGatewayWarning("Capture Proxyが中継中です。先に中継を終了してください。");
        else void gateway.stop();
      }} gatewayWarning={gatewayWarning} />
      <main className="app-main dashboard">
        <GatewayStatusPanel snapshot={snapshot} onOpenConfigFolder={gateway.openConfigFolder} />
        <DeepSeekCard snapshot={snapshot} deepseek={deepseek} onGatewayWarning={setGatewayWarning} />
        <TrafficAnalysisCard traffic={traffic} />
        <ProcessLogPanel logs={gateway.logs} />
      </main>
    </div>
  );
}
