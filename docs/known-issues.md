# 既知の問題（Known Issues）

この文書は、Moon Bridge の現時点で判明している既知の問題と制約を正本として記録する。
親計画 `docs/plans/plan-routing-observability-and-thinking-correctness.md` の受入判断と
テスト分類（D = 環境依存・判定不能）の根拠もここに集約する。

## 現在の正式サポート範囲

- 2026-08-11 時点の正式サポートは **DeepSeek による Sol／Terra／Luna routing**。
- 自動 smoke と実 Desktop からの手動 smoke の両方で PASS 済み（同一 request alias で
  `routing_resolved` → `provider_request_prepared` → `payload` → `response.completed` が相関）。

## 既知の問題・制約

### 1. MultiplePlugins E2E（未解決）

- **状態**: 未解決。
- **影響**: 複数の plugin を同一 request に適用する動作は未保証。実行順序の変化、
  後続 plugin が呼ばれない、途中の model 変更による適用判定の変化などの可能性がある。
- **現在の利用範囲**: DeepSeek 単独経路では問題を確認していない。
- **対応**: 複数 plugin 対応を正式に進める際に調査する。

### 2. Google tool-use（未対応）

- **状態**: 未対応（不具合ではなく現時点のサポート対象外）。
- **影響**: Google GenAI provider の tool-use round trip は保証しない。
- **対応**: Google provider 実装時にテストとともに対応する。

### 3. Windows race 検証（検証環境の制約）

- **状態**: 検証環境の制約（製品バグの検出ではない）。
- **現象**: `go test -race` の binary は compile 成功するが、起動前に `0xC0000139`
  （STATUS_ENTRYPOINT_NOT_FOUND）で終了し、`=== RUN` に到達しない。
- **意味**: race が検出されたのではなく、race 検査自体を実行できていない。
- **代替確認**: 通常テスト（全 package 20 回）と並行性関連テスト（50 回）は PASS。
  ただしこれは代替証拠であり、**race PASS ではない**。
- **対応**: Windows の Go/runtime 環境を別途整備して再検証する。

### 4. Placeholder provider

- **状態**: 未実装。
- **対象**: MiniMax／Kimi／MiMo／OpenRouter は placeholder。
- **現時点の正式サポート**: DeepSeek のみ。

## 受入判断

親計画の受入分類（D = 環境依存・判定不能）に該当する上記の項目は、DeepSeek 限定スコープでは
リリース非阻害事項として扱う。判断の記録は `docs/plans/plan-routing-observability-and-thinking-correctness.md`
の Decision Log を参照。
