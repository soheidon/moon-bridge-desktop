# Codex Desktop 通信仕様書（暫定版）

版: 0.2-draft  
作成日: 2026-08-08  
状態: 実機観測およびMoon Bridge Traffic Analysis実装に基づく暫定仕様

## 1. 目的

本書は、通常のCodex DesktopがChatGPTサブスクリプション認証を利用して行う通信について、Moon Bridgeでの実機観測から現時点で判明している仕様を整理する。

確度は次の3段階で区別する。

- **確認済み**: 実機通信または実装テストで直接確認。
- **暫定解釈**: 観測結果から強く示唆されるが追加試験が必要。
- **未確定**: 今後のTraffic Analysis試験で確認するもの。

Moon Bridge内部のprovider解決・ルーティング・Capture Proxy制御は `moon-bridge-routing-spec-draft.md` を参照する。

## 2. 対象

対象は通常のCodex Desktopである。独立した `CODEX_HOME` や検証専用CLIを前提とせず、普段利用しているCodex Desktopの `openai_base_url` を一時的にMoon Bridge Capture Proxyへ向けて通信を観測する。

認証はCodex Desktopが既に保持しているChatGPT側認証を使用する。Moon Bridge Traffic AnalysisはAuthorization、Cookie、API keyを保存・表示しない。

## 3. ネットワーク構成

通常の上流観測:

```text
Codex Desktop
    │ openai_base_url
    ▼
Moon Bridge Capture Proxy
127.0.0.1:38441
    │ HTTP / SSE / WebSocket relay
    ▼
Codex upstream
```

Moon Bridge Gatewayへルーティングする場合:

```text
Codex Desktop
    ▼
Capture Proxy :38441
    ▼
Moon Bridge Gateway :38440
    ▼
configured provider
```

## 4. 推論要求の主要経路

### 4.1 Responses系endpoint

推論要求では `/responses` 系の経路が使用されることを確認している。Moon Bridge側では `/responses` および `/v1/responses` をResponses系要求として処理する。

### 4.2 最新実機観測: POST + SSE

2026-08-08の最新実機観測では、少なくとも確認セッションにおいて次の構成が確認された。

```text
POST /responses
Content-Encoding: zstd
        │
        ▼
HTTP 200
Content-Type: text/event-stream
        │
        ▼
多数のSSEイベント
```

同一セッションで複数のzstd POSTと、それぞれに対応する多数のSSEイベントが観測された。したがって、現在の実機ではPOST request + SSE response streamが主要推論経路として使われる場合がある。

### 4.3 WebSocket

過去の実機観測ではResponses系でWebSocket通信も確認されている。多数の細粒度JSONメッセージ、ping/pong、継続接続、toolを伴う処理での利用例がある。

ただし最新観測ではPOST + SSEが使用されているため、Codex DesktopのResponses通信は単一transportに固定されているとはみなさない。HTTP/SSEとWebSocketの双方を互換対象とし、選択条件は未確定とする。

### 4.4 GET /responses ポーリング（確認済み 2026-08-16）

実機でCodex Desktopが `/responses` へGETを繰り返すことを観測した。Moon Bridge Gatewayはこれに
405 Method Not Allowed（"方法不允许"）を返すが、続くPOST /responsesは正常に処理され、upstream応答まで
返る。このGETは推論要求ではなく、POST経路を妨げないため現行実装では許容される。

## 5. Request Content-Encoding

### 5.1 zstd

最新実機観測では、推論POST requestに次が確認された。

```text
content_encoding: zstd
payload_kind: binary
decoding_status: unsupported_encoding
```

この `unsupported_encoding` はCodex通信自体が未対応という意味ではなく、現時点のTraffic Analysisがzstd bodyをmetadata-onlyとして扱っていることを示す。

Gateway側にはzstd展開処理が存在し、Responses requestのroutingに必要なJSONを読み取れる。

### 5.2 zstdの安全構造観測（実装済み）

Traffic Analysisはzstd bodyをメモリ上でbounded decodeし、安全な構造情報のみを抽出する。decoded bodyそのものは永続化しない。

```text
content_encoding: zstd
payload_kind: json
decoding_status: decoded
```

zstd decode失敗・decoded size超過・JSON parse失敗時は、Traffic Analysisだけmetadata-onlyへfallbackする。Capture relayのforward byte列、Content-Encoding、routing/provider通信は変更しない。

## 6. Request JSON構造

確認済みのトップレベルフィールド:

```text
include
input
model
parallel_tool_calls
reasoning
store
stream
type
```

## 7. 会話継続方式

### 7.1 HIST-01実機試験結果（PASS）

