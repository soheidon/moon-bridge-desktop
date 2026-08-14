//go:build windows

package codexconfig

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsBackupStructSizes(t *testing.T) {
	if got := unsafe.Sizeof(fileIDInfo{}); got != 24 {
		t.Fatalf("fileIDInfo size=%d", got)
	}
	if got := unsafe.Sizeof(fileDispositionInfo{}); got != 1 {
		t.Fatalf("fileDispositionInfo size=%d", got)
	}
}

func TestWindowsValidBareNameMatrix(t *testing.T) {
	valid := "20260805T103040123Z-config.toml"
	if !validBareName(valid) {
		t.Fatal("valid bare name rejected")
	}
	for _, name := range []string{"", ".", "..", `C:\x`, `C:foo`, `\\server\share`, "a/b", `a\b`, "a\x00", "a\x1f", "a\x7f", "a.", "a "} {
		if validBareName(name) {
			t.Fatalf("invalid bare name accepted: %q", name)
		}
	}
}

func TestWindowsTrustedBaseBoundaryFailClosed(t *testing.T) {
	base := t.TempDir()
	before, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	p := windowsBackupPlatform{trustedBase: base}
	for _, dir := range []string{base, filepath.Join(base, "..", filepath.Base(base)), "relative-backup"} {
		if _, err := p.openRoot(dir); err == nil {
			t.Fatalf("openRoot accepted unsafe dir %q", dir)
		}
	}
	if _, err := (windowsBackupPlatform{trustedBase: "relative-base"}).openRoot(filepath.Join(base, "child")); err == nil {
		t.Fatal("relative trusted base accepted")
	}
	after, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("trusted base changed: before=%d after=%d", len(before), len(after))
	}
}

func TestWindowsCreateBackupRealAPIAndDACL(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "Moon Bridge", "backups", "codex-config")
	payload := []byte("windows-backup-payload")
	path, err := createBackupWith(dir, payload, windowsBackupPlatform{trustedBase: base})
	if err != nil {
		t.Fatalf("createBackupWith: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("backup content mismatch")
	}
	if sha256.Sum256(got) != sha256.Sum256(payload) {
		t.Fatal("backup hash mismatch")
	}
	if len(got) != len(payload) {
		t.Fatalf("backup size=%d, want %d", len(got), len(payload))
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	p := windowsBackupPlatform{trustedBase: base}
	rootAny, err := p.openRoot(filepath.Join(base, "acl-root"))
	if err != nil {
		t.Fatal(err)
	}
	root := rootAny.(*windowsBackupRoot)
	defer p.close(root, nil)
	if err := p.verifyRoot(root); err != nil {
		t.Fatal(err)
	}
	if err := p.applyRootSecurity(root); err != nil {
		t.Fatal(err)
	}
	if err := p.verifyRootSecurity(root); err != nil {
		t.Fatal(err)
	}
	if err := verifySecurity(root.handle, true); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsDACLHardeningRemovesBroadAllow(t *testing.T) {
	base := t.TempDir()
	p := windowsBackupPlatform{trustedBase: base}
	rootAny, err := p.openRoot(filepath.Join(base, "hardening"))
	if err != nil {
		t.Fatal(err)
	}
	root := rootAny.(*windowsBackupRoot)
	defer p.close(root, nil)
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(world),
		},
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(root.handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.applyRootSecurity(root); err != nil {
		t.Fatal(err)
	}
	if err := p.verifyRootSecurity(root); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsJunctionRejectedWithoutTargetMutation(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "junction")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "pre.txt"), []byte("pre-existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J failed: %v (%s)", err, out)
	}
	_, err := createBackupWith(filepath.Join(link, "backups"), []byte("secret-sentinel"), windowsBackupPlatform{trustedBase: base})
	if err == nil {
		t.Fatal("junction accepted")
	}
	if strings.Contains(err.Error(), "secret-sentinel") || strings.Contains(strings.ToLower(err.Error()), "target") {
		t.Fatal("reparse error leaked sensitive details")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("junction target changed: %v", entries)
	}
	if got, err := os.ReadFile(filepath.Join(target, "pre.txt")); err != nil || string(got) != "pre-existing" {
		t.Fatalf("junction target file mutated: %q err=%v", got, err)
	}
}

