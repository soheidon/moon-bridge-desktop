package trafficanalysis

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestAnalyzerSanitizesSentinelsEverywhere(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	observation := analyzer.Record(PayloadInput{
		Direction:    DirectionClientToUpstream,
		Transport:    TransportHTTP,
		Method:       "POST",
		ReceivedHost: "127.0.0.1:38441",
		ReceivedPath: "/responses",
		UpstreamHost: "chatgpt.com",
		UpstreamPath: "/backend-api/codex/responses",
		Headers:      http.Header{"Authorization": {"Bearer SENTINEL_AUTH_SECRET"}, "Cookie": {"session=SENTINEL_COOKIE_SECRET"}},
		ContentType:  "application/json",
		Payload:      []byte(`{"model":"SENTINEL_PROMPT_SECRET","input":"SENTINEL_SOURCE_SECRET","tools":[{"type":"function","name":"SENTINEL_TOOL_SECRET","description":"SENTINEL_TOOL_SECRET","encrypted_content":"SENTINEL_ENCRYPTED_SECRET","response_id":"SENTINEL_AUTH_SECRET"}]}`),
	})
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	output := string(encoded) + fmt.Sprintf("%+v", observation) + fmt.Sprintf("%#v", analyzer)
	for _, sentinel := range []string{"SENTINEL_AUTH_SECRET", "SENTINEL_COOKIE_SECRET", "SENTINEL_PROMPT_SECRET", "SENTINEL_SOURCE_SECRET", "SENTINEL_TOOL_SECRET", "SENTINEL_ENCRYPTED_SECRET"} {
		if strings.Contains(output, sentinel) {
			t.Fatalf("sanitized output contains sentinel %q: %s", sentinel, output)
		}
	}
	if !observation.HeaderSummary.AuthorizationPresent || !observation.HeaderSummary.CookiePresent {
		t.Fatalf("header presence was not classified: %+v", observation.HeaderSummary)
	}
	if observation.PayloadShape == nil || observation.PayloadShape.ModelValue != "" {
		t.Fatalf("sensitive model value was retained: %+v", observation.PayloadShape)
	}
	if len(observation.OpaqueFields) == 0 {
		t.Fatal("opaque field was not classified")
	}
}

func TestAnalyzerRecordsRequestModelOnlyForEligibleResponsesRequest(t *testing.T) {
	cases := []struct {
		name  string
		input PayloadInput
		want  string
	}{
		{name: "responses", input: PayloadInput{Direction: DirectionClientToUpstream, Transport: TransportHTTP, Method: "POST", ReceivedPath: "/responses", Payload: []byte(`{"model":"sol-literal","nested":{"model":"nested-literal"}}`), ContentType: "application/json", RequestModelEligible: true}, want: "sol-literal"},
		{name: "v1 responses", input: PayloadInput{Direction: DirectionClientToUpstream, Transport: TransportHTTP, Method: "POST", ReceivedPath: "/v1/responses", Payload: []byte(`{"model":"terra-literal"}`), ContentType: "application/json", RequestModelEligible: true}, want: "terra-literal"},
		{name: "missing", input: PayloadInput{RequestModelEligible: true, Payload: []byte(`{"input":"x"}`), ContentType: "application/json"}},
		{name: "non string", input: PayloadInput{RequestModelEligible: true, Payload: []byte(`{"model":{"value":"x"}}`), ContentType: "application/json"}},
		{name: "nested only", input: PayloadInput{RequestModelEligible: true, Payload: []byte(`{"input":{"model":"nested"}}`), ContentType: "application/json"}},
		{name: "ineligible", input: PayloadInput{Direction: DirectionUpstreamToClient, Transport: TransportHTTP, Method: "POST", ReceivedPath: "/responses", Payload: []byte(`{"model":"response-model"}`), ContentType: "application/json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			analyzer, err := NewAnalyzer(10)
			if err != nil {
				t.Fatal(err)
			}
			observation := analyzer.Record(tc.input)
			if observation.PayloadShape == nil || observation.PayloadShape.RequestModel != tc.want {
				t.Fatalf("request model = %q, want %q; shape=%+v", requestModelFromObservation(observation), tc.want, observation.PayloadShape)
			}
			if tc.want != "" && observation.PayloadShape.RequestModel == "" {
				t.Fatal("eligible model was omitted")
			}
		})
	}
}

