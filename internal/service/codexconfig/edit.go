package codexconfig

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// Field is a single target-key value the editor writes. Table is the owning
// table path (nil = root table); Key is the bare key name.
type Field struct {
	Key   string
	Table []string
	Value any // string or int64
}

// Apply rewrites original so each field holds its value, preserving
// everything else (comments, ordering, unknown fields, trailing comments).
// It returns the new bytes and whether anything changed. The file must parse
// as TOML; if the edit cannot be made safely it is refused with an
// edit_unsupported error (no partial write).
func Apply(original []byte, fields []Field) ([]byte, bool, error) {
	if !utf8.Valid(original) {
		return nil, false, editUnsupported("invalid UTF-8")
	}
	eol, err := detectEOL(original)
	if err != nil {
		return nil, false, err
	}
	text := string(original)

	rawLines := strings.Split(text, "\n")
	if eol == "\r\n" {
		for i := range rawLines {
			rawLines[i] = strings.TrimSuffix(rawLines[i], "\r")
		}
	}
	inside := multilineContinuations(text)

	// Classify lines first: BurntSushi rejects duplicate keys/tables as a parse
	// error, but the plan classifies them as edit_unsupported, so duplicates are
	// detected structurally before the parse.
	lines := make([]lineInfo, len(rawLines))
	var headerIdx []int
	curScope := []string{}
	curArray := false
	seenHeaders := map[string]bool{}
	seenKeys := map[string]bool{}
	badHeader := false

	for i, raw := range rawLines {
		if inside[i] {
			lines[i].kind = lineContinuation
			continue
		}
		content := strings.TrimLeft(raw, " \t")
		if content == "" || strings.HasPrefix(content, "#") {
			continue
		}
		if strings.HasPrefix(content, "[") {
			path, isArray, ok := parseHeader(raw)
			if !ok {
				badHeader = true
				continue
			}
			key := strings.Join(path, "\x00")
			if seenHeaders[key] {
				return nil, false, editUnsupported("duplicate table definition")
			}
			seenHeaders[key] = true
			switch {
			case isArray:
				curArray = true
			case scopePrefix(curScope, path):
				// nested: inherit array scope
			default:
				curArray = false
			}
			curScope = path
			headerIdx = append(headerIdx, i)
			lines[i].kind = lineHeader
			lines[i].headerPath = path
			lines[i].headerArray = isArray
			continue
		}
		eq := findEq(raw)
		if eq < 0 {
			continue // not a key line in valid TOML
		}
		keyParts := parseKeyPath(strings.TrimSpace(raw[:eq]))
		begin, end, multiline, inline := valueRegion(raw, eq)
		full := make([]string, 0, len(curScope)+len(keyParts))
		full = append(full, curScope...)
		full = append(full, keyParts...)
		key := strings.Join(full, "\x00")
		if seenKeys[key] {
			return nil, false, editUnsupported("duplicate key definition")
		}
		seenKeys[key] = true
		lines[i].kind = lineKeyValue
		lines[i].fullKey = full
		lines[i].keyIsDotted = len(keyParts) > 1
		lines[i].eqIdx = eq
		lines[i].valueBegin = begin
		lines[i].valueEnd = end
		lines[i].valueMultiline = multiline
		lines[i].inline = inline
		lines[i].inArrayScope = curArray
	}

	var decoded map[string]any
	if _, err := toml.Decode(text, &decoded); err != nil {
		return nil, false, parseFailed("config TOML parse failed")
	}
	if badHeader {
		return nil, false, editUnsupported("unrecognized table header")
	}

	var edits []edit
	for _, f := range fields {
		target := make([]string, 0, len(f.Table)+1)
		target = append(target, f.Table...)
		target = append(target, f.Key)

		var matchIdx = -1
		for i := range lines {
			if lines[i].kind == lineKeyValue && equalStr(lines[i].fullKey, target) {
				if matchIdx >= 0 {
					return nil, false, editUnsupported("duplicate target key")
				}
				matchIdx = i
			}
		}

		if matchIdx >= 0 {
			li := lines[matchIdx]
			if li.inArrayScope {
				return nil, false, editUnsupported("target key inside array of tables")
			}
			if li.valueMultiline {
				return nil, false, editUnsupported("target key uses a multiline string")
			}
			if li.inline {
				return nil, false, editUnsupported("target key inside an inline table")
			}
			if li.keyIsDotted {
				return nil, false, editUnsupported("target key expressed as a dotted key")
			}
			if cur, ok := lookup(decoded, f.Table, f.Key); ok && valuesEqual(cur, f.Value) {
				continue // no-op
			}
			edits = append(edits, edit{
				pos:   matchIdx,
				repl:  rawLines[matchIdx][:li.valueBegin] + encodeValue(f.Value) + rawLines[matchIdx][li.valueEnd:],
				lines: matchIdx,
			})
			continue
		}

		// Missing: refuse any dotted/inline construction of the target path,
		// then find an unambiguous insertion position.
		for i := range lines {
			if lines[i].kind != lineKeyValue {
				continue
			}
			li := lines[i]
			if equalStr(li.fullKey, target) {
				continue
			}
			if !scopePrefix(li.fullKey, target) {
				continue
			}
			if li.inline {
				return nil, false, editUnsupported("target table defined as an inline table")
			}
			if li.keyIsDotted {
				return nil, false, editUnsupported("target table defined by a dotted key")
			}
		}

		var pos int
		if len(f.Table) == 0 {
			pos = len(rawLines)
			if len(headerIdx) > 0 {
				pos = headerIdx[0]
			}
		} else {
			headerPos := -1
			for _, j := range headerIdx {
				if equalStr(lines[j].headerPath, f.Table) {
					headerPos = j
					break
				}
			}
			if headerPos < 0 {
				return nil, false, editUnsupported("target table has no unambiguous header to insert into")
			}
			if lines[headerPos].headerArray {
				return nil, false, editUnsupported("target table is an array of tables")
			}
			pos = len(rawLines)
			for _, j := range headerIdx {
				if j > headerPos {
					pos = j
					break
				}
			}
		}
		// Back off blank lines so the key lands after the last content line of
		// its table, preserving the blank separator before the next header (and
		// the file's trailing newline).
		for pos > 0 && isBlankLine(rawLines[pos-1]) {
			pos--
		}
		edits = append(edits, edit{
			pos:  pos,
			line: f.Key + " = " + encodeValue(f.Value),
		})
	}

	if len(edits) == 0 {
		return original, false, nil
	}

	out := applyEdits(rawLines, edits, eol)
	if _, err := toml.Decode(out, &decoded); err != nil {
		return nil, false, editUnsupported("editor produced invalid TOML")
	}
	return []byte(out), true, nil
}