5ターンの暗号ワード試験で、Codex Desktopの会話継続方式を実機確認した。各ターンでPOST requestの`input_item_count`は次の推移を示した。

```text
Turn 1:  input 5 items  (developer + user + 3 structure)
Turn 2:  input 7 items  (+ assistant reply + new user message)
Turn 3:  input 9 items  (+ assistant reply + new user message)
Turn 4:  input 11 items (+ assistant reply + new user message)
Turn 5:  input 13 items (+ assistant reply + new user message)
```

各ターンのPOSTでtop-level `previous_response_id`は観測されなかった。

### 7.2 通常経路の継続方式: 累積input再送

HIST-01の結果から、Codex Desktopの通常経路では次の方式が使われることが確認された。

- 各ターンでassistant replyとnew user messageをinput arrayの末尾に追加する
- 全累積inputを毎requestで再送する
- `previous_response_id`は使用しない（少なくとも今回の観測セッションでは）
- response IDからconversation historyをlookupするserver-side stateはこの経路では不要

この方式は、Moon BridgeのOpenAI adapterがstatelessに変換できることをadapter unit testでpin済みである。

### 7.3 previous_response_id

`previous_response_id`が送信される例が過去の実機観測にあるが、HIST-01の5ターン試験では使用されなかった。別条件（例: WebSocket経路、特定のmodel、Desktop再起動後）での利用可能性は未確定とする。

### 7.4 response.idの扱い

HIST-01の観測で、複数POST・複数ターンの`response_id_aliases`に同一alias値が再出現した。response IDがcall単位で常に一意という前提は成立しない。Traffic Analysisでは同一aliasが複数ターンで出現することを許容する。

### 7.5 US-02実機試験結果（PASS）

固定応答`Reply with exactly: OK`を2ターン実施した。

- 1ターン目は内部処理を含むPOSTが2本。両方とも`input_item_count=5`。
- 2ターン目は`input_item_count=7`へ増加し、`assistant=1, developer=3, user=3`。前ターンassistant replyと新しいuser messageを含む累積input再送を再確認した。
- 1ターン目のusageは`12033/4736`、`5653/4608`（input/cached）。
- 2ターン目は`input_tokens=12045`、`cached_input_tokens=12032`、`output_tokens=3`、`total_tokens=12048`。cached比率は約`99.89%`。
- 未知SSE event typeは固定`unknown`として出力され、digestは出力されなかった。

この結果は、累積input再送と上流側prefix cachingの挙動に整合する。ただしcacheの具体的な実装方式・課金方式までは本観測から断定しない。


## 8. Response / SSEイベント

1回のPOSTに対し多数のSSEイベントが返ることは確認済み。

次段階の安全観測対象:

```text
event type
response.id
item.id
call_id
usage
event sequence
```

未確定:

- 正確なevent type一覧
- event順序
- response ID発行タイミング
- item IDとresponse IDの階層関係
- tool callイベントの完全順序
- reasoning関連イベント構造
- usageが現れるevent/path

## 9. ID体系

観測対象候補:

```text
response.id (parent path "response")
item.id     (parent path "item")
call_id
conversation-related id
other *_id fields
previous_response_id
```

### 9.1 alias-only出力（実装済み）

Traffic Analysisの外部出力はsession-global `id#N` aliasのみとする。

- 同一Analyzer session内で同一raw ID → 同一`id#N`（bucket横断でも）
- 新しいAnalyzer sessionでは`id#1`から再スタート（cross-session correlation不能）
- raw ID、HMAC digestはDTO/autosave/frontendのいずれにも出力しない
- `json:"-"`タグとsentinel-basedテストでboundaryを固定済み

### 9.2 path-aware分類

nested JSON pathを使ってIDをbucketに分類する。

| 条件 | bucket |
|---|---|
| `previous_response_id` | PreviousResponseID |
| `call_id` | CallID |
| parent = `response` の `id` | ResponseID |
| parent = `item` の `id` | ItemID |
| parent が未確認の nested `id` | OtherID |

`InputItemFingerprint`内のidentifiersも同一registryでalias化する。

## 10. Usage

`response.completed`の`response.usage`から、次の実在fieldを確認済み。

```text
input_tokens
output_tokens
total_tokens
input_tokens_details.cached_tokens
```

US-01/US-02で`reasoning_tokens`は未観測。通常SSE eventにはusageを記録しない。

US-02では2ターン目の`input_tokens=12045`に対して`cached_input_tokens=12032`（約99.89%）を確認した。累積input再送とprefix cachingの利用に整合するが、cacheの具体的な実装方式・課金方式は断定しない。

今後は次の3概念を分離して調査する。