func requestModelFromObservation(observation Observation) string {
	if observation.PayloadShape == nil {
		return ""
	}
	return observation.PayloadShape.RequestModel
}

func TestAnalyzerRecordsSafeProtocolShape(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	obs := analyzer.Record(PayloadInput{
		Direction:    DirectionClientToUpstream,
		Transport:    TransportHTTP,
		Method:       "POST",
		ReceivedPath: "/responses",
		ContentType:  "application/json",
		Payload:      []byte(`{"model":"gpt-5.6-luna","stream":true,"reasoning":{"effort":"high"},"tools":[{"type":"function"}],"response_id":"resp-123"}`),
	})
	if obs.PayloadKind != PayloadJSON || obs.DecodingStatus != DecodingIdentity {
		t.Fatalf("payload classification = %q/%q", obs.PayloadKind, obs.DecodingStatus)
	}
	if obs.PayloadShape == nil || obs.PayloadShape.ModelValue != "gpt-5.6-luna" || obs.PayloadShape.StreamValue == nil || !*obs.PayloadShape.StreamValue {
		t.Fatalf("safe fields not recorded: %+v", obs.PayloadShape)
	}
	if obs.PayloadShape.ReasoningEffort != "high" || obs.PayloadShape.ToolCount != 1 {
		t.Fatalf("reasoning/tools not recorded: %+v", obs.PayloadShape)
	}
	if len(obs.Identifiers.ResponseIDAliases) != 1 {
		t.Fatalf("response ID was not HMAC-classified: %+v", obs.Identifiers)
	}
}

func TestAnalyzerUsesDifferentPerSessionHMACKeys(t *testing.T) {
	first, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	input := PayloadInput{Payload: []byte(`{"response_id":"resp-123"}`), ContentType: "application/json"}
	firstObservation := first.Record(input)
	secondObservation := second.Record(input)
	if firstObservation.RawPayloadHMAC == secondObservation.RawPayloadHMAC {
		t.Fatal("different sessions reused the same HMAC")
	}
	if firstObservation.Identifiers.ResponseIDAliases[0] != "id#1" || secondObservation.Identifiers.ResponseIDAliases[0] != "id#1" {
		t.Fatal("session-local aliases did not start at id#1")
	}
}

