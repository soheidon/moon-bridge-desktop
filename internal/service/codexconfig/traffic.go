package codexconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

const trafficRootURLKey = "openai_base_url"

// RootURLSnapshot is the safe, decoded state of the top-level
// openai_base_url. Hash is the SHA-256 of the decoded URL value, not of the
// source line. ConfigHash is the SHA-256 of the complete config bytes and is
// used only for compare-and-swap operations inside the service layer.
type RootURLSnapshot struct {
	Present    bool
	Value      string
	Hash       string
	ConfigHash string
}

// RoutingIdentitySnapshot is the non-secret top-level Codex routing identity
// used by Traffic Analysis. It deliberately contains no provider credentials,
// model catalog, or config body. ConfigHash binds the identity to the same
// compare-and-swap generation used for openai_base_url edits.
type RoutingIdentitySnapshot struct {
	Model         string
	ModelProvider string
	ConfigHash    string
}

// ReadRoutingIdentity reads only the top-level model and model_provider keys.
// The values are used to create a session-scoped routing mapping; the Codex
// config is never rewritten by this method.
func (s *Service) ReadRoutingIdentity(ctx context.Context) (RoutingIdentitySnapshot, error) {
	if err := contextErr(ctx); err != nil {
		return RoutingIdentitySnapshot{}, err
	}
	path, err := s.ResolvePath()
	if err != nil {
		return RoutingIdentitySnapshot{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RoutingIdentitySnapshot{}, &Error{Kind: KindNotFound, Message: "codex config not found"}
		}
		return RoutingIdentitySnapshot{}, err
	}
	return readRoutingIdentity(data)
}

// PreparedRootURLChange contains a candidate top-level URL change. The
// candidate bytes are deliberately private so callers cannot accidentally
// persist or expose raw config content outside the commit operation.
type PreparedRootURLChange struct {
	BeforeHash      string
	AfterHash       string
	Present         bool
	Value           string
	PreviousPresent bool
	PreviousValue   string

	path      string
	candidate []byte
}

// ReadRootURL reads and validates the current config and returns only the
// decoded top-level URL state and hashes.
func (s *Service) ReadRootURL(ctx context.Context) (RootURLSnapshot, error) {
	if err := contextErr(ctx); err != nil {
		return RootURLSnapshot{}, err
	}
	path, err := s.ResolvePath()
	if err != nil {
		return RootURLSnapshot{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RootURLSnapshot{}, &Error{Kind: KindNotFound, Message: "codex config not found"}
		}
		return RootURLSnapshot{}, err
	}
	return readRootURL(data)
}

// PrepareRootURLChange prepares a lossless set or delete operation. A nil
// desired value deletes the top-level key. The optional expectedConfigHash is
// checked against the complete file before a candidate is produced.
func (s *Service) PrepareRootURLChange(ctx context.Context, desired *string, expectedConfigHash string) (*PreparedRootURLChange, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	path, err := s.ResolvePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &Error{Kind: KindNotFound, Message: "codex config not found"}
		}
		return nil, err
	}
	if expectedConfigHash != "" && hashBytes(data) != expectedConfigHash {
		return nil, &Error{Kind: KindConflict, Message: "codex config changed before traffic edit"}
	}

	before, err := readRootURL(data)
	if err != nil {
		return nil, err
	}
	if desired != nil {
		if err := validateTrafficURL(*desired); err != nil {
			return nil, err
		}
	}

	var fields []Field
	if desired != nil {
		fields = []Field{{Key: trafficRootURLKey, Value: *desired}}
	}
	candidate := data
	changed := false
	if desired != nil {
		candidate, changed, err = Apply(data, fields)
	} else {
		candidate, changed, err = deleteRootURL(data)
	}
	if err != nil {
		return nil, err
	}
	if !changed {
		candidate = data
	}
	after, err := readRootURL(candidate)
	if err != nil {
		return nil, err
	}
	return &PreparedRootURLChange{
		BeforeHash:      hashBytes(data),
		AfterHash:       hashBytes(candidate),
		Present:         after.Present,
		Value:           after.Value,
		PreviousPresent: before.Present,
		PreviousValue:   before.Value,
		path:            path,
		candidate:       append([]byte(nil), candidate...),
	}, nil
}