type edit struct {
	pos   int    // line index (before-emission for inserts)
	line  string // insert: the line to add
	repl  string // replace: full replacement line
	lines int    // replace: which line index to replace
}

func applyEdits(rawLines []string, edits []edit, eol string) string {
	inserts := map[int][]string{}
	replaces := map[int]string{}
	var positions []int
	for _, e := range edits {
		if e.line != "" {
			inserts[e.pos] = append(inserts[e.pos], e.line)
		} else {
			replaces[e.lines] = e.repl
		}
	}
	for p := range inserts {
		positions = append(positions, p)
	}
	sort.Ints(positions)
	pi := 0
	var out []string
	for i, raw := range rawLines {
		for pi < len(positions) && positions[pi] == i {
			out = append(out, inserts[i]...)
			pi++
		}
		if repl, ok := replaces[i]; ok {
			out = append(out, repl)
		} else {
			out = append(out, raw)
		}
	}
	for pi < len(positions) && positions[pi] == len(rawLines) {
		out = append(out, inserts[len(rawLines)]...)
		pi++
	}
	return strings.Join(out, eol)
}

type lineKind int

const (
	lineContinuation lineKind = iota
	lineHeader
	lineKeyValue
)

type lineInfo struct {
	kind           lineKind
	headerPath     []string
	headerArray    bool
	fullKey        []string
	keyIsDotted    bool
	eqIdx          int
	valueBegin     int
	valueEnd       int
	valueMultiline bool
	inline         bool
	inArrayScope   bool
}

func detectEOL(data []byte) (string, error) {
	hasCRLF, hasLF, hasBareCR := false, false, false
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			if i > 0 && data[i-1] == '\r' {
				hasCRLF = true
			} else {
				hasLF = true
			}
		case '\r':
			if i+1 >= len(data) || data[i+1] != '\n' {
				hasBareCR = true
			}
		}
	}
	if hasBareCR {
		return "", editUnsupported("bare or mixed CR line endings")
	}
	if hasCRLF && hasLF {
		return "", editUnsupported("mixed line endings")
	}
	if hasCRLF {
		return "\r\n", nil
	}
	return "\n", nil
}

// multilineContinuations marks each line that begins while a multiline string
// (""" or ”') is still open, so its content is not mistaken for a key line.
func multilineContinuations(text string) []bool {
	runes := []rune(text)
	starts := make([]bool, 0, 16)
	starts = append(starts, false)
	state := 0 // 0 none, 1 in """, 2 in '''
	inSingle := 0
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if c == '\n' {
			starts = append(starts, state == 1 || state == 2)
			continue
		}
		switch state {
		case 0:
			switch {
			case inSingle == 1:
				if c == '\\' {
					i++
				} else if c == '"' {
					inSingle = 0
				}
			case inSingle == 2:
				if c == '\'' {
					inSingle = 0
				}
			case c == '"':
				if i+2 < len(runes) && runes[i+1] == '"' && runes[i+2] == '"' {
					state = 1
					i += 2
				} else {
					inSingle = 1
				}
			case c == '\'':
				if i+2 < len(runes) && runes[i+1] == '\'' && runes[i+2] == '\'' {
					state = 2
					i += 2
				} else {
					inSingle = 2
				}
			}
		case 1:
			if c == '\\' {
				i++
			} else if c == '"' && i+2 < len(runes) && runes[i+1] == '"' && runes[i+2] == '"' {
				state = 0
				i += 2
			}
		case 2:
			if c == '\'' && i+2 < len(runes) && runes[i+1] == '\'' && runes[i+2] == '\'' {
				state = 0
				i += 2
			}
		}
	}
	return starts
}