func TestAnalyzerBoundsRawAndDecodedPayloads(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("a"), MaxRawAnalysisBytes+1)
	obs := analyzer.Record(PayloadInput{Payload: large, ContentType: "text/plain"})
	if obs.RawPayloadSize != len(large) || obs.DecodingStatus != DecodingRawLimitExceeded || !obs.Truncated {
		t.Fatalf("raw limit not enforced: size=%d status=%q truncated=%v", obs.RawPayloadSize, obs.DecodingStatus, obs.Truncated)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(bytes.Repeat([]byte("b"), MaxDecodedBytes+1)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	obs = analyzer.Record(PayloadInput{Payload: compressed.Bytes(), ContentEncoding: "gzip", ContentType: "application/json"})
	if obs.DecodingStatus != DecodingDecodedLimitExceeded || !obs.Truncated {
		t.Fatalf("decoded limit not enforced: status=%q truncated=%v", obs.DecodingStatus, obs.Truncated)
	}
}

func TestAnalyzerBoundsJSONShapeCounts(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	arrayPayload := `{"items":[` + strings.Repeat("0,", MaxArrayLength) + `0]}`
	arrayObservation := analyzer.Record(PayloadInput{Payload: []byte(arrayPayload), ContentType: "application/json"})
	if arrayObservation.PayloadShape == nil || arrayObservation.PayloadShape.ArrayLengths["items"] != MaxArrayLength || !arrayObservation.PayloadShape.ShapeTruncated {
		t.Fatalf("array shape limit not enforced: %+v", arrayObservation.PayloadShape)
	}

	var objectBuilder strings.Builder
	objectBuilder.WriteString(`{"items":{`)
	for index := 0; index < MaxObjectFields+1; index++ {
		if index > 0 {
			objectBuilder.WriteByte(',')
		}
		fmt.Fprintf(&objectBuilder, `"field%d":0`, index)
	}
	objectBuilder.WriteString(`}}`)
	objectObservation := analyzer.Record(PayloadInput{Payload: []byte(objectBuilder.String()), ContentType: "application/json"})
	if objectObservation.PayloadShape == nil || objectObservation.PayloadShape.ObjectFieldCounts["items"] != MaxObjectFields || !objectObservation.PayloadShape.ShapeTruncated {
		t.Fatalf("object shape limit not enforced: %+v", objectObservation.PayloadShape)
	}
}

func TestAnalyzerHandlesInvalidAndUnsupportedEncoding(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	invalid := analyzer.Record(PayloadInput{Payload: []byte("not gzip"), ContentEncoding: "gzip"})
	if invalid.DecodingStatus != DecodingInvalid {
		t.Fatalf("invalid gzip status = %q", invalid.DecodingStatus)
	}
	unsupported := analyzer.Record(PayloadInput{Payload: []byte("opaque"), ContentEncoding: "br"})
	if unsupported.DecodingStatus != DecodingUnsupported || unsupported.DecodedObservationSize != 0 {
		t.Fatalf("unsupported encoding = %q/%d", unsupported.DecodingStatus, unsupported.DecodedObservationSize)
	}
}

func TestAnalyzerDecodesZstdForSafeShapeObservation(t *testing.T) {
	var compressed bytes.Buffer
	writer, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"model":"gpt-test","previous_response_id":"resp-123","input":[{"type":"message"}],"tools":[{"type":"function"}],"prompt":"SENTINEL_PROMPT"}`)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	observation := analyzer.Record(PayloadInput{
		Payload:              compressed.Bytes(),
		ContentType:          "application/json",
		ContentEncoding:      "zstd",
		RequestModelEligible: true,
	})
	if observation.PayloadShape == nil || observation.PayloadShape.RequestModel != "gpt-test" {
		t.Fatalf("zstd request model = %q", requestModelFromObservation(observation))
	}
	if observation.ContentEncoding != "zstd" || observation.DecodingStatus != DecodingDecoded || observation.PayloadKind != PayloadJSON {
		t.Fatalf("zstd observation = encoding=%q status=%q kind=%q", observation.ContentEncoding, observation.DecodingStatus, observation.PayloadKind)
	}
	if observation.DecodedObservationSize != len(payload) || observation.PayloadShape == nil {
		t.Fatalf("zstd shape observation = size=%d shape=%+v", observation.DecodedObservationSize, observation.PayloadShape)
	}
	if len(observation.Identifiers.PreviousResponseIDAliases) != 1 || observation.Identifiers.PreviousResponseIDAliases[0] != "id#1" || strings.Contains(string(mustJSON(observation)), "resp-123") {
		t.Fatalf("zstd identifier was not safely summarized: %+v", observation.Identifiers)
	}
}

func TestAnalyzerFallsBackForMalformedZstd(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	observation := analyzer.Record(PayloadInput{Payload: []byte("not zstd"), ContentType: "application/json", ContentEncoding: "zstd"})
	if observation.DecodingStatus != DecodingInvalid || observation.DecodedObservationSize != 0 || observation.PayloadShape != nil {
		t.Fatalf("malformed zstd fallback = status=%q size=%d shape=%+v", observation.DecodingStatus, observation.DecodedObservationSize, observation.PayloadShape)
	}
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func TestAnalyzerClassifiesNestedIdentifiersAndInputSummary(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	observation := analyzer.Record(PayloadInput{
		Payload:     []byte(`{"response":{"id":"response-1"},"item":{"id":"item-1"},"call_id":"call-1","function_call":{"id":"function-1"},"previous_response_id":"previous-1","input":[{"type":"message","role":"user","content":"SENTINEL_TEXT"},{"type":"function_call","call_id":"call-2"}]}`),
		ContentType: "application/json",
	})
	if observation.PayloadShape == nil || observation.PayloadShape.InputItemCount != 2 {
		t.Fatalf("input summary = %+v", observation.PayloadShape)
	}
	if observation.PayloadShape.InputItemTypeCounts["message"] != 1 || observation.PayloadShape.InputItemTypeCounts["function_call"] != 1 {
		t.Fatalf("input type counts = %+v", observation.PayloadShape.InputItemTypeCounts)
	}
	if len(observation.PayloadShape.InputItemFingerprints) != 2 || observation.PayloadShape.InputItemFingerprints[0].Index != 0 || observation.PayloadShape.InputItemFingerprints[0].ContentCount != 0 {
		t.Fatalf("input fingerprints = %+v", observation.PayloadShape.InputItemFingerprints)
	}
	if observation.PayloadShape.InputItemFingerprints[0].Type != "message" || observation.PayloadShape.InputItemFingerprints[0].Role != "user" {
		t.Fatalf("input fingerprint classification = %+v", observation.PayloadShape.InputItemFingerprints[0])
	}
	// input[1] (function_call) carries call_id:"call-2"; its fingerprint
	// identifiers must be aliased with session-global id#N and the HMAC
	// digest must not survive into the fingerprint.
	fp1 := observation.PayloadShape.InputItemFingerprints[1]
	if len(fp1.Identifiers.CallIDAliases) != 1 {
		t.Fatalf("fingerprint[1] call aliases = %+v", fp1.Identifiers.CallIDAliases)
	}
	if len(fp1.Identifiers.CallIDHMACs) != 0 || len(fp1.Identifiers.ResponseIDHMACs) != 0 {
		t.Fatalf("fingerprint[1] leaked HMAC digests: call_hmacs=%+v response_hmacs=%+v", fp1.Identifiers.CallIDHMACs, fp1.Identifiers.ResponseIDHMACs)
	}
	if observation.PayloadShape.InputRoleCounts["user"] != 1 || !observation.PayloadShape.HasPreviousResponseID {
		t.Fatalf("input role/previous summary = roles=%+v previous=%v", observation.PayloadShape.InputRoleCounts, observation.PayloadShape.HasPreviousResponseID)
	}
	ids := observation.Identifiers
	if len(ids.ResponseIDAliases) != 1 || len(ids.ItemIDAliases) != 1 || len(ids.CallIDAliases) != 2 || len(ids.PreviousResponseIDAliases) != 1 || len(ids.OtherIDAliases) != 1 {
		t.Fatalf("identifier buckets = %+v", ids)
	}
	encoded := string(mustJSON(observation))
	for _, secret := range []string{"response-1", "item-1", "call-1", "function-1", "previous-1", "SENTINEL_TEXT"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("raw identifier or text leaked: %q", secret)
		}
	}
}

func TestSafeEventTypeUsesLiteralForKnownAndUnknownForUnrecognized(t *testing.T) {
	if got := safeEventType("response.completed"); got != "response.completed" {
		t.Fatalf("known event = %q", got)
	}
	unknown := "response.secret_internal_event"
	if got := safeEventType(unknown); got != "unknown" {
		t.Fatalf("unknown event = %q, want unknown", got)
	}
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	obs := analyzer.Record(PayloadInput{SSEEventType: unknown, Payload: []byte(`{"type":"response.secret_internal_event"}`), ContentType: "application/json"})
	if obs.SSEEventType != "unknown" {
		t.Fatalf("observation event = %q, want unknown", obs.SSEEventType)
	}
	encoded := string(mustJSON(obs))
	if strings.Contains(encoded, unknown) || strings.Contains(encoded, "event:") {
		t.Fatalf("unknown event or digest leaked: %s", encoded)
	}
}

func TestExtractUsageFromResponseCompleted(t *testing.T) {
	payload := []byte(`{"response":{"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":5}}}}`)
	u := extractUsageSummary(payload)
	if u == nil {
		t.Fatal("extractUsageSummary returned nil")
	}
	assertIntPtr(t, "input_tokens", u.InputTokens, 100)
	assertIntPtr(t, "output_tokens", u.OutputTokens, 20)
	assertIntPtr(t, "total_tokens", u.TotalTokens, 120)
	assertIntPtr(t, "cached_input_tokens", u.CachedInputTokens, 30)
	assertIntPtr(t, "reasoning_tokens", u.ReasoningTokens, 5)
}

func TestExtractUsageFromTopLevelUsage(t *testing.T) {
	payload := []byte(`{"usage":{"input_tokens":50,"output_tokens":10}}`)
	u := extractUsageSummary(payload)
	if u == nil {
		t.Fatal("extractUsageSummary returned nil")
	}
	assertIntPtr(t, "input_tokens", u.InputTokens, 50)
	assertIntPtr(t, "output_tokens", u.OutputTokens, 10)
	if u.TotalTokens != nil {
		t.Fatalf("total_tokens should be absent, got %d", *u.TotalTokens)
	}
}

func TestExtractUsagePrefersResponseUsage(t *testing.T) {
	// response.usage is valid → top-level usage is ignored.
	payload := []byte(`{"response":{"usage":{"input_tokens":100,"output_tokens":20}},"usage":{"input_tokens":999,"output_tokens":999}}`)
	u := extractUsageSummary(payload)
	if u == nil {
		t.Fatal("extractUsageSummary returned nil")
	}
	assertIntPtr(t, "input_tokens", u.InputTokens, 100)
	assertIntPtr(t, "output_tokens", u.OutputTokens, 20)
}

func TestExtractUsageFallsBackWhenResponseUsageEmpty(t *testing.T) {
	// response.usage exists but is empty (all zero) → fallback to top-level.
	payload := []byte(`{"response":{"usage":{"input_tokens":0,"output_tokens":0}},"usage":{"input_tokens":50,"output_tokens":10}}`)
	u := extractUsageSummary(payload)
	if u == nil {
		t.Fatal("extractUsageSummary returned nil; expected fallback to top-level usage")
	}
	assertIntPtr(t, "input_tokens", u.InputTokens, 50)
	assertIntPtr(t, "output_tokens", u.OutputTokens, 10)
}

func TestExtractUsageNilWhenNoUsage(t *testing.T) {
	payload := []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
	if u := extractUsageSummary(payload); u != nil {
		t.Fatalf("expected nil, got %+v", u)
	}
}

func TestExtractUsageIgnoresMalformed(t *testing.T) {
	if u := extractUsageSummary([]byte(`not json`)); u != nil {
		t.Fatalf("malformed JSON: expected nil, got %+v", u)
	}
	if u := extractUsageSummary([]byte(`{"usage":"string_not_object"}`)); u != nil {
		t.Fatalf("string usage: expected nil, got %+v", u)
	}
}

func TestExtractUsageIgnoresNegative(t *testing.T) {
	payload := []byte(`{"usage":{"input_tokens":-5,"output_tokens":10}}`)
	u := extractUsageSummary(payload)
	if u == nil {
		t.Fatal("extractUsageSummary returned nil")
	}
	if u.InputTokens != nil {
		t.Fatalf("negative input_tokens should be absent, got %d", *u.InputTokens)
	}
	assertIntPtr(t, "output_tokens", u.OutputTokens, 10)
}

func TestExtractUsageIgnoresFractional(t *testing.T) {
	// Go's json.Unmarshal rejects fractional values for int fields.
	// The entire usage object is rejected when any field is non-integer.
	payload := []byte(`{"usage":{"input_tokens":1.5,"output_tokens":10}}`)
	if u := extractUsageSummary(payload); u != nil {
		t.Fatalf("fractional value should cause rejection, got %+v", u)
	}
}

func TestExtractUsageIgnoresStringNumber(t *testing.T) {
	// String "123" is not a JSON number — json.Unmarshal into int rejects it.
	payload := []byte(`{"usage":{"input_tokens":"123","output_tokens":10}}`)
	if u := extractUsageSummary(payload); u != nil {
		t.Fatalf("string number should cause rejection, got %+v", u)
	}
}

func TestExtractUsageIgnoresUnknownField(t *testing.T) {
	payload := []byte(`{"usage":{"input_tokens":10,"unknown_metric":42}}`)
	u := extractUsageSummary(payload)
	if u == nil {
		t.Fatal("extractUsageSummary returned nil")
	}
	assertIntPtr(t, "input_tokens", u.InputTokens, 10)
}

func TestExtractUsageDoesNotComputeTotal(t *testing.T) {
	payload := []byte(`{"usage":{"input_tokens":100,"output_tokens":20}}`)
	u := extractUsageSummary(payload)
	if u == nil {
		t.Fatal("extractUsageSummary returned nil")
	}
	if u.TotalTokens != nil {
		t.Fatalf("total_tokens should be absent (not computed), got %d", *u.TotalTokens)
	}
}

func TestExtractUsageSkipsNonResponseCompleted(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	obs := analyzer.Record(PayloadInput{
		SSEEventType: "response.output_text.delta",
		Payload:      []byte(`{"usage":{"input_tokens":100,"output_tokens":20}}`),
		ContentType:  "application/json",
	})
	if obs.Usage != nil {
		t.Fatalf("non-response.completed should not extract usage, got %+v", obs.Usage)
	}
}

func TestExtractUsageOnResponseCompleted(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	obs := analyzer.Record(PayloadInput{
		SSEEventType: "response.completed",
		Payload:      []byte(`{"response":{"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}}`),
		ContentType:  "application/json",
	})
	if obs.Usage == nil {
		t.Fatal("response.completed should extract usage")
	}
	assertIntPtr(t, "input_tokens", obs.Usage.InputTokens, 100)
	assertIntPtr(t, "output_tokens", obs.Usage.OutputTokens, 20)
	assertIntPtr(t, "total_tokens", obs.Usage.TotalTokens, 120)
}

