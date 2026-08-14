//go:build windows

package codexconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// FILE_ID_INFO is not exported by x/sys; class FileIdInfo (18) fills a 24-byte
// buffer laid out as VolumeSerialNumber (ULONGLONG) followed by FileId
// (128-bit). Size == 24 is pinned by a Windows test.
type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileId             [16]byte
}

// FILE_DISPOSITION_INFO is not exported by x/sys; class FileDispositionInfo (4)
// takes a 1-byte BOOLEAN. Size == 1 is pinned by a Windows test.
type fileDispositionInfo struct {
	DeleteFile byte
}

// fileFullControl is the same mask used for every ACE: STANDARD_RIGHTS_REQUIRED
// (0x1F0000, includes DELETE/READ_CONTROL/WRITE_DAC/WRITE_OWNER) + SYNCHRONIZE
// (0x100000) + the file-specific 0x1FF. It is exactly 0x001F01FF.
const fileFullControl = 0x001F01FF

// fileAddFile, fileAddSubdir, and fileTraverse name the directory rights needed
// to descend into and create children via a root handle. FILE_ADD_FILE is the
// create synonym of FILE_WRITE_DATA (0x2) and FILE_ADD_SUBDIRECTORY of
// FILE_APPEND_DATA (0x4), which are the constants x/sys exposes.
const (
	fileAddFile   = 0x00000002 // FILE_ADD_FILE (== FILE_WRITE_DATA)
	fileAddSubdir = 0x00000004 // FILE_ADD_SUBDIRECTORY (== FILE_APPEND_DATA)
	fileTraverse  = 0x00000020 // FILE_TRAVERSE
)

// dirAccess is what every directory handle (trusted anchor, intermediate
// components, backup root) is opened with. It allows handle-relative descent
// and create (files and directories), plus DACL/owner management, identity
// reads, and attribute reads (GetFileInformationByHandle requires
// FILE_READ_ATTRIBUTES).
const dirAccess = windows.READ_CONTROL | windows.WRITE_DAC | windows.WRITE_OWNER |
	windows.FILE_LIST_DIRECTORY | fileAddFile | fileAddSubdir | fileTraverse |
	windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE

// windowsBackupPlatform carries the trusted base anchor for Windows.
type windowsBackupPlatform struct {
	trustedBase string
}

type windowsBackupRoot struct {
	handle           windows.Handle
	dir              string
	trustedBase      string
	identity         fileIDInfo
	basePhysical     string // handle-resolved final path of the trusted base
	baseVolumeSerial uint64 // volume serial from trusted base handle identity
}

type windowsBackupFile struct {
	handle windows.Handle
}

// createBackupPlatform resolves the trusted base from the same env chain as
// defaultBackupDir: %LOCALAPPDATA% -> APPDATA -> USERPROFILE. The anchor is the
// env root itself, which is guaranteed to exist on first launch; the app-owned
// directories below it are created handle-relatively, never absolute-open.
func createBackupPlatform() backupPlatformOps {
	return windowsBackupPlatform{trustedBase: windowsTrustedBase()}
}

