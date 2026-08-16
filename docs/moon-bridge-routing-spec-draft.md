# Moon Bridge ルーティング仕様書（暫定版）

版: 0.2-draft  
作成日: 2026-08-08  
状態: 現在のMoon Bridge Desktop実装・実機smokeに基づく暫定仕様

## 1. 目的

本書はMoon Bridge DesktopがCodex Desktopから受信したResponses系通信を、Capture Proxy、Moon Bridge Gateway、ProviderManager、provider routeへどのように中継・解決するかを整理する。

Codex Desktop自体の通信形式については `codex-desktop-communication-spec-draft.md` を参照する。

## 2. 全体構成

```text
Codex Desktop
    │ openai_base_url
    ▼
Capture Proxy
127.0.0.1:38441
    │
    ├── Traffic Analysis
    │
    ▼
Moon Bridge Gateway
127.0.0.1:38440
    │
    ▼
ProviderManager
    │
    ▼
route / provider / target model
    │
    ▼
Upstream Provider
```

Capture Proxyを上流直接passthroughへ設定する場合はGatewayを経由しない構成も取れる。

## 3. コンポーネント

### 3.1 Codex Desktop

通常のCodex Desktop設定を利用する。Traffic Analysis開始時に `openai_base_url` をCapture Proxyへ一時的に向ける。

### 3.2 Capture Proxy

既定待受:

```text
127.0.0.1:38441
```

役割:

- HTTP relay
- SSE relay
- WebSocket relay
- Traffic Analysis観測
- request/response metadata収集
- Gatewayまたはupstreamへの透過転送

Capture Proxyは**relayとobservationの境界**であり、provider routingの中心ではない。

### 3.3 Moon Bridge Gateway

既定待受:

```text
127.0.0.1:38440
```

役割:

- OpenAI互換request受信
- `/responses` / `/v1/responses` 処理
- Content-Encoding処理
- request model取得
- ProviderManagerによる解決
- protocol adapter / extension適用
- upstream provider呼び出し
- response stream変換

### 3.4 ProviderManager

Moon Bridge既存のConfig Graphに基づいてprovider/model/offer/routeを解決する。Desktop専用の重複provider registryは作らず、元のMoon Bridge構造を再利用する。

## 4. Config Graph

Desktop provider設定も既存Config Graphを使用する。

主な構成:

```text
ProviderDef
ModelDef
OfferEntry
Route
Settings
```

provider activation時は必要なprovider/model/offer/route/default設定を同一グラフへ反映し、Runtime reloadで実行時へ反映する。

## 5. Public alias

Desktopから利用する公開側aliasは論理名:

```text
moonbridge
```

として扱う。provider変更時もCodex Desktop側の概念を増やさず、Moon Bridge内部target routeを切り替える。

## 6. Responses request処理

Gatewayは次をResponses系endpointとして扱う。

```text
POST /responses
POST /v1/responses
```

処理概念:

```text
request
   │
   ├─ Content-Encoding decode
   ├─ JSON parse
   ├─ request model取得
   ├─ model/route解決
   ├─ provider adapter
   └─ upstream
```

zstd request bodyはGateway側でbounded decodeする。

## 7. Request modelと設定modelの違い

Codex設定のtop-level `model` はdefault設定を表すが、実request modelはconversation/sessionレベルで決まる場合がある。

実機で次の不一致が確認された。

```text
config.toml top-level model
        !=
actual POST /responses request model
```

したがって、Traffic routingでは設定ファイルのmodelだけをsource modelの真実とは扱わない。

## 8. Exact request-model mapping

### 8.1 目的

Codex Desktopが送る実request modelを、Moon Bridgeで設定されたtarget routeへ安全に関連付ける。

### 8.2 Lazy bind

Traffic relay開始時点ではtarget routeだけを登録し、source modelは未確定とする。

最初に観測した実 `POST /responses` のrequest modelをsourceとしてlazy bindする。

```text
Traffic Analysis start
    │
    ├─ target route = known
    └─ source model = unbound

first real POST /responses
    │
    ├─ read request model
    └─ bind exact source model
            │
            ▼
exact source model → target route
```

### 8.3 Lifetime

mappingはprocess-localで、Traffic relayのownership / generation / Gateway identityに紐付く。Capture終了時にclearする。

### 8.4 Fail closed

mapping適用条件が一致しない場合、wildcard/default/family fallbackを追加しない。

確認対象:

```text
generation
gateway identity
desktop owner
relay active
exact source model
```

誤providerへ送るより解決失敗を優先する。

## 10. Routing profiles and reasoning policy