1. response単位のtoken accounting
2. context windowの占有量・残量表示
3. ChatGPT subscription側のquota/rate-limit消費

同一計算式とは仮定しない。

## 11. Tool利用

toolを伴う処理でもResponses系通信が使われる例がある。

未確定:

- tool invocationのevent type
- `call_id`の発行元
- tool resultの関連付け
- tool call前後のresponse/item ID関係
- Desktop側tool executionと上流stateの分担

Traffic Analysisではtool arguments、tool output、source codeを保存しない。

## 12. Traffic Analysis安全境界

保存可能:

```text
transport
direction
timestamp
sequence
method
status code
content encoding classification
raw size
decoded size
payload classification
JSON field names/types
bounded counts
event type
anonymous ID relation
allowlisted usage numbers
```

保存禁止:

```text
Authorization value
Cookie value
API key
user prompt
assistant response text
system/developer instructions
source code
tool arguments
tool output
file content
image content
raw payload
decoded payload
full headers
query values
complete opaque/encrypted content
raw identifiers
HMAC digest (external output)
```

## 13. 現時点の確定状況

| 項目 | 状態 |
|---|---|
| Responses系endpoint | 確認済み |
| POST request | 確認済み |
| zstd request body | 確認済み |
| zstd安全構造観測 | 実装済み |
| SSE response stream | 確認済み |
| WebSocket Responses通信 | 過去実機で確認済み |
| 継続方式: 累積input再送 | HIST-01 PASS（5→7→9→11→13） |
| `previous_response_id` 使用 | HIST-01では未使用、別条件は未確定 |
| `response.id` の一意性 | call単位一意と仮定しない |
| Session Store（conversation history） | 通常経路では不要 |
| ID出力: alias-only | 実装済み（session-global `id#N`） |
| usage抽出 | US-01/US-02 PASS。response.completedからallowlist fieldを抽出 |
| path-aware ID分類 | 実装済み |
| InputItemFingerprint alias化 | 実装済み |
| usage JSON path | response.completedのresponse.usageで確認済み |
| context usage計算式 | 未確定 |
| subscription quota計算 | 未確定 |

## 14. 次の試験

### ID-01 最小応答

```text
Return exactly: OK
```

確認: response ID、item ID、call ID、event type、usage location。

### ID-02 2ターン継続

Turn 1:

```text
Return exactly: OK
```

Turn 2:

```text
What did I ask you to return?
```

匿名HMAC/aliasで `response N.id == request N+1.previous_response_id` を確認する。

### US-01 usage基本構造

最小応答でusage pathとevent typeを確定する。

### US-02 2ターンusageとcache

2ターンの固定短文でinput item累積、input/cached token、cache比率、内部POST差を比較する。実機PASS済み。

### CX-01 継続会話

複数ターンでrequest size、input item count、previous_response_id、usage、context表示の関係を見る。

## 15. 2026-08-08時点の構造観測実装

### 15.1 zstd requestの安全観測

Traffic Analysis観測コピーでは、boundedなzstd decodeに成功した場合、展開後JSONを既存のshape/identifier sanitizerへ渡す。

```text
content_encoding: zstd
payload_kind: json
decoding_status: decoded
```

保存されるのはtop-level field/type、bounded count、tool type/count、event/object/status等のshapeと、既存session-local HMACによる匿名ID分類だけである。decoded body、raw ID、prompt、input text、tool arguments/output、source code、opaque/encrypted本文はObservation、Desktop DTO、autosaveへ保持しない。

zstd decode失敗・decoded size超過・JSON parse失敗時は、Traffic Analysisだけmetadata-onlyへfallbackする。Capture relayのforward byte列、Content-Encoding、routing/provider通信は変更しない。

### 15.2 SSE event type

SSE relayが既に抽出している`event:`分類を、秘密情報を含まない`SSEEventType`としてDesktop DTO/autosaveへ伝搬できる。SSE data JSONは共通shape/identifier sanitizerを通り、response/item/call等のID候補は生値ではなくsession-local HMAC分類になる。

nested `response.id` / `item.id`は親pathを使ってそれぞれResponseIDHMACs / ItemIDHMACsへ分類する。`call_id`はCallIDHMACs、`previous_response_id`は専用PreviousResponseIDHMACsへ分類し、`function_call.id`などnamespace未確認のnested `id`はOtherIDHMACsへ残す。これにより、response IDとprevious response IDをログ上で混同せず比較できる。

### 15.3 継続方式とinput summary

今回のID-02実機観測では、2ターン目POSTのtop-level fieldsに`previous_response_id`が見えなかった。この結果は前ターンのresponse IDとの不一致を意味せず、Codex Desktopが常に同フィールドを使うとも、inputへ履歴を再送したともまだ断定しない。