// parseHeader extracts a table header path. isArray reports [[...]] form.
func parseHeader(raw string) (path []string, isArray bool, ok bool) {
	content := strings.TrimSpace(raw)
	if !strings.HasPrefix(content, "[") {
		return nil, false, false
	}
	isArray = strings.HasPrefix(content, "[[")
	start := 1
	if isArray {
		start = 2
	}
	inBasic, inLiteral := false, false
	closeIdx := -1
	for i := start; i < len(content); i++ {
		c := content[i]
		switch {
		case inBasic:
			if c == '\\' {
				i++
			} else if c == '"' {
				inBasic = false
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
		case c == '"':
			inBasic = true
		case c == '\'':
			inLiteral = true
		case c == ']':
			closeIdx = i
			i = len(content)
		}
	}
	if closeIdx < 0 {
		return nil, false, false
	}
	if isArray {
		if closeIdx+1 >= len(content) || content[closeIdx+1] != ']' {
			return nil, false, false
		}
		rest := strings.TrimSpace(content[closeIdx+2:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return nil, false, false
		}
		return parseKeyPath(content[start:closeIdx]), true, true
	}
	rest := strings.TrimSpace(content[closeIdx+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return nil, false, false
	}
	return parseKeyPath(content[start:closeIdx]), false, true
}

// parseKeyPath splits a (possibly dotted, possibly quoted) key or header name
// into its unquoted parts.
func parseKeyPath(s string) []string {
	var parts []string
	var cur strings.Builder
	inBasic, inLiteral := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inBasic:
			if c == '\\' && i+1 < len(s) {
				cur.WriteByte(s[i+1])
				i++
			} else if c == '"' {
				inBasic = false
			} else {
				cur.WriteByte(c)
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			} else {
				cur.WriteByte(c)
			}
		case c == '"':
			inBasic = true
		case c == '\'':
			inLiteral = true
		case c == '.':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, cur.String())
	return parts
}

// findEq returns the index of the first '=' outside quoted sections, or -1.
func findEq(raw string) int {
	inBasic, inLiteral := false, false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case inBasic:
			if c == '\\' {
				i++
			} else if c == '"' {
				inBasic = false
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
		case c == '"':
			inBasic = true
		case c == '\'':
			inLiteral = true
		case c == '=':
			return i
		}
	}
	return -1
}

// valueRegion returns the value span [begin, end) within raw for the key at
// eqIdx, and flags a multiline-string value or an inline table.
func valueRegion(raw string, eqIdx int) (begin, end int, multiline, inline bool) {
	n := len(raw)
	begin = eqIdx + 1
	for begin < n && (raw[begin] == ' ' || raw[begin] == '\t') {
		begin++
	}
	if begin >= n {
		return begin, begin, false, false
	}
	if strings.HasPrefix(raw[begin:], `"""`) || strings.HasPrefix(raw[begin:], `'''`) {
		return begin, begin, true, false
	}
	inBasic, inLiteral := false, false
	end = n
	for i := begin; i < n; i++ {
		c := raw[i]
		switch {
		case inBasic:
			if c == '\\' {
				i++
			} else if c == '"' {
				inBasic = false
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
		case c == '"':
			inBasic = true
		case c == '\'':
			inLiteral = true
		case c == '{':
			inline = true
		case c == '#':
			end = i
			i = n
		}
	}
	for end > begin && (raw[end-1] == ' ' || raw[end-1] == '\t') {
		end--
	}
	return begin, end, false, inline
}

func encodeValue(v any) string {
	switch x := v.(type) {
	case string:
		return encodeBasicString(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	}
	return ""
}

func encodeBasicString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func lookup(decoded map[string]any, table []string, key string) (any, bool) {
	var cur any = decoded
	for _, part := range table {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}

func valuesEqual(cur, want any) bool {
	if cur == nil {
		return false
	}
	if w, ok := want.(string); ok {
		s, ok := cur.(string)
		return ok && s == w
	}
	if w, ok := want.(int64); ok {
		switch c := cur.(type) {
		case int64:
			return c == w
		case int:
			return int64(c) == w
		case int32:
			return int64(c) == w
		}
		return false
	}
	return false
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isBlankLine(s string) bool {
	return strings.Trim(s, " \t") == ""
}

// scopePrefix reports whether parent is a strict or equal prefix of child.
func scopePrefix(parent, child []string) bool {
	if len(child) < len(parent) {
		return false
	}
	for i := range parent {
		if parent[i] != child[i] {
			return false
		}
	}
	return true
}
