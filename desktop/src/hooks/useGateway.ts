import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import { useCallback, useEffect, useState } from "react";
import type { GatewayLog, GatewaySnapshot } from "../types/gateway";

export function useGateway() {
  const [snapshot, setSnapshot] = useState<GatewaySnapshot | null>(null);
  const [logs, setLogs] = useState<GatewayLog[]>([]);
  const [busy, setBusy] = useState(false);

  const refresh = useCallback(async () => {
    setSnapshot(await invoke<GatewaySnapshot>("gateway_status"));
  }, []);

  useEffect(() => {
    let disposed = false;
    let unlisten: UnlistenFn | undefined;
    void refresh();
    void listen<GatewayLog>("gateway-log", (event) => {
      if (disposed) return;
      setLogs((current) => [...current.slice(-499), event.payload]);
      void refresh();
    }).then((fn) => {
      if (disposed) fn();
      else unlisten = fn;
    });
    return () => {
      disposed = true;
      unlisten?.();
    };
  }, [refresh]);

  const start = useCallback(async () => {
    setBusy(true);
    try { setSnapshot(await invoke<GatewaySnapshot>("gateway_start")); }
    catch { await refresh(); }
    finally { setBusy(false); }
  }, [refresh]);

  const stop = useCallback(async () => {
    setBusy(true);
    try { setSnapshot(await invoke<GatewaySnapshot>("gateway_stop")); }
    catch { await refresh(); }
    finally { setBusy(false); }
  }, [refresh]);

  const openConfigFolder = useCallback(() => {
    void invoke("open_config_folder");
  }, []);

  return { snapshot, logs, busy, refresh, start, stop, openConfigFolder };
}