本文を保存せず、request `input`について次の構造summaryだけを観測する。

```text
input_item_count
input_item_type_counts
input_role_counts
nested object/array counts
HMAC化済みID分類
has_previous_response_id
```

input text/content、prompt、tool arguments/output、source code、file contentは保存しない。

### 15.4 usage

usageの実際のevent type・JSON path・数値フィールドは未確定であり、推測schemaを先行実装しない。

### 15.5 HIST-01実機観測: 累積input再送の確認（PASS）

5ターン暗号ワード試験で、input_item_countが5→7→9→11→13と推移した。各ターンでassistant replyとnew user messageがinput array末尾に追加されるパターンを確認した。全POSTでtop-level `previous_response_id`は観測されなかった。

この結果から、通常経路での会話継続は累積input再送であり、`previous_response_id`参照やserver-side conversation history lookupに依存しないことが確認された。

### 15.6 alias-only ID出力（実装済み）

Traffic Analysisの外部出力はsession-global `id#N` aliasのみとする。HID digestはDTO/autosave/frontendに出力しない。path-aware bucket分類（ResponseID、PreviousResponseID、ItemID、CallID、OtherID）と`InputItemFingerprint`内のidentifiersも同一registryでalias化する。

### 15.7 response.idの再出現

HIST-01の観測で、複数POST・複数ターンの`response_id_aliases`に同一alias値が再出現した。response IDがcall単位で常に一意という前提は成立しない。

### 15.8 usage

`response.completed`のSSE event JSONからusage情報を抽出する。

**JSON path**: `response.usage`を優先、`usage`（top-level）へfallback（既存server semanticsと同一）。

**valid判定**: `input_tokens == 0 && output_tokens == 0 && cached_tokens == 0`のときinvalidとみなし、fallback pathを試行する。`total_tokens`や`reasoning_tokens`だけではvalid判定しない。

**fail-closed**: 各フィールドを`int`型で直接`json.Unmarshal`しているため、fractional・stringified numberが1つでもあるとusage object全体のunmarshalが失敗しnilになる。field-by-field partial recoveryは行わない。

**抽出フィールド（固定allowlist）**:

| フィールド | JSON path |
|---|---|
| input_tokens | `input_tokens` |
| output_tokens | `output_tokens` |
| total_tokens | `total_tokens` |
| cached_input_tokens | `input_tokens_details.cached_tokens` |
| reasoning_tokens | `output_tokens_details.reasoning_tokens` |

**safety bound**: `maxUsageTokens = 1_000_000_000`（上限超過フィールドは無視）。

**presence/absence**: `*int`ポインタフィールドで表現。nil = 未出現。

**test-pinned semantics**:
- malformed field → object全体drop、top-level usageへfallback（`TestExtractUsageMalformedFieldDropsEntireObjectAndFallsBack`）
- response.usage valid判定は`input_tokens/output_tokens/cached_tokens`のみ。`total_tokens/reasoning_tokens`だけではfallbackする（`TestExtractUsageValidityIgnoresTotalAndReasoningTokens`）

**US-01 PASS（実機）**: Codex Desktopの`response.completed` 2 eventから、`input_tokens`、`output_tokens`、`total_tokens`、`cached_input_tokens`をsecret-freeに抽出できた。今回のsessionでは`reasoning_tokens`は未観測。通常SSE eventにはusageを記録しない。

### 15.9 Unknown SSE event type

`protocolEvents`で確認済みのevent typeだけをliteralで出力する。未知event typeは常に固定文字列`unknown`へ分類し、raw event type、HMAC、digest、cross-event correlationを保存・DTO/autosave/frontend出力しない。

## 15.10 Moon Bridge routing observability

The picker model is an exact request field, not a promise that the same name is sent upstream. Moon Bridge resolves it to a routing slot and keeps that provenance through provider conversion. Traffic Analysis may show only the allowlisted requested model, slot, provider/upstream labels, and typed thinking/effort state; it never exposes Codex authorization, cookies, prompt content, tool content, raw bodies, or response IDs. The two internal events are correlated by a session-local `req#N` alias.

The final acceptance path uses a loopback mock provider and the network contract (`POST /responses`, including streaming when selected by the fixture), so an external Codex Desktop session and credential-backed API call are not required.

## 16. 更新条件

次を確認した時点で更新する。

- usage実機fixtureによるJSON path/event type確定
- context usage計算式
- subscription quota/rate-limitの分離
- WebSocket/SSE選択条件
- `previous_response_id`が使用される別条件の確認
- usage実機fixtureによる`input_tokens_details.cached_tokens`・`output_tokens_details.reasoning_tokens`の出現確認