The routing-profile extension is authoritative when present: `config.active_profile` is the only active-profile source. The exact picker IDs are `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna`; suffixes and case variants do not match.

The resolved slot keeps its slot ID, active-profile identity, provider, upstream model, mode, and reasoning override through ProviderManager and adapter dispatch. A normal slot sets typed `thinking.type=disabled` and clears only the typed effort field. A thinking slot sets `thinking.type=enabled` and canonicalizes `low|medium|high` to `high` and `xhigh|max` to `max`.

For DeepSeek V4's Anthropic compatibility, `budget_tokens` is not a control field: the provider documents it as accepted but ignored. The supported observable contract is `thinking.type` plus `output_config.effort`. Invalid mode or effort fails before provider conversion and transport.

## 10b. Baseline route（安全基準ルート）

The routing-profile extension additionally holds a global, profile-independent `config.baseline` (top-level, not a 4th profile slot). It is the internal safety reference route, defaulting to `deepseek / deepseek-v4-flash / normal`.

- Mode is fixed `normal` and carries no reasoning override; only `provider` and `upstream_model` are editable (`SaveBaseline`).
- It is deliberately **not** part of `allSlots`/`modelToSlot`: the resolver never routes to it at request time. Slot miss remains fail-closed — there is no runtime fallback to Baseline.
- `SafeResolverState.BaselineState` (`ready`/`missing`/`invalid`) is diagnostic only. It never affects `SlotCount` (3/3) or the Sol/Terra/Luna readiness.
- A missing baseline key in legacy config is lazily canonicalized on read (filled with the default); it is persisted only on the next save, not migrated.

The card-level 「既定」 label was removed; the "default" role is consolidated into Baseline. 確認済み（実装・単体テスト・build済み）.

## 11. Internal observability boundary

After JSON decode, Gateway creates an in-process correlation key. Traffic Analysis aliases it to `req#N` and the active profile to `profile#N`; raw values never enter headers, public APIs, DTOs, autosave, errors, or trace output. Two structured event kinds use the existing bounded ring:

- `routing_resolved`: requested model, slot/profile alias, provider, upstream model, mode, configured effort.
- `provider_request_prepared`: provider/protocol/final model and typed thinking/effective effort after provider conversion.

Trace remains the full-payload diagnostic channel. Traffic Analysis remains the secret-safe user-facing channel.

## 9. Provider resolution

基本方針:

1. 既存ProviderManagerの通常解決を優先。
2. 通常解決が見つからず、Traffic relay用exact mappingが有効な場合だけmappingを評価。
3. mappingのtarget routeを既存ProviderManagerで解決。
4. 解決不能ならfail closed。

```text
request model
    │
    ▼
normal ProviderManager resolution
    │
    ├─ success ───────────────► route
    │
    └─ not found
          │
          ▼
Traffic exact mapping?
          │
          ├─ no ──────────────► fail
          │
          └─ yes
                │
                ▼
           target route
                │
                ▼
          ProviderManager
                │
                ├─ success ───► provider
                └─ fail ──────► fail
```

Traffic mappingはProviderManagerを置き換えるものではない。

## 10. Provider activation

Desktopのprovider設定保存・接続確認はGateway稼働を前提とする。

provider activationでは少なくとも次を整合させる。

```text
provider
model
offer
route
default/public alias
runtime reload
```

設定はrestart後もconfig storeから復元できることを要件とする。

## 11. Streaming

Moon Bridgeはupstream providerからのstreaming responseをCodex Desktop側の期待するResponses streamへ中継・変換する。

最新実機ではCodex Desktop → Capture ProxyがPOST、responseがSSEとなる経路を確認している。

routing層は次を壊してはならない。

- HTTP status
- stream開始
- SSE event sequence
- connection lifetime
- cancellation
- error propagation

WebSocket経路でもCapture Proxyはcontrol frameを含む透過性を維持する。

## 12. Capture Proxyの状態

### 12.1 Capturing

```text
state = capturing
```

観測しながらrelayする。

- HTTP/SSE/WebSocket relay
- safe metadata observation
- payload classification
- bounded JSON shape解析
- identifier匿名化
- ring buffer
- autosave用安全DTO

### 12.2 Passthrough

```text
state = passthrough
```

通信のみ継続し、新たなpayload解析を止める。

維持:

- listener
- existing/new relay
- required headers
- raw body relay
- WebSocket control frames

停止:

- payload analysis
- HMAC analysis
- JSON analysis
- SSE/WebSocket message observation
- ring buffer追加

### 12.3 Stopped / Idle

Capture Proxyを閉じ、Traffic mapping等のprocess-local stateを解放する。

## 13. Traffic Analysisの転送不変条件

観測のために通信内容を書き換えない。