func windowsTrustedBase() string {
	for _, k := range []string{"LOCALAPPDATA", "APPDATA", "USERPROFILE"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

var (
	errBackupDirOutsideTrusted   = errors.New("backup directory is outside the trusted base")
	errTrustedBaseUnavailable    = errors.New("trusted base is unavailable")
	errTrustedBaseNotDirectory   = errors.New("trusted base is not a directory")
	errTrustedBaseReparse        = errors.New("trusted base is a reparse point")
	errRootNotDirectory          = errors.New("backup root is not a directory")
	errRootReparse               = errors.New("backup root is a reparse point")
	errRootPathMismatch          = errors.New("backup root path mismatch")
	errInvalidRootComponent      = errors.New("invalid backup root component")
	errInvalidFileName           = errors.New("invalid backup file name")
	errFileNotRegular            = errors.New("backup file is not a regular file")
	errFileReparse               = errors.New("backup file is a reparse point")
	errPartialWrite              = errors.New("partial write")
	errSecurityOwnerMismatch     = errors.New("owner mismatch")
	errSecurityDACLNotProtected  = errors.New("DACL is not protected")
	errSecurityNoDACL            = errors.New("DACL is missing")
	errSecurityUnexpectedAce     = errors.New("unexpected ACE type")
	errSecurityInheritedAce      = errors.New("inherited ACE present")
	errSecurityUserMask          = errors.New("user ACE mask mismatch")
	errSecuritySystemMask        = errors.New("SYSTEM ACE mask mismatch")
	errSecurityRootInheritMiss   = errors.New("root ACE inheritance missing")
	errSecurityFileInheritPres   = errors.New("file ACE has inheritance flags")
	errSecurityUserMissing       = errors.New("user ACE missing")
	errSecuritySystemMissing     = errors.New("SYSTEM ACE missing")
	errSecurityUnexpectedTrustee = errors.New("unexpected trustee")
)

// isReparsePoint is the single reparse decision used by both verifyRoot and
// verifyFile. It branches only on FILE_ATTRIBUTE_REPARSE_POINT and never reads
// the IO_REPARSE_TAG, so junction, symlink, and any other reparse tag take the
// exact same rejection path. D.46/D.48 pin this single-branch behavior.
func isReparsePoint(attributes uint32) bool {
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func (p windowsBackupPlatform) openRoot(dir string) (backupRoot, error) {
	if p.trustedBase == "" || !filepath.IsAbs(p.trustedBase) || filepath.VolumeName(p.trustedBase) == "" {
		return nil, errTrustedBaseUnavailable
	}
	if !filepath.IsAbs(dir) || filepath.VolumeName(dir) == "" || !sameVolume(p.trustedBase, dir) {
		return nil, errBackupDirOutsideTrusted
	}
	rel, err := filepath.Rel(p.trustedBase, dir)
	if err != nil {
		return nil, errBackupDirOutsideTrusted
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, errBackupDirOutsideTrusted
	}
	base, basePhysical, baseID, err := openTrustedAnchor(p.trustedBase)
	if err != nil {
		return nil, err
	}
	current := base
	ok := false
	baseClosed := false
	currentClosed := false
	defer func() {
		if !ok {
			if !currentClosed {
				windows.CloseHandle(current)
			}
			if current != base && !baseClosed {
				windows.CloseHandle(base)
			}
		}
	}()
	comps := strings.Split(rel, string(filepath.Separator))
	for _, comp := range comps {
		if !validBareName(comp) {
			return nil, errInvalidRootComponent
		}
		next, err := openDirComponent(current, comp)
		if err != nil {
			return nil, err
		}
		if current != base {
			windows.CloseHandle(current)
		}
		current = next
	}
	// The anchor is only needed as the descent root; once the backup root handle
	// exists, close it so no anchor handle outlives the open.
	windows.CloseHandle(base)
	baseClosed = true
	identity, err := fileIdentity(current)
	if err != nil {
		windows.CloseHandle(current)
		currentClosed = true
		return nil, errIdentityFailed()
	}
	ok = true
	return &windowsBackupRoot{handle: current, dir: dir, trustedBase: p.trustedBase, identity: identity, basePhysical: basePhysical, baseVolumeSerial: baseID.VolumeSerialNumber}, nil
}

func sameVolume(a, b string) bool {
	return strings.EqualFold(filepath.VolumeName(a), filepath.VolumeName(b))
}

// verifyRoot pins the backup root handle: directory, non-reparse, FILE_ID_INFO
// obtainable, handle-resolved final path is a strict child of the trusted
// base's handle-resolved final path, and the handle-derived volume serial
// numbers match.
func (windowsBackupPlatform) verifyRoot(r backupRoot) error {
	root := r.(*windowsBackupRoot)
	info, err := getFileInformation(root.handle)
	if err != nil {
		return errIdentityFailed()
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errRootNotDirectory
	}
	if isReparsePoint(info.FileAttributes) {
		return errRootReparse
	}
	rootID, err := fileIdentity(root.handle)
	if err != nil {
		return errIdentityFailed()
	}
	if rootID.VolumeSerialNumber != root.baseVolumeSerial {
		return errRootPathMismatch
	}
	got, err := finalPathByHandle(root.handle)
	if err != nil {
		return errIdentityFailed()
	}
	if !handleResolvedStrictChild(root.basePhysical, got) {
		return errRootPathMismatch
	}
	return nil
}

func (windowsBackupPlatform) applyRootSecurity(r backupRoot) error {
	root := r.(*windowsBackupRoot)
	if err := applyProtectedDACL(root.handle, true); err != nil {
		return errSecurityFailed()
	}
	return nil
}

func (windowsBackupPlatform) verifyRootSecurity(r backupRoot) error {
	root := r.(*windowsBackupRoot)
	if err := verifySecurity(root.handle, true); err != nil {
		return errSecurityFailed()
	}
	return nil
}

func (windowsBackupPlatform) createFile(r backupRoot, name string) (backupFile, error) {
	root := r.(*windowsBackupRoot)
	if !validBareName(name) {
		return nil, errInvalidFileName
	}
	// RootDirectory-relative create: ObjectName is the bare file name relative
	// to the already-verified root handle. No path resolution happens, so a
	// concurrent swap of the root directory cannot redirect this write.
	us, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: root.handle,
		ObjectName:    us,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var handle windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	status := windows.NtCreateFile(&handle, fileFullControl, oa, &iosb, nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE, // create-new only; never overwrites or opens existing
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0)
	if status != nil {
		if status == windows.STATUS_OBJECT_NAME_COLLISION {
			return nil, errBackupExists
		}
		// Return the raw NtCreateFile status. A reparse point (e.g. a junction)
		// sitting at the chosen name surfaces as STATUS_FILE_IS_A_DIRECTORY, not a
		// collision, and must propagate as a non-retryable create failure.
		return nil, status
	}
	return &windowsBackupFile{handle: handle}, nil
}

// verifyFile confirms the created handle is a regular, non-reparse file whose
// identity (FILE_ID_INFO) is obtainable. The handle was created with
// RootDirectory = root.handle, so it is necessarily the object we created; the
// identity match with the root is structural (no path re-resolution).
func (windowsBackupPlatform) verifyFile(f backupFile, _ backupRoot, _ string) error {
	file := f.(*windowsBackupFile)
	info, err := getFileInformation(file.handle)
	if err != nil {
		return errIdentityFailed()
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return errFileNotRegular
	}
	if isReparsePoint(info.FileAttributes) {
		return errFileReparse
	}
	if _, err := fileIdentity(file.handle); err != nil {
		return errIdentityFailed()
	}
	return nil
}

func (windowsBackupPlatform) applyFileSecurity(f backupFile) error {
	file := f.(*windowsBackupFile)
	if err := applyProtectedDACL(file.handle, false); err != nil {
		return errSecurityFailed()
	}
	return nil
}

func (windowsBackupPlatform) verifyFileSecurity(f backupFile) error {
	file := f.(*windowsBackupFile)
	if err := verifySecurity(file.handle, false); err != nil {
		return errSecurityFailed()
	}
	return nil
}

func (windowsBackupPlatform) write(f backupFile, data []byte) error {
	file := f.(*windowsBackupFile)
	var done uint32
	if err := windows.WriteFile(file.handle, data, &done, nil); err != nil {
		return errWriteFailed()
	}
	if int(done) != len(data) {
		return errWriteFailed()
	}
	return nil
}

func (windowsBackupPlatform) sync(f backupFile) error {
	file := f.(*windowsBackupFile)
	if err := windows.FlushFileBuffers(file.handle); err != nil {
		return errWriteFailed()
	}
	return nil
}

// deleteOnClose marks the file for deletion on close. It is only ever called
// while the file handle is still valid; there is no post-close path
// re-resolution fallback.
func (windowsBackupPlatform) deleteOnClose(f backupFile) error {
	file := f.(*windowsBackupFile)
	info := fileDispositionInfo{DeleteFile: 1}
	if err := windows.SetFileInformationByHandle(file.handle, windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return err
	}
	return nil
}

func (windowsBackupPlatform) retain(r backupRoot, dir string, protected []string) error {
	root := r.(*windowsBackupRoot)
	current, err := fileIdentity(root.handle)
	if err != nil || current != root.identity {
		return errIdentityFailed()
	}
	candidate, err := (windowsBackupPlatform{trustedBase: root.trustedBase}).openRoot(dir)
	if err != nil {
		return errIdentityFailed()
	}
	candidateRoot := candidate.(*windowsBackupRoot)
	candidateID := candidateRoot.identity
	closeErr := (windowsBackupPlatform{}).close(candidate, nil)
	if closeErr != nil {
		return errIdentityFailed()
	}
	if candidateID != root.identity {
		return errIdentityFailed()
	}
	retainConfigBackups(dir, protected...)
	return nil
}

// close releases the file handle (if any) and then the root handle. It is
// idempotent and never assumes handle validity after a CloseHandle attempt, so
// no double-close or post-close handle use can occur.
func (windowsBackupPlatform) close(r backupRoot, f backupFile) error {
	var firstErr error
	zero := func(h *windows.Handle) {
		if *h == 0 {
			return
		}
		if err := windows.CloseHandle(*h); err != nil && firstErr == nil {
			firstErr = err
		}
		*h = 0
	}
	if f != nil {
		file := f.(*windowsBackupFile)
		zero(&file.handle)
	}
	if r != nil {
		root := r.(*windowsBackupRoot)
		zero(&root.handle)
	}
	return firstErr
}

func errIdentityFailed() error { return errors.New(backupIdentityFailed) }
func errSecurityFailed() error { return errors.New(backupSecurityFailed) }
func errWriteFailed() error    { return errors.New(backupWriteFailed) }

// openTrustedAnchor opens the trusted base and verifies the anchor conditions
// before it may be used as the root of handle-relative descent: directory,
// non-reparse, FILE_ID_INFO obtainable. The handle-resolved final path and
// file identity are returned so the caller can perform handle-based containment
// and volume-identity checks after descent.
func openTrustedAnchor(base string) (windows.Handle, string, fileIDInfo, error) {
	p, err := windows.UTF16PtrFromString(base)
	if err != nil {
		return 0, "", fileIDInfo{}, err
	}
	h, err := windows.CreateFile(p, dirAccess,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, "", fileIDInfo{}, err
	}
	ok := false
	defer func() {
		if !ok {
			windows.CloseHandle(h)
		}
	}()
	info, err := getFileInformation(h)
	if err != nil {
		return 0, "", fileIDInfo{}, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return 0, "", fileIDInfo{}, errTrustedBaseNotDirectory
	}
	if isReparsePoint(info.FileAttributes) {
		return 0, "", fileIDInfo{}, errTrustedBaseReparse
	}
	got, err := finalPathByHandle(h)
	if err != nil {
		return 0, "", fileIDInfo{}, err
	}
	baseID, err := fileIdentity(h)
	if err != nil {
		return 0, "", fileIDInfo{}, err
	}
	ok = true
	return h, got, baseID, nil
}

// openDirComponent opens (creating if needed) one bare-name component relative
// to an already-verified parent handle, then confirms it is a non-reparse
// directory. FILE_OPEN_REPARSE_POINT means a junction/symlink component opens
// as the link itself and is rejected below rather than being traversed.
func openDirComponent(parent windows.Handle, comp string) (windows.Handle, error) {
	us, err := windows.NewNTUnicodeString(comp)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    us,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	status := windows.NtCreateFile(&h, dirAccess, oa, &iosb, nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN_IF, // create-or-open directory component
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0)
	if status != nil {
		return 0, status
	}
	info, err := getFileInformation(h)
	if err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || isReparsePoint(info.FileAttributes) {
		windows.CloseHandle(h)
		return 0, errInvalidRootComponent
	}
	return h, nil
}

// applyProtectedDACL replaces the object's owner and DACL with a fresh ACL
// containing only the current user and SYSTEM (full control), with the DACL
// protected from inheritance. Root ACEs inherit to children; file ACEs do not.
func applyProtectedDACL(h windows.Handle, root bool) error {
	userSID, err := currentUserSID()
	if err != nil {
		return err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	inheritance := uint32(0)
	if root {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: fileFullControl,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(userSID),
		},
	}}
	if !systemSID.Equals(userSID) {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: fileFullControl,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		})
	}
	// merged=nil builds a new ACL from only these explicit ACEs, so inherited
	// ACEs from a broad parent DACL never survive into the protected DACL.
	// x/sys ACLFromEntries frees its internal win-heap ACL and returns a
	// Go-managed copy, so no LocalFree is required or permitted here.
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		userSID, nil, acl, nil)
}