func TestWindowsRootSymlinkRejectedWithoutTargetMutation(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "rootlink")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Developer mode is enabled in this environment, so os.Symlink on a directory
	// target creates a directory symlink without elevation (proven by D.52's file
	// symlink). The root-open descent must reject it without writing to target.
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("os.Symlink failed (developer mode required): %v", err)
	}
	_, err := createBackupWith(filepath.Join(link, "backups"), []byte("secret-sentinel"), windowsBackupPlatform{trustedBase: base})
	if err == nil {
		t.Fatal("symlink root accepted")
	}
	if strings.Contains(err.Error(), "secret-sentinel") || strings.Contains(strings.ToLower(err.Error()), "target") {
		t.Fatal("reparse error leaked sensitive details")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("symlink target changed: %v", entries)
	}
	if got, err := os.ReadFile(filepath.Join(target, "keep.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("symlink target file mutated: %q err=%v", got, err)
	}
}

func TestWindowsReparseHelperUsesAttributeOnly(t *testing.T) {
	for _, attrs := range []uint32{
		windows.FILE_ATTRIBUTE_REPARSE_POINT,
		windows.FILE_ATTRIBUTE_REPARSE_POINT | 0x00000010,
		windows.FILE_ATTRIBUTE_REPARSE_POINT | 0x00000020,
	} {
		if !isReparsePoint(attrs) {
			t.Fatalf("reparse attributes not rejected: %#x", attrs)
		}
	}
	if isReparsePoint(windows.FILE_ATTRIBUTE_DIRECTORY) {
		t.Fatal("directory attribute treated as reparse")
	}
}

