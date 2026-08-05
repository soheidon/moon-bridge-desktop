package codexconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	backupStemLen = 19 // YYYYMMDD T HHMMSS mmm Z
	maxBackups    = 5
)

// defaultBackupDir mirrors the old Tauri layout:
// %LOCALAPPDATA%\Moon Bridge\backups\codex-config
func defaultBackupDir(lookup func(string) string) (string, error) {
	root := lookup("LOCALAPPDATA")
	if root == "" {
		root = lookup("APPDATA")
	}
	if root == "" {
		root = lookup("USERPROFILE")
	}
	if root == "" {
		return "", errors.New("LOCALAPPDATA/APPDATA/USERPROFILE is unavailable")
	}
	return filepath.Join(root, "Moon Bridge", "backups", "codex-config"), nil
}

func backupName(now time.Time) string {
	now = now.UTC()
	return fmt.Sprintf("%04d%02d%02dT%02d%02d%02d%03dZ-config.toml",
		now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), now.Nanosecond()/int(time.Millisecond))
}

// CreateBackup writes data to a fresh create_new backup (never overwrites),
// trims the directory to the newest maxBackups, and returns the full path.
func CreateBackup(dir string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var path string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		path = filepath.Join(dir, backupName(time.Now()))
		var f *os.File
		f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			if _, werr := f.Write(data); werr != nil {
				err = werr
			} else {
				err = f.Sync()
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				os.Remove(path)
				return "", err
			}
			retainConfigBackups(dir, path)
			return path, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
		// Same-millisecond collision with an existing backup: let the clock tick
		// on so the retry produces a fresh name.
		time.Sleep(time.Millisecond)
	}
	return "", err
}

// ListBackups returns valid codex-config backups newest-first. A missing
// directory is an empty list, not an error. Symlinks and foreign files are
// skipped.
func ListBackups(dir string) ([]BackupInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []BackupInfo
	for _, e := range entries {
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		stem, ok := configBackupStem(e.Name())
		if !ok {
			continue
		}
		t, ok := parseBackupTimestamp(stem)
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{
			ID:        e.Name(),
			Name:      e.Name(),
			Path:      filepath.Join(dir, e.Name()),
			CreatedAt: t,
			Size:      info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// ResolveBackupPath accepts only a bare backup file name (never a path) and
// returns its full path under dir, rejecting traversal.
func ResolveBackupPath(dir, id string) (string, error) {
	if id == "" || id != filepath.Base(id) {
		return "", errors.New("backup id must be a bare file name")
	}
	if _, ok := configBackupStem(id); !ok {
		return "", errors.New("backup id is not a codex-config backup name")
	}
	dir = filepath.Clean(dir)
	resolved := filepath.Join(dir, id)
	if !pathWithin(dir, resolved) {
		return "", errors.New("backup path escapes the backup directory")
	}
	return resolved, nil
}

func pathWithin(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// retainConfigBackups deletes the oldest backups beyond maxBackups, never the
// just-created one.
func retainConfigBackups(dir, protected string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type entry struct {
		path string
		time time.Time
	}
	var list []entry
	for _, e := range entries {
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		stem, ok := configBackupStem(e.Name())
		if !ok {
			continue
		}
		t, ok := parseBackupTimestamp(stem)
		if !ok {
			continue
		}
		list = append(list, entry{filepath.Join(dir, e.Name()), t})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].time.After(list[j].time) })
	kept := 0
	for _, e := range list {
		if e.path == protected {
			continue
		}
		kept++
		if kept > maxBackups {
			os.Remove(e.path)
		}
	}
}

// configBackupStem validates a backup file name and returns its timestamp stem
// (YYYYMMDD T HHMMSS mmm Z).
func configBackupStem(name string) (string, bool) {
	stem, ok := strings.CutSuffix(name, "-config.toml")
	if !ok || len(stem) != backupStemLen {
		return "", false
	}
	for i := 0; i < len(stem); i++ {
		switch i {
		case 8:
			if stem[i] != 'T' {
				return "", false
			}
		case 18:
			if stem[i] != 'Z' {
				return "", false
			}
		default:
			if stem[i] < '0' || stem[i] > '9' {
				return "", false
			}
		}
	}
	return stem, true
}

func parseBackupTimestamp(stem string) (time.Time, bool) {
	year, err := strconv.Atoi(stem[0:4])
	if err != nil {
		return time.Time{}, false
	}
	month, _ := strconv.Atoi(stem[4:6])
	day, _ := strconv.Atoi(stem[6:8])
	hour, _ := strconv.Atoi(stem[9:11])
	min, _ := strconv.Atoi(stem[11:13])
	sec, _ := strconv.Atoi(stem[13:15])
	msec, _ := strconv.Atoi(stem[15:18])
	return time.Date(year, time.Month(month), day, hour, min, sec, msec*int(time.Millisecond), time.FixedZone("UTC", 0)), true
}