// When the response.usage object contains a malformed field (fractional or
// stringified number), the entire response.usage parse fails. If a valid
// top-level usage exists, the extractor falls back to it. This is fail-closed:
// we do NOT attempt field-by-field partial recovery from a structurally
// invalid object.
func TestExtractUsageMalformedFieldDropsEntireObjectAndFallsBack(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"response":{"usage":{"input_tokens":500,"output_tokens":"invalid","total_tokens":600}},"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`
	obs := analyzer.Record(PayloadInput{
		SSEEventType: "response.completed",
		Payload:      []byte(payload),
		ContentType:  "application/json",
	})
	if obs.Usage == nil {
		t.Fatal("expected fallback to top-level usage when response.usage is malformed")
	}
	// Fallback to top-level usage.
	assertIntPtr(t, "input_tokens", obs.Usage.InputTokens, 10)
	assertIntPtr(t, "output_tokens", obs.Usage.OutputTokens, 2)
	assertIntPtr(t, "total_tokens", obs.Usage.TotalTokens, 12)
}

// Server validity semantics: response.usage is considered invalid when
// input_tokens == 0 && output_tokens == 0 && cached_tokens == 0, regardless
// of total_tokens or reasoning_tokens. The extractor must fall back to the
// top-level usage in that case.
func TestExtractUsageValidityIgnoresTotalAndReasoningTokens(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"response":{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":999,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":999}}},"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}`
	obs := analyzer.Record(PayloadInput{
		SSEEventType: "response.completed",
		Payload:      []byte(payload),
		ContentType:  "application/json",
	})
	if obs.Usage == nil {
		t.Fatal("expected fallback to top-level usage when response.usage is invalid")
	}
	// Fallback to top-level usage.
	assertIntPtr(t, "input_tokens", obs.Usage.InputTokens, 100)
	assertIntPtr(t, "output_tokens", obs.Usage.OutputTokens, 20)
	assertIntPtr(t, "total_tokens", obs.Usage.TotalTokens, 120)
}