// CommitPreparedRootURLChange atomically commits a prepared change only if
// the full file still has the prepared before hash. It re-reads and validates
// the managed key after the write.
func (s *Service) CommitPreparedRootURLChange(ctx context.Context, prepared *PreparedRootURLChange) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if prepared == nil || prepared.path == "" || prepared.candidate == nil {
		return &Error{Kind: KindValidationFailed, Message: "prepared traffic edit is invalid"}
	}
	path, err := s.ResolvePath()
	if err != nil {
		return err
	}
	if path != prepared.path {
		return &Error{Kind: KindConflict, Message: "codex config path changed before traffic edit"}
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return &Error{Kind: KindUpdateFailed, Message: "read codex config before traffic edit failed"}
	}
	if hashBytes(current) != prepared.BeforeHash {
		return &Error{Kind: KindConflict, Message: "codex config changed before traffic edit"}
	}
	if err := atomicWrite(path, prepared.candidate); err != nil {
		return &Error{Kind: KindUpdateFailed, Message: "atomic traffic config update failed"}
	}
	written, err := os.ReadFile(path)
	if err != nil || hashBytes(written) != prepared.AfterHash {
		return &Error{Kind: KindVerifyFailed, Message: "traffic config update verification failed"}
	}
	state, err := readRootURL(written)
	if err != nil || state.Present != prepared.Present || state.Value != prepared.Value {
		return &Error{Kind: KindVerifyFailed, Message: "traffic config managed key verification failed"}
	}
	return nil
}

// KindConflict identifies a compare-and-swap failure without exposing the
// changed config contents.
const KindConflict ErrorKind = "codex_config_conflict"

func readRootURL(data []byte) (RootURLSnapshot, error) {
	if !isValidUTF8(data) {
		return RootURLSnapshot{}, editUnsupported("invalid UTF-8")
	}
	eol, err := detectEOL(data)
	if err != nil {
		return RootURLSnapshot{}, err
	}
	lines := strings.Split(string(data), "\n")
	if eol == "\r\n" {
		for i := range lines {
			lines[i] = strings.TrimSuffix(lines[i], "\r")
		}
	}
	inside := multilineContinuations(string(data))
	var scope []string
	var found *rootURLLine
	for i, raw := range lines {
		if inside[i] {
			continue
		}
		content := strings.TrimLeft(raw, " \t")
		if content == "" || strings.HasPrefix(content, "#") {
			continue
		}
		if strings.HasPrefix(content, "[") {
			path, isArray, ok := parseHeader(raw)
			if !ok {
				return RootURLSnapshot{}, editUnsupported("unrecognized table header")
			}
			scope = path
			if isArray {
				// A root key cannot be safely edited from an array-of-tables scope.
				scope = append([]string{"\x00array"}, scope...)
			}
			continue
		}
		if len(scope) != 0 {
			continue
		}
		eq := findEq(raw)
		if eq < 0 {
			continue
		}
		keyParts := parseKeyPath(strings.TrimSpace(raw[:eq]))
		if len(keyParts) != 1 || keyParts[0] != trafficRootURLKey {
			continue
		}
		if found != nil {
			return RootURLSnapshot{}, editUnsupported("duplicate traffic URL key")
		}
		begin, end, multiline, inline := valueRegion(raw, eq)
		if multiline || inline {
			return RootURLSnapshot{}, editUnsupported("traffic URL key uses an unsupported value")
		}
		found = &rootURLLine{index: i, begin: begin, end: end}
	}
	decoded, err := decodeTOML(data)
	if err != nil {
		return RootURLSnapshot{}, err
	}
	if found != nil {
		value, ok := lookup(decoded, nil, trafficRootURLKey)
		if !ok {
			return RootURLSnapshot{}, parseFailed("traffic URL key could not be decoded")
		}
		text, ok := value.(string)
		if !ok {
			return RootURLSnapshot{}, parseFailed("traffic URL value is not a string")
		}
		found.value = text
	}
	result := RootURLSnapshot{ConfigHash: hashBytes(data)}
	if found != nil {
		result.Present = true
		result.Value = found.value
		result.Hash = hashString(found.value)
	}
	return result, nil
}