```text
bytes received from Codex
        │
        ├─ observation copy
        │
        └─ relay bytes ──► destination
```

Traffic Analysisでzstd展開を追加する場合も、展開対象は観測用コピーだけとする。forwardされるbyte列、Content-Encoding、header意味論は変更しない。

## 14. Traffic Analysis安全境界

保存可能:

```text
direction
timestamp
sequence
transport
method
status
safe Content-Encoding classification
raw/decoded size
payload kind
bounded shape
anonymous identifier relationship
allowlisted numeric usage
event type
```

保存禁止:

```text
Authorization value
Cookie value
API key
prompt
assistant text
system/developer instructions
source code
tool arguments
tool output
raw body
decoded body
complete opaque/encrypted content
raw IDs
```

## 15. zstd and safe observation

### 15.1 Gateway routing

GatewayのResponses request処理はzstdをbounded decodeし、routingに必要なrequest JSON/modelを取得する。Gateway側のdecodeはprovider routingのための処理であり、Capture Proxyのforward bytesを変更しない。

### 15.2 Traffic Analysis observation copy

Traffic Analysisの観測コピーでは、bounded zstd decodeに成功した場合、展開後JSONを既存のshape/identifier sanitizerへ渡す。

```text
content_encoding: zstd
payload_kind: json
decoding_status: decoded
```

Observationへ残るのはbounded shape、tool分類、event/object/status分類、SSE event type、session-global `id#N` aliasによる匿名ID分類だけである。nested `response.id` / `item.id`は親pathでbucketに分類し、`call_id`と`previous_response_id`は専用bucketへ分離する。namespace未確認のnested `id`はOtherIDへ残す。外部出力はaliasのみであり、raw ID、HMAC digestはDTO/autosave/frontendに出力しない。`InputItemFingerprint`内のidentifiersも同一registryでalias化する。

decoded body、raw ID、prompt、tool arguments/output、source code、opaque/encrypted本文は保存しない。

request `input`は本文を保持せず、input item count、type/role件数、nested object/array counts、alias化済みID分類、previous_response_idの存在分類だけを観測する。usageのSSE event/path/数値allowlistは実機fixtureで確認後に追加するため、現時点では未確定である。

### 15.3 Fail closed

zstd decode失敗、decoded size超過、JSON parse失敗、未知構造の場合はTraffic Analysisだけmetadata-onlyへfallbackする。relay/routing/provider通信は継続する。

```text
malformed zstd
oversized decoded body
malformed JSON
unknown structure
        │
        ▼
observation = metadata-only
relay = continue
```

### 15.4 Forward不変条件

観測用decodeはCapture relayのforward body、Content-Encoding、header意味論を変更しない。Gateway側request decoder、provider routing、Recovery semanticsとは責務を分ける。

### 15.5 テスト1Aからの状態遷移

テスト1A時点ではzstdを識別するmetadata-only表示だった。

```text
content_encoding: zstd
payload_kind: binary
decoding_status: unsupported_encoding
```

現在はdecode成功時に`payload_kind: json` / `decoding_status: decoded`へ更新され、decode失敗時のみ上記metadata-only表示へfallbackする。

## 16. Codex設定transaction

Traffic Analysis開始時はCodex設定の `openai_base_url` をCapture Proxyへ一時変更する。終了時には開始前設定へ復元する。

設定変更はtransactionとして扱い、外部変更との競合を検出する。

設定変更の詳細とRecoveryの扱いは以降の節に従う。

## 17. Recovery

Traffic Analysis実行中にCodex設定が外部変更され、終了時の復元対象と現在値が一致しない場合は勝手に上書きしない。

```text
reconciliation_status = config_conflict
```

としてユーザー確認を要求する。

ユーザーが競合を確認して復元した場合、live Desktop modeでも公式restore経路で以前の設定へ戻し、成功後にrelayを終了する。

Recoveryのownership判定ではlive traffic transaction serviceが保持するowner identityを正とする。restart後などowner evidenceが失われた場合はfail closedする。

## 18. Autosave

Traffic Analysis observationは安全化済みDTOだけをautosaveする。

要件:

- raw payloadを書かない
- raw IDを書かない
- secretを書かない
- incremental write
- stop時flush
- abnormal shutdown時も可能な範囲でfinalize
- bounded retention

ログはprotocol解析用の安全な証拠であり、完全traffic dumpではない。

## 19. 確認済みend-to-end経路

実機smokeで次の基本経路が成立することを確認している。

```text
Codex Desktop
   │ /responses
   ▼
Capture Proxy
   ▼
Moon Bridge Gateway
   ▼
configured route/provider
   ▼
real upstream provider
   ▼
stream response
   ▼
Codex Desktop
```

