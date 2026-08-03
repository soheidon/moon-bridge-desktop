package trafficanalysis

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
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
	if len(obs.Identifiers.ResponseIDHMACs) != 1 {
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
	if firstObservation.Identifiers.ResponseIDHMACs[0] == secondObservation.Identifiers.ResponseIDHMACs[0] {
		t.Fatal("different sessions reused the same identifier HMAC")
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