// verifySecurity re-reads the object's security and checks the protected DACL
// semantically: owner is the current user, DACL present and protected, exactly
// user (full control) and SYSTEM (full control) ACEs with no inherited or
// unexpected entries, and root ACEs carrying OI|CI while file ACEs carry none.
func verifySecurity(h windows.Handle, root bool) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	userSID, err := currentUserSID()
	if err != nil {
		return err
	}
	if !owner.Equals(userSID) {
		return errSecurityOwnerMismatch
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PRESENT == 0 || control&windows.SE_DACL_PROTECTED == 0 {
		return errSecurityDACLNotProtected
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errSecurityNoDACL
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	expectedFlags := byte(0)
	if root {
		expectedFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	expectedACECount := uint32(1)
	if !systemSID.Equals(userSID) {
		expectedACECount++
	}
	if uint32(dacl.AceCount) != expectedACECount {
		return errSecurityUnexpectedAce
	}
	seenUser, seenSystem := false, false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != expectedFlags {
			return errSecurityUnexpectedAce
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case aceSID.Equals(userSID):
			if seenUser || ace.Mask != fileFullControl {
				return errSecurityUserMask
			}
			seenUser = true
		case aceSID.Equals(systemSID):
			if seenSystem || ace.Mask != fileFullControl {
				return errSecuritySystemMask
			}
			seenSystem = true
		default:
			return errSecurityUnexpectedTrustee
		}
	}
	if !seenUser {
		return errSecurityUserMissing
	}
	if !systemSID.Equals(userSID) && !seenSystem {
		return errSecuritySystemMissing
	}
	return nil
}

func currentUserSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	// Keep the SID independent of the token-info buffer (no string round-trip).
	return user.User.Sid.Copy()
}