func TestExtractUsageBounded(t *testing.T) {
	payload := []byte(fmt.Sprintf(`{"usage":{"input_tokens":%d,"output_tokens":20}}`, maxUsageTokens+1))
	u := extractUsageSummary(payload)
	if u == nil {
		t.Fatal("extractUsageSummary returned nil")
	}
	if u.InputTokens != nil {
		t.Fatalf("over-bound input_tokens should be absent, got %d", *u.InputTokens)
	}
	assertIntPtr(t, "output_tokens", u.OutputTokens, 20)
}

func assertIntPtr(t *testing.T, name string, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is nil, want %d", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %d, want %d", name, *got, want)
	}
}

func TestRingBufferSnapshotsAndCapacity(t *testing.T) {
	analyzer, err := NewAnalyzer(2)
	if err != nil {
		t.Fatal(err)
	}
	analyzer.Record(PayloadInput{Payload: []byte("one")})
	analyzer.Record(PayloadInput{Payload: []byte("two")})
	analyzer.Record(PayloadInput{Payload: []byte("three")})
	items, dropped := analyzer.Snapshot(0)
	if len(items) != 2 || dropped != 1 {
		t.Fatalf("snapshot = %d items, %d dropped", len(items), dropped)
	}
	if items[0].Sequence != 2 || items[1].Sequence != 3 {
		t.Fatalf("sequences = %d, %d", items[0].Sequence, items[1].Sequence)
	}
	items, _ = analyzer.Snapshot(2)
	if len(items) != 1 || items[0].Sequence != 3 {
		t.Fatalf("after snapshot = %+v", items)
	}
	analyzer.Clear()
	items, _ = analyzer.Snapshot(0)
	if len(items) != 0 {
		t.Fatalf("clear left %d items", len(items))
	}
}

