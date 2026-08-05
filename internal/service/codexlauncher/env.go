package codexlauncher

import (
	"sort"
	"strings"
)

// MergeEnv returns base with overrides applied using case-insensitive key
// matching (Windows environment variable names are case-insensitive). Keys in
// overrides replace the existing entry in place — they are never duplicated —
// and unknown keys are appended. Order of surviving base entries is preserved.
func MergeEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	lastIndex := make(map[string]int, len(base))
	for i, entry := range base {
		if k, _, ok := cutEnv(entry); ok {
			lastIndex[strings.ToUpper(k)] = i
		}
	}
	out := append([]string(nil), base...)
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		entry := k + "=" + overrides[k]
		if i, ok := lastIndex[strings.ToUpper(k)]; ok {
			out[i] = entry
		} else {
			out = append(out, entry)
		}
	}
	return out
}

// cutEnv splits an "KEY=VALUE" environment entry. It reports false for entries
// without a separator.
func cutEnv(entry string) (key, value string, ok bool) {
	i := strings.IndexByte(entry, '=')
	if i < 0 {
		return "", "", false
	}
	return entry[:i], entry[i+1:], true
}