## 20. 禁止fallback

Traffic exact mappingでは次を導入しない。

```text
wildcard model fallback
model-family fallback
implicit default fallback
unowned mapping reuse
stale generation reuse
stale gateway reuse
```

必要なevidenceが不足すればfail closedする。

## 21. 会話継続とSession Store

### 21.1 HIST-01の結果

5ターン暗号ワード試験（HIST-01 PASS）で、Codex Desktopの通常経路では累積input再送による会話継続が使われることが確認された。各ターンでinput arrayにassistant replyとnew user messageが追加され、全累積inputが毎requestで再送される。

`previous_response_id`はHIST-01の5ターンで使用されなかった。別条件での利用可能性は未確定。

### 21.2 OpenAI adapterのstateless変換

累積inputのstateless変換はadapter unit testでpin済みである。OpenAI adapter (`convertInput`) はrequest内の累積input arrayをそのままCore messagesへ変換し、response IDからのhistory lookupを行わない。

変換で保持されるもの:

- developer/system role → system blocks
- user/assistant message → 順序維持
- function_call / function_call_output → adjacency維持
- reasoning → 既存semantics維持

### 21.3 Session Storeの要否

通常経路では、Codex Desktopがconversation historyをinputに再送するため、Moon Bridge側のSession Store（conversation history resolver）は不要である。

Session Managerはplugin/extension state（DeepSeek thinking replay等）の用途であり、conversation historyのresolveには使っていない。

`previous_response_id`が使用される別条件（WebSocket経路、特定model、Desktop再起動後等）が確認された場合、Session Storeの要否を再評価する。

### 21.4 US-02のinput累積とprefix cache（PASS）

同一会話の2ターン試験で、1ターン目のPOSTは2本とも`input_item_count=5`、2ターン目は`input_item_count=7`（`assistant=1, developer=3, user=3`）となった。前ターンassistant replyと新user messageを含む累積input再送を再確認した。

2ターン目の`response.completed`では`input_tokens=12045`、`cached_input_tokens=12032`、`output_tokens=3`、`total_tokens=12048`を観測した。cached比率は約99.89%で、累積inputを再送しながら上流側prefix cachingが効いている挙動と整合する。

ただし、この観測だけからcacheの具体的な実装方式・課金方式は断定しない。Moon Bridgeは累積inputをstatelessに変換し、usage summaryを安全に観測するが、cache semanticsを独自に解釈しない。

### 21.5 alias-onlyはrouting意味論を変えない

Traffic AnalysisのID出力がalias-only（session-global `id#N`）になったことは、provider routingの意味論を変えない。aliasは観測境界の安全措置であり、provider request/responseの変換には影響しない。

## 22. 未確定事項

- request modelとCodex内部session modelの完全な関係
- `previous_response_id`を他provider向け履歴へどう変換するか
- Codexが毎turnどの程度historyを再送するか
- response/item/call IDのstate mapping
- providerごとのtool call変換
- reasoning event変換
- usage変換
- provider側cache semantics / 課金方式
- context-window管理
- SSE/WebSocket選択条件
- provider固有errorからCodex向けerrorへの完全変換表

## 23. 次の検証

### R-ID-01

最初のresponse IDと次requestの `previous_response_id` が同一か確認。

### R-HIST-01

複数turnでrequest input item count / request sizeを比較し、履歴再送量を見る。

### R-USAGE-01

upstream usageを安全に抽出し、将来のcontext managementに利用可能か確認。

### R-TOOL-01

tool callの `call_id` とtool resultの対応を匿名IDで追跡。

## 24. 設計原則

1. 既存ProviderManager/Config Graphを再利用する。
2. Desktop専用の重複provider体系を作らない。
3. 実requestを設定値より優先して観測する。
4. model mappingはexactかつownedにする。
5. 推測fallbackよりfail closedを優先する。
6. Traffic Analysisはrelay semanticsを変えない。
7. decoded contentを永続化しない。
8. Recoveryはtransaction ownershipを尊重する。
9. protocol不明点は実機観測で確定してから実装する。
10. routingとobservabilityの責務を分離する。

## 25. 更新条件

次を確認したら更新する。

- ~~zstd Traffic Analysis safe decode~~ → 実装済み
- ~~ID相関 (HIST-01)~~ → PASS、累積input再送確認
- ~~alias-only ID出力~~ → 実装済み
- usage schema（実機fixture確定後）
- context accounting
- ~~Session Store要否~~ → 通常経路では不要、別条件は未確定
- tool call state mapping
- provider追加時のgeneric routing検証
- SSE/WebSocket transport条件
