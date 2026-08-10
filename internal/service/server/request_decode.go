package server

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

var (
	errUnsupportedContentEncoding = errors.New("unsupported content encoding")
	errRequestBodyTooLarge        = errors.New("request body too large")
)

const (
	maxEncodedZstdRequestBody = 16 << 20 // encoded (compressed) side cap
	maxDecodedRequestBody     = 16 << 20 // decoded side cap
)

// normalizeContentEncoding reduces the full Content-Encoding header — which may
// be split across multiple header lines — to a single supported token:
// "identity" or "zstd". Any comma-separated list, multiple header values, or an
// unknown single encoding is rejected. The Codex request carries a single
// "zstd" token.
func normalizeContentEncoding(values []string) (string, error) {
	if len(values) == 0 {
		return "identity", nil
	}
	if len(values) > 1 {
		return "", errUnsupportedContentEncoding
	}
	raw := values[0]
	if strings.Contains(raw, ",") {
		return "", errUnsupportedContentEncoding
	}
	token := strings.ToLower(strings.TrimSpace(raw))
	switch token {
	case "", "identity":
		return "identity", nil
	case "zstd":
		return "zstd", nil
	}
	return "", errUnsupportedContentEncoding
}

// decodeRequestBody applies the resolved Content-Encoding to the encoded body.
// identity/empty returns the body unchanged. Anything else (including a
// multi-encoding list that survived a buggy caller) is rejected. The encoded
// side is bounded by the caller before reading.
func decodeRequestBody(encoded []byte, contentEncoding string) ([]byte, error) {
	switch contentEncoding {
	case "", "identity":
		return encoded, nil
	case "zstd":
		return decodeZstdBounded(encoded)
	default:
		return nil, errUnsupportedContentEncoding
	}
}

// decodeZstdBounded decompresses a zstd frame under a decoded-size cap. The
// decoder memory is bounded via WithDecoderMaxMemory + WithDecoderLowmem; a
// frame that merely declares a large window but produces small output is still
// accepted (WithDecoderMaxWindow is deliberately not set to avoid that false
// rejection). Malformed frames fail at the reader.
func decodeZstdBounded(encoded []byte) ([]byte, error) {
	reader, err := zstd.NewReader(bytes.NewReader(encoded),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(maxDecodedRequestBody),
	)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, maxDecodedRequestBody+1))
	if err != nil {
		return nil, err
	}
	if len(decoded) > maxDecodedRequestBody {
		return nil, errRequestBodyTooLarge
	}
	return decoded, nil
}