func TestGatewayEventsAliasAndSanitizeWithoutRawKeys(t *testing.T) {
	analyzer, err := NewAnalyzer(10)
	if err != nil {
		t.Fatal(err)
	}
	first := analyzer.RecordGatewayEvent(GatewayEventInput{Kind: ObservationRoutingResolved, CorrelationKey: "opaque-request-secret", ProfileID: "profile-secret", RequestedModel: "gpt-5.6-luna", RoutingSlot: "luna", Provider: "deepseek", UpstreamModel: "deepseek-v4-flash", Mode: "normal", ConfiguredEffort: ""})
	second := analyzer.RecordGatewayEvent(GatewayEventInput{Kind: ObservationProviderRequestPrepared, CorrelationKey: "opaque-request-secret", ProfileID: "profile-secret", Provider: "deepseek", Protocol: "anthropic", Model: "deepseek-v4-flash", Thinking: "disabled"})
	third := analyzer.RecordGatewayEvent(GatewayEventInput{Kind: ObservationProviderResponseReceived, CorrelationKey: "opaque-request-secret", ProfileID: "profile-secret", Provider: "deepseek", Protocol: "anthropic", Model: "deepseek-v4-flash", StatusCode: 200, ExchangeIndex: 1})
	diagnostic := analyzer.RecordGatewayEvent(GatewayEventInput{Kind: ObservationRoutingResolutionDiagnosed, CorrelationKey: "opaque-request-secret", Resolver: &ResolverDiagnosticInput{RequestedModel: "gpt-5.6-luna", ServerInstance: "server#7", ResolverGeneration: 4, ResolverPresent: true, InstallSource: "startup", ConfigSource: "persisted_store", ExtensionState: "valid", ActiveProfileState: "present_valid", SlotCount: 3, SolState: "ready", TerraState: "ready", LunaState: "ready", NormalResult: "slot_hit", ResolvedSlot: "luna", FallbackResult: "not_consulted", FinalStage: "exact_slot", KnownAlias: true}})
	responseModelSentinel := analyzer.RecordGatewayEvent(GatewayEventInput{Kind: ObservationProviderResponseModel, CorrelationKey: "opaque-request-secret", ProfileID: "profile-secret", Provider: "deepseek", Protocol: "anthropic", ResponseModel: "SENTINEL RESPONSE MODEL"})
	responseModelKnown := analyzer.RecordGatewayEvent(GatewayEventInput{Kind: ObservationProviderResponseModel, CorrelationKey: "opaque-request-secret", ProfileID: "profile-secret", Provider: "deepseek", Protocol: "anthropic", ResponseModel: "deepseek-v4-flash"})
	if first.GatewayEvent == nil || second.GatewayEvent == nil || first.GatewayEvent.RequestAlias != second.GatewayEvent.RequestAlias {
		t.Fatalf("event aliases = %#v / %#v, want correlated", first.GatewayEvent, second.GatewayEvent)
	}
	if first.GatewayEvent.ActiveProfile != "profile#1" {
		t.Fatalf("profile alias = %q, want profile#1", first.GatewayEvent.ActiveProfile)
	}
	if third.GatewayEvent == nil || third.GatewayEvent.StatusCode != 200 || third.GatewayEvent.ExchangeIndex != 1 {
		t.Fatalf("egress event summary = %#v", third.GatewayEvent)
	}
	if diagnostic.GatewayEvent == nil || diagnostic.GatewayEvent.Resolver == nil || diagnostic.GatewayEvent.Resolver.RequestedModel != "known_luna" || diagnostic.GatewayEvent.RequestAlias != first.GatewayEvent.RequestAlias {
		t.Fatalf("resolver diagnostic = %#v", diagnostic.GatewayEvent)
	}
	if responseModelSentinel.GatewayEvent == nil || responseModelSentinel.GatewayEvent.ResponseModel != "unknown" {
		t.Fatalf("response model sentinel not collapsed to unknown: %#v", responseModelSentinel.GatewayEvent)
	}
	if responseModelKnown.GatewayEvent == nil || responseModelKnown.GatewayEvent.ResponseModel != "deepseek-v4-flash" {
		t.Fatalf("response model allowlisted value = %#v, want deepseek-v4-flash", responseModelKnown.GatewayEvent)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, sentinel := range []string{"opaque-request-secret", "profile-secret", "Authorization", "api-key", "C:\\Users\\secret", "https://secret.example"} {
		if strings.Contains(text, sentinel) {
			t.Fatalf("serialized event contains sentinel %q: %s", sentinel, text)
		}
	}
	encoded, err = json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"opaque-request-secret", "C:\\Users\\secret", "https://secret.example", "Authorization", "api-key"} {
		if strings.Contains(string(encoded), sentinel) {
			t.Fatalf("serialized resolver diagnostic contains sentinel %q: %s", sentinel, encoded)
		}
	}
	if first.Kind != ObservationRoutingResolved || second.Kind != ObservationProviderRequestPrepared {
		t.Fatalf("kinds = %q / %q", first.Kind, second.Kind)
	}
	payload := analyzer.Record(PayloadInput{CorrelationKey: "opaque-request-secret", RequestModelEligible: true, Payload: []byte(`{"model":"gpt-5.6-luna"}`), ContentType: "application/json"})
	if payload.RequestID != first.GatewayEvent.RequestAlias {
		t.Fatalf("payload request alias = %q, gateway alias = %q", payload.RequestID, first.GatewayEvent.RequestAlias)
	}
	encoded, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "opaque-request-secret") {
		t.Fatalf("payload serialized raw correlation key: %s", encoded)
	}
}

func TestHeaderSummaryNeverCopiesSensitiveValues(t *testing.T) {
	observation := analyzePayload([]byte("key"), PayloadInput{Headers: http.Header{
		"Authorization":          {"Bearer SENTINEL_AUTH_SECRET"},
		"Cookie":                 {"session=SENTINEL_COOKIE_SECRET"},
		"Content-Type":           {"application/json; charset=utf-8"},
		"Content-Encoding":       {"SENTINEL_ENCODING_SECRET"},
		"SENTINEL_HEADER_SECRET": {"SENTINEL_HEADER_VALUE"},
		"User-Agent":             {"Codex/1.2.3 extra-sensitive-data"},
	}})
	encoded, _ := json.Marshal(observation.HeaderSummary)
	if strings.Contains(string(encoded), "SENTINEL") || strings.Contains(string(encoded), "extra-sensitive-data") {
		t.Fatalf("sensitive header value leaked: %s", encoded)
	}
	if observation.HeaderSummary.ContentType != "application/json" || observation.HeaderSummary.ContentEncoding != "" || observation.HeaderSummary.UserAgentProduct != "Codex/1.2.3" {
		t.Fatalf("safe header classification = %+v", observation.HeaderSummary)
	}
}