func readRoutingIdentity(data []byte) (RoutingIdentitySnapshot, error) {
	if !isValidUTF8(data) {
		return RoutingIdentitySnapshot{}, editUnsupported("invalid UTF-8")
	}
	decoded, err := decodeTOML(data)
	if err != nil {
		return RoutingIdentitySnapshot{}, err
	}
	var model, modelProvider string
	if value, ok := lookup(decoded, nil, "model"); ok {
		model, _ = value.(string)
	}
	if value, ok := lookup(decoded, nil, "model_provider"); ok {
		modelProvider, _ = value.(string)
	}
	return RoutingIdentitySnapshot{
		Model:         model,
		ModelProvider: modelProvider,
		ConfigHash:    hashBytes(data),
	}, nil
}

type rootURLLine struct {
	index int
	begin int
	end   int
	value string
}

func deleteRootURL(data []byte) ([]byte, bool, error) {
	if !isValidUTF8(data) {
		return nil, false, editUnsupported("invalid UTF-8")
	}
	eol, err := detectEOL(data)
	if err != nil {
		return nil, false, err
	}
	lines := strings.Split(string(data), "\n")
	if eol == "\r\n" {
		for i := range lines {
			lines[i] = strings.TrimSuffix(lines[i], "\r")
		}
	}
	inside := multilineContinuations(string(data))
	scope := []string{}
	target := -1
	for i, raw := range lines {
		if inside[i] {
			continue
		}
		content := strings.TrimLeft(raw, " \t")
		if content == "" || strings.HasPrefix(content, "#") {
			continue
		}
		if strings.HasPrefix(content, "[") {
			path, _, ok := parseHeader(raw)
			if !ok {
				return nil, false, editUnsupported("unrecognized table header")
			}
			scope = path
			continue
		}
		if len(scope) != 0 {
			continue
		}
		eq := findEq(raw)
		if eq < 0 {
			continue
		}
		parts := parseKeyPath(strings.TrimSpace(raw[:eq]))
		if len(parts) != 1 || parts[0] != trafficRootURLKey {
			continue
		}
		if target >= 0 {
			return nil, false, editUnsupported("duplicate traffic URL key")
		}
		_, _, multiline, inline := valueRegion(raw, eq)
		if multiline || inline {
			return nil, false, editUnsupported("traffic URL key uses an unsupported value")
		}
		target = i
	}
	if target < 0 {
		if _, err := decodeTOML(data); err != nil {
			return nil, false, err
		}
		return data, false, nil
	}
	if _, err := decodeTOML(data); err != nil {
		return nil, false, err
	}
	out := append([]string(nil), lines[:target]...)
	out = append(out, lines[target+1:]...)
	return []byte(strings.Join(out, eol)), true, nil
}

func validateTrafficURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return &Error{Kind: KindValidationFailed, Message: "traffic URL must be an absolute HTTP(S) URL without credentials or query data"}
	}
	return nil
}

func decodeTOML(data []byte) (map[string]any, error) {
	var decoded map[string]any
	if _, err := toml.Decode(string(data), &decoded); err != nil {
		return nil, parseFailed("config TOML parse failed")
	}
	return decoded, nil
}

func isValidUTF8(data []byte) bool {
	return utf8.Valid(data)
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func hashString(value string) string {
	return hashBytes([]byte(value))
}

func contextErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
