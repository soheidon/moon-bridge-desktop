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

// errBackupExists is returned by backupPlatformOps.createFile when the chosen
// bare name already exists; the orchestration retries with a fresh timestamp.
var errBackupExists = errors.New("backup exists")

// Safe error codes surfaced to callers. They never embed paths, Backup IDs,
// SIDs, usernames, or secret material.
const (
	backupSecurityFailed = "backup_security_failed"
	backupIdentityFailed = "backup_identity_failed"
	backupWriteFailed    = "backup_write_failed"
	backupCleanupFailed  = "backup_cleanup_failed"
)

// backupRoot and backupFile are opaque handle wrappers. Their concrete
// definitions are platform-specific (backup_other.go / backup_windows.go) so
// the common orchestration never touches platform handle types directly.
type backupRoot interface{}
type backupFile interface{}

// backupPlatformOps isolates the security-sensitive create sequence per
// platform. On Windows it performs a handle-based root open, protected DACL
// apply+verify, and root-handle-relative create-new so a directory swap cannot
// redirect the secret payload. Elsewhere it preserves the original
// os.OpenFile(O_CREATE|O_EXCL|O_WRONLY, 0600) behavior.
type backupPlatformOps interface {
	// openRoot opens dir as a directory, creating it as needed, and returns an
	// opaque root handle. On Windows the directory chain is created relative to
	// a trusted base handle and must not be created by absolute-path MkdirAll.
	openRoot(dir string) (backupRoot, error)
	// verifyRoot confirms the opened root is a non-reparse directory and pins
	// its identity (FILE_ID_INFO on Windows).
	verifyRoot(r backupRoot) error
	applyRootSecurity(r backupRoot) error
	verifyRootSecurity(r backupRoot) error
	// createFile creates a new file named name under root. It returns
	// errBackupExists when the bare name is already taken.
	createFile(r backupRoot, name string) (backupFile, error)
	// verifyFile confirms f is a regular, non-reparse file created under root
	// with the given bare name.
	verifyFile(f backupFile, r backupRoot, name string) error
	applyFileSecurity(f backupFile) error
	verifyFileSecurity(f backupFile) error
	// write writes all of data. A partial write is an error.
	write(f backupFile, data []byte) error
	sync(f backupFile) error
	// deleteOnClose marks f for deletion on close (identity-safe). It is only
	// called while the handle is still valid; there is no post-close path
	// re-resolution fallback.
	deleteOnClose(f backupFile) error
	// retain performs the existing retention policy while r remains the verified
	// root owner. Implementations must refuse retention if root identity changed.
	retain(r backupRoot, dir, protected string) error
	// close closes the file (if any) and the root handle exactly once.
	close(r backupRoot, f backupFile) error
}

// CreateBackup writes data to a fresh create_new backup (never overwrites),
// trims the directory to the newest maxBackups, and returns the full path.
func CreateBackup(dir string, data []byte) (string, error) {
	return createBackupWith(dir, data, createBackupPlatform())
}

// createBackupWith runs the platform ops in the fixed order required for a
// secret-safe backup: root open -> reparse/identity verify -> root security
// apply+verify -> create-new -> file verify -> file security apply+verify ->
// write -> sync -> close. The payload is never written before the file handle
// is fully verified.
func createBackupWith(dir string, data []byte, p backupPlatformOps) (path string, retErr error) {
	root, err := p.openRoot(dir)
	if err != nil {
		return "", err
	}
	var currentFile backupFile
	closed := false
	closeOnce := func() error {
		if closed {
			return nil
		}
		closed = true
		return p.close(root, currentFile)
	}
	defer func() {
		if err := closeOnce(); err != nil {
			path = ""
			retErr = errors.New(backupCleanupFailed)
		}
	}()
	if err := p.verifyRoot(root); err != nil {
		return "", err
	}
	if err := p.applyRootSecurity(root); err != nil {
		return "", err
	}
	if err := p.verifyRootSecurity(root); err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		name := backupName(time.Now())
		f, err := p.createFile(root, name)
		if err != nil {
			if !errors.Is(err, errBackupExists) {
				return "", err
			}
			// Same-millisecond collision with an existing backup: let the clock
			// tick on so the retry produces a fresh name.
			lastErr = err
			time.Sleep(time.Millisecond)
			continue
		}
		currentFile = f
		path := filepath.Join(dir, name)
		fail := func(cause error) (string, error) {
			deleteErr := p.deleteOnClose(f)
			closeErr := closeOnce()
			if deleteErr != nil || closeErr != nil {
				return "", errors.New(backupCleanupFailed)
			}
			return "", cause
		}
		if err := p.verifyFile(f, root, name); err != nil {
			return fail(err)
		}
		if err := p.applyFileSecurity(f); err != nil {
			return fail(err)
		}
		if err := p.verifyFileSecurity(f); err != nil {
			return fail(err)
		}
		if err := p.write(f, data); err != nil {
			return fail(err)
		}
		if err := p.sync(f); err != nil {
			return fail(err)
		}
		if err := p.retain(root, dir, path); err != nil {
			_ = closeOnce()
			return "", errors.New(backupCleanupFailed)
		}
		if err := closeOnce(); err != nil {
			return "", errors.New(backupCleanupFailed)
		}
		return path, nil
	}
	return "", lastErr
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