func getFileInformation(h windows.Handle) (*windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func fileIdentity(h windows.Handle) (fileIDInfo, error) {
	var id fileIDInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&id)), uint32(unsafe.Sizeof(id))); err != nil {
		return fileIDInfo{}, err
	}
	return id, nil
}

// finalPathByHandle is diagnostic only: it resolves the handle to its final
// path so a mismatch with the caller's canonical path reveals an ancestor
// reparse/swap. The result is lowercased to match canonicalPath's case-
// insensitive comparison. It is never used as the identity proof.
func finalPathByHandle(h windows.Handle) (string, error) {
	n, err := windows.GetFinalPathNameByHandle(h, nil, 0, 0)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, n+1)
	n2, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", err
	}
	return strings.ToLower(windows.UTF16ToString(buf[:n2])), nil
}

// canonicalPath normalizes a caller path into the extended-length, lowercased
// form that GetFinalPathNameByHandle returns, without ever resolving reparse
// points (EvalSymlinks and reparse-resolved expected values are forbidden).
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	s := strings.ToLower(filepath.Clean(abs))
	// Strip any extended-length prefix to a plain form first.
	if strings.HasPrefix(s, `\\?\unc\`) {
		s = `\\` + s[len(`\\?\unc\`):]
	} else if strings.HasPrefix(s, `\\?\`) {
		s = s[len(`\\?\`):]
	}
	// UNC: \\server\share\... -> \\?\UNC\server\share\...
	if strings.HasPrefix(s, `\\`) {
		s = `\\?\UNC` + s[1:]
		if strings.TrimRight(s[len(`\\?\UNC`):], `\`) == "" {
			return s // share root keeps its trailing separator
		}
		return strings.TrimRight(s, `\`)
	}
	// Drive path: c:\... -> \\?\c:\...
	vol := filepath.VolumeName(s)
	rest := strings.TrimRight(strings.TrimPrefix(s, vol), `\`)
	if rest == "" {
		return `\\?\` + vol + `\` // drive root keeps its trailing separator
	}
	return `\\?\` + vol + rest
}

// handleResolvedStrictChild reports whether finalRoot is a strict child path
// of finalBase, where both are handle-resolved final paths (lowercased,
// extended-length, no trailing separator except drive roots). The volume
// portion must match case-insensitively.
func handleResolvedStrictChild(finalBase, finalRoot string) bool {
	if len(finalRoot) <= len(finalBase) {
		return false
	}
	volBase := filepath.VolumeName(finalBase)
	volRoot := filepath.VolumeName(finalRoot)
	if !strings.EqualFold(volBase, volRoot) {
		return false
	}
	// Both paths are lowercased by finalPathByHandle, so case-sensitive prefix
	// check is sufficient. The base must be a strict prefix and the next char
	// must be a separator (prevents C:\safe matching C:\safe2).
	if !strings.HasPrefix(finalRoot, finalBase) {
		return false
	}
	after := finalRoot[len(finalBase):]
	if after == "" {
		return false
	}
	// The base may end with a trailing separator (drive root) or not.
	if after[0] == '\\' {
		return true
	}
	// If the base does not end with \, then after[0] != \ means no separator
	// boundary — false positive (e.g. C:\safe matching C:\safe2).
	// If the base ends without \, the base path itself already excluded the
	// trailing char, so we need to check the last char of finalBase.
	if finalBase[len(finalBase)-1] == '\\' {
		return true
	}
	return false
}

// validBareName rejects any component that cannot be a single path element:
// empty, "."/"..", embedded separators or colons, control characters, and
// trailing dots/spaces are all refused.
func validBareName(comp string) bool {
	if comp == "" || comp == "." || comp == ".." {
		return false
	}
	if strings.ContainsAny(comp, `\/:`) {
		return false
	}
	for i := 0; i < len(comp); i++ {
		c := comp[i]
		if c < 0x20 || c == 0x7f { // C0 controls and DEL are rejected
			return false
		}
	}
	last := comp[len(comp)-1]
	if last == '.' || last == ' ' {
		return false
	}
	return true
}