func TestWindowsDACLHardeningReplacesInheritedBroadAllow(t *testing.T) {
	for _, tc := range []struct {
		label     string
		wellKnown windows.WELL_KNOWN_SID_TYPE
	}{
		{"Everyone", windows.WinWorldSid},
		{"Users", windows.WinBuiltinUsersSid},
		{"Authenticated Users", windows.WinAuthenticatedUserSid},
	} {
		t.Run(tc.label, func(t *testing.T) {
			base := t.TempDir()
			sid, err := windows.CreateWellKnownSid(tc.wellKnown)
			if err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(base, "parent")
			child := filepath.Join(parent, "backups")
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			pParent, err := windows.UTF16PtrFromString(parent)
			if err != nil {
				t.Fatal(err)
			}
			ph, err := windows.CreateFile(pParent, windows.READ_CONTROL|windows.WRITE_DAC,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
				windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
			if err != nil {
				t.Fatal(err)
			}
			acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
				AccessPermissions: windows.GENERIC_READ,
				AccessMode:        windows.GRANT_ACCESS,
				Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
				Trustee: windows.TRUSTEE{
					TrusteeForm:  windows.TRUSTEE_IS_SID,
					TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
					TrusteeValue: windows.TrusteeValueFromSID(sid),
				},
			}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := windows.SetSecurityInfo(ph, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
				t.Fatal(err)
			}
			windows.CloseHandle(ph)
			if err := os.Mkdir(child, 0o755); err != nil {
				t.Fatal(err)
			}
			p := windowsBackupPlatform{trustedBase: base}
			rootAny, err := p.openRoot(child)
			if err != nil {
				t.Fatal(err)
			}
			root := rootAny.(*windowsBackupRoot)
			defer p.close(root, nil)
			if err := verifySecurity(root.handle, true); err == nil {
				t.Fatal("inherited broad Allow survived into the child DACL")
			}
			if err := p.applyRootSecurity(root); err != nil {
				t.Fatal(err)
			}
			if err := p.verifyRootSecurity(root); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWindowsCreateFileRejectsReparseAtName(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "backups")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := backupName(backupTimes[0])
	link := filepath.Join(dir, name)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J failed: %v (%s)", err, out)
	}
	p := windowsBackupPlatform{trustedBase: base}
	rootAny, err := p.openRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := rootAny.(*windowsBackupRoot)
	defer p.close(root, nil)
	// NtCreateFile with FILE_CREATE on an existing junction returns
	// STATUS_FILE_IS_A_DIRECTORY (0xc00000ba) — a non-retryable create failure,
	// never a write through the reparse point.
	if _, err := p.createFile(root, name); err == nil {
		t.Fatal("reparse at backup filename was accepted")
	}
	if got, err := os.ReadFile(filepath.Join(target, "keep.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("junction target mutated: %q err=%v", got, err)
	}
	pLink, err := windows.UTF16PtrFromString(link)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := windows.GetFileAttributes(pLink)
	if err != nil {
		t.Fatalf("junction was removed: %v", err)
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		t.Fatalf("junction replaced with a non-reparse object: attrs=%#x", attrs)
	}
}

func TestWindowsVerifyFileRejectsReparseHandle(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real.txt")
	if err := os.WriteFile(real, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("file symlink failed (developer mode required): %v", err)
	}
	p := windowsBackupPlatform{trustedBase: base}
	pLink, err := windows.UTF16PtrFromString(link)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(pLink, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := p.verifyFile(&windowsBackupFile{handle: h}, nil, ""); !errors.Is(err, errFileReparse) {
		t.Fatalf("verifyFile returned %v, want errFileReparse", err)
	}
}

func TestWindowsDirectorySwapCannotRedirectCreateOrWrite(t *testing.T) {
	base := t.TempDir()
	rootDir := filepath.Join(base, "backups")
	if err := os.Mkdir(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "moved")
	p := windowsBackupPlatform{trustedBase: base}
	rootAny, err := p.openRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	root := rootAny.(*windowsBackupRoot)
	defer p.close(root, nil)
	if err := p.verifyRoot(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "unrelated.txt"), []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rootDir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(rootDir, "decoy.txt")
	if err := os.WriteFile(decoy, []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "swaptest.toml"
	f, err := p.createFile(root, name)
	if err != nil {
		t.Fatal(err)
	}
	file := f.(*windowsBackupFile)
	defer p.close(root, file)
	if err := p.verifyFile(f, root, name); err != nil {
		t.Fatal(err)
	}
	if err := p.write(f, []byte("secret-payload")); err != nil {
		t.Fatal(err)
	}
	if err := p.sync(f); err != nil {
		t.Fatal(err)
	}
	if err := p.close(root, file); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(moved, name))
	if err != nil || string(got) != "secret-payload" {
		t.Fatalf("payload not written through the pinned handle: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, name)); !os.IsNotExist(err) {
		t.Fatalf("decoy dir received the backup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(moved, "unrelated.txt")); err != nil || string(got) != "unrelated" {
		t.Fatalf("unrelated file in pinned dir changed: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(decoy); err != nil || string(got) != "decoy" {
		t.Fatalf("decoy dir content changed: %q err=%v", got, err)
	}
}

// TestWindowsDeletePendingKeepsProtectedArtifact is AUXILIARY evidence for
// G.80 (the normal deletion path), not a substitute for delete-failure proof.
// It shows the protected DACL is applied to a real artifact (applyFileSecurity
// + verifyFileSecurity + verifySecurity) and survives a pending delete until
// the last handle closes. The delete-FAILURE path itself is pinned by
// TestCreateBackupWithDeleteFailureKeepsArtifactAndIsCleanupSafe via the fake
// seam; together they form the synthetic evidence for G.80, because a real-API
// delete failure cannot be induced in-process (bidirectional sharing rule).
func TestWindowsHandleResolvedStrictChild(t *testing.T) {
	for _, tc := range []struct {
		label    string
		base     string
		root     string
		wantPass bool
	}{
		{"strict child (base without trailing sep)", `\\?\c:\users\sohei\appdata\local`, `\\?\c:\users\sohei\appdata\local\moon bridge`, true},
		{"strict child (base with trailing sep)", `\\?\c:\`, `\\?\c:\safe`, true},
		{"exact match", `\\?\c:\users\sohei\appdata\local`, `\\?\c:\users\sohei\appdata\local`, false},
		{"root shorter", `\\?\c:\users\sohei\appdata\local`, `\\?\c:\users\sohei\appdata`, false},
		{"prefix false match (no separator boundary)", `\\?\c:\safe`, `\\?\c:\safe2`, false},
		{"different volume", `\\?\c:\safe`, `\\?\d:\safe\child`, false},
		{"redirected: base=packages, root=packages child",
			`\\?\c:\users\sohei\appdata\local\packages\claude_pzs8sxrjxfjjc\localcache\local`,
			`\\?\c:\users\sohei\appdata\local\packages\claude_pzs8sxrjxfjjc\localcache\local\moon bridge`,
			true},
		{"redirected mismatch: base=logical, root=packages child",
			`\\?\c:\users\sohei\appdata\local`,
			`\\?\c:\users\sohei\appdata\local\packages\claude_pzs8sxrjxfjjc\localcache\local\moon bridge`,
			true},
		{"redirected escape: root not under base",
			`\\?\c:\users\sohei\appdata\local\packages\claude_pzs8sxrjxfjjc\localcache\local`,
			`\\?\c:\users\sohei\appdata\local\moon bridge`,
			false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			got := handleResolvedStrictChild(tc.base, tc.root)
			if got != tc.wantPass {
				t.Fatalf("handleResolvedStrictChild(%q, %q) = %v, want %v",
					tc.base, tc.root, got, tc.wantPass)
			}
		})
	}
}

// TestWindowsVerifyRootVolumeSerialRejection verifies that verifyRoot rejects
// a root whose handle-derived VolumeSerialNumber does not match the trusted
// base's. Because openRoot always sets baseVolumeSerial from the real anchor,
// we mutate the field after openRoot to simulate a volume mismatch. This
// confirms production compares serials directly — not merely drive-letter
// strings.
func TestWindowsVerifyRootVolumeSerialRejection(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "vol-child")
	p := windowsBackupPlatform{trustedBase: base}
	rootAny, err := p.openRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := rootAny.(*windowsBackupRoot)
	defer p.close(root, nil)
	// verifyRoot succeeds with the real serial.
	if err := p.verifyRoot(root); err != nil {
		t.Fatalf("verifyRoot failed with real serial: %v", err)
	}
	// Mutate to a different serial to verify the serial check fires.
	original := root.baseVolumeSerial
	root.baseVolumeSerial = original + 1
	err = p.verifyRoot(root)
	if err == nil {
		t.Fatal("verifyRoot accepted mismatched volume serial")
	}
	// Restore and confirm success again.
	root.baseVolumeSerial = original
	if err := p.verifyRoot(root); err != nil {
		t.Fatalf("verifyRoot failed after restoring serial: %v", err)
	}
}

func TestWindowsDeletePendingKeepsProtectedArtifact(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "backups")
	p := windowsBackupPlatform{trustedBase: base}
	rootAny, err := p.openRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := rootAny.(*windowsBackupRoot)
	name := backupName(backupTimes[0])
	f, err := p.createFile(root, name)
	if err != nil {
		t.Fatal(err)
	}
	file := f.(*windowsBackupFile)
	defer p.close(root, file)
	if err := p.verifyFile(f, root, name); err != nil {
		t.Fatal(err)
	}
	if err := p.applyFileSecurity(f); err != nil {
		t.Fatal(err)
	}
	if err := p.verifyFileSecurity(f); err != nil {
		t.Fatal(err)
	}
	if err := p.write(f, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := p.sync(f); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	// The backup file handle always holds DELETE access, so any coexisting handle
	// must grant FILE_SHARE_DELETE (bidirectional sharing rule); a delete-pending
	// artifact therefore stays on disk, fully protected, until every handle
	// closes. An obstructed deletion must never destroy the artifact or its DACL.
	pPath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := windows.CreateFile(pPath, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.deleteOnClose(f); err != nil {
		t.Fatalf("delete-on-close failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("protected artifact was removed while deletion was pending")
	}
	if err := verifySecurity(file.handle, false); err != nil {
		t.Fatalf("protected artifact lost its DACL while deletion was pending: %v", err)
	}
	windows.CloseHandle(h2)
	if err := p.close(root, file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("artifact not removed after all handles closed: %v", err)
	}
}
