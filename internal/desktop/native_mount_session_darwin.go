//go:build darwin

package desktop

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cloudflare/artifact-fs/internal/fsbridge"
	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sys/unix"
)

const nativeMountSessionVersion = 1
const nativeMountSessionLimit = 16 << 10

var nativeMountSessionMu sync.Mutex

var errNativeMountSessionUnsafe = errors.New("the repository folder's private mount session is unsafe")
var errNativeMountSessionInvalid = errors.New("the repository folder's private mount session is invalid")
var errNativeMountSessionChanged = errors.New("the repository folder's private mount session changed")

// A session receipt records one verified kernel attachment. It is deliberately
// separate from the backing-root receipt: a prior mount is not evidence that a
// later kernel attachment or bridge belongs to this process.
type nativeMountSessionReceipt struct {
	Version      int                      `json:"version"`
	BootUUID     string                   `json:"boot_uuid"`
	Mount        nativeMountSessionMount  `json:"mount"`
	RootIdentity nativeRootObjectIdentity `json:"root_identity"`
	Bridge       fsbridge.SessionIdentity `json:"bridge"`
	fileIdentity nativeMountSessionFileIdentity
}

type nativeMountSessionMount struct {
	FSID     nativeMountSessionFSID `json:"fsid"`
	Owner    uint32                 `json:"owner"`
	TypeName string                 `json:"type_name"`
	Root     string                 `json:"root"`
	Source   string                 `json:"source"`
}

type nativeMountSessionFSID [2]int32

func (fsid *nativeMountSessionFSID) UnmarshalJSON(data []byte) error {
	var values []int32
	if err := json.Unmarshal(data, &values); err != nil || len(values) != 2 {
		return errNativeMountSessionInvalid
	}
	copy(fsid[:], values)
	return nil
}

type nativeMountSessionFileIdentity struct {
	device     uint64
	inode      uint64
	volumeUUID string
	birthSec   int64
	birthNSec  int64
}

func nativeMountSessionFileID(dir *os.File, stat *unix.Stat_t) (nativeMountSessionFileIdentity, error) {
	var parent unix.Stat_t
	if unix.Fstat(int(dir.Fd()), &parent) != nil || parent.Dev != stat.Dev {
		return nativeMountSessionFileIdentity{}, errNativeMountSessionUnsafe
	}
	volumeUUID, err := nativeBackingVolumeUUID(int(dir.Fd()))
	if err != nil || !(nativeRootObjectIdentity{VolumeUUID: volumeUUID, Inode: stat.Ino, BirthSec: stat.Btim.Sec, BirthNSec: stat.Btim.Nsec, UID: stat.Uid}).valid() {
		return nativeMountSessionFileIdentity{}, errNativeMountSessionUnsafe
	}
	return nativeMountSessionFileIdentity{device: uint64(stat.Dev), inode: stat.Ino, volumeUUID: volumeUUID,
		birthSec: stat.Btim.Sec, birthNSec: stat.Btim.Nsec}, nil
}

func newNativeMountSessionReceipt(bootUUID string, mount fsKitMountIdentity, root nativeRootObjectIdentity, bridge fsbridge.SessionIdentity) nativeMountSessionReceipt {
	return nativeMountSessionReceipt{Version: nativeMountSessionVersion, BootUUID: bootUUID,
		Mount:        nativeMountSessionMount{FSID: mount.fsid, Owner: mount.owner, TypeName: mount.typeName, Root: mount.root, Source: mount.source},
		RootIdentity: root, Bridge: bridge}
}

func (receipt nativeMountSessionReceipt) identity() fsKitMountIdentity {
	return fsKitMountIdentity{fsid: receipt.Mount.FSID, owner: receipt.Mount.Owner, typeName: receipt.Mount.TypeName,
		root: receipt.Mount.Root, source: receipt.Mount.Source}
}

func (receipt nativeMountSessionReceipt) valid(root string) bool {
	uuid, err := canonicalNativeBootUUID(receipt.BootUUID)
	return err == nil && uuid == receipt.BootUUID && receipt.Version == nativeMountSessionVersion &&
		filepath.IsAbs(root) && !strings.ContainsRune(root, 0) && "/"+model.CleanPath(root) == root && receipt.Mount.Root == root &&
		receipt.Mount.FSID != ([2]int32{}) && receipt.Mount.Owner == uint32(os.Geteuid()) && receipt.Mount.TypeName == "reporeach" &&
		receipt.RootIdentity.valid() && receipt.RootIdentity.UID == uint32(os.Geteuid()) &&
		receipt.Bridge.Validate(receipt.Bridge.SourceDirectory, receipt.Bridge.SocketDirectory) == nil &&
		mountSourceMatches(receipt.Mount.Source, receipt.Bridge.SourceDirectory)
}

func (receipt nativeMountSessionReceipt) sameSession(other nativeMountSessionReceipt) bool {
	receipt.fileIdentity = nativeMountSessionFileIdentity{}
	other.fileIdentity = nativeMountSessionFileIdentity{}
	return receipt == other
}

func nativeBootSessionUUID() (string, error) {
	uuid, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", errors.New("the host boot identity is unavailable")
	}
	return canonicalNativeBootUUID(uuid)
}

func canonicalNativeBootUUID(uuid string) (string, error) {
	if len(uuid) == 36 {
		if uuid[8] != '-' || uuid[13] != '-' || uuid[18] != '-' || uuid[23] != '-' {
			return "", errors.New("the host boot identity is invalid")
		}
		uuid = strings.ReplaceAll(uuid, "-", "")
	}
	if len(uuid) != 32 {
		return "", errors.New("the host boot identity is invalid")
	}
	value, err := hex.DecodeString(uuid)
	if err != nil {
		return "", errors.New("the host boot identity is invalid")
	}
	for _, b := range value {
		if b != 0 {
			return strings.ToLower(uuid), nil
		}
	}
	return "", errors.New("the host boot identity is invalid")
}

func nativeMountSessionPath(stateDir, root string) string {
	digest := sha256.Sum256([]byte(root))
	return filepath.Join(stateDir, "native-mount-sessions", hex.EncodeToString(digest[:])+".json")
}

// Opening and validating only the private state directory avoids resolving or
// touching the potentially unresponsive mounted root recorded in the receipt.
func openNativeMountSessionDirectory(stateDir string, create bool) (*os.File, error) {
	if err := nativePrivateReceiptDirectory(stateDir, false); err != nil {
		return nil, errNativeMountSessionUnsafe
	}
	var stateStat unix.Stat_t
	if unix.Lstat(stateDir, &stateStat) != nil || stateStat.Mode&0o7777 != 0o700 || stateStat.Uid != uint32(os.Geteuid()) || !nativeObjectACLFree(stateDir, &stateStat) {
		return nil, errNativeMountSessionUnsafe
	}
	path := filepath.Join(stateDir, "native-mount-sessions")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil, nil
		}
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	if err := nativePrivateReceiptDirectory(path, false); err != nil {
		return nil, errNativeMountSessionUnsafe
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errNativeMountSessionUnsafe
	}
	dir := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&0o7777 != 0o700 || stat.Uid != uint32(os.Geteuid()) ||
		!nativeObjectACLFree(path, &stat) || unix.Lstat(stateDir, &stateStat) != nil ||
		stateStat.Mode&0o7777 != 0o700 || stateStat.Uid != uint32(os.Geteuid()) || !nativeObjectACLFree(stateDir, &stateStat) {
		dir.Close()
		return nil, errNativeMountSessionUnsafe
	}
	if create {
		// Persist the receipt directory's entry in StateDir as well as the
		// receipt itself. Repeat on retries after an earlier sync failure.
		stateFD, err := unix.Open(stateDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			dir.Close()
			return nil, err
		}
		state := os.NewFile(uintptr(stateFD), stateDir)
		var opened unix.Stat_t
		if unix.Fstat(stateFD, &opened) != nil || opened.Dev != stateStat.Dev || opened.Ino != stateStat.Ino || !nativeObjectACLFree(stateDir, &opened) {
			state.Close()
			dir.Close()
			return nil, errNativeMountSessionUnsafe
		}
		err = state.Sync()
		state.Close()
		if err != nil {
			dir.Close()
			return nil, err
		}
	}
	return dir, nil
}

func readNativeMountSession(stateDir, root string) (*nativeMountSessionReceipt, error) {
	nativeMountSessionMu.Lock()
	defer nativeMountSessionMu.Unlock()
	dir, err := openNativeMountSessionDirectory(stateDir, false)
	if err != nil || dir == nil {
		return nil, err
	}
	defer dir.Close()
	return readNativeMountSessionAt(dir, root)
}

func readNativeMountSessionAt(dir *os.File, root string) (*nativeMountSessionReceipt, error) {
	name := filepath.Base(nativeMountSessionPath("", root))
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, errNativeMountSessionUnsafe
	}
	file := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name))
	defer file.Close()
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 ||
		stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || stat.Size < 0 || stat.Size > nativeMountSessionLimit || !nativeObjectACLFree(file.Name(), &stat) {
		return nil, errNativeMountSessionUnsafe
	}
	data, err := io.ReadAll(io.LimitReader(file, nativeMountSessionLimit+1))
	if err != nil || len(data) > nativeMountSessionLimit || validateNativeMountSessionJSON(data) != nil {
		return nil, errNativeMountSessionInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var receipt nativeMountSessionReceipt
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF || !receipt.valid(root) {
		return nil, errNativeMountSessionInvalid
	}
	var after unix.Stat_t
	if unix.Fstat(fd, &after) != nil {
		return nil, errNativeMountSessionChanged
	}
	// Reading can legitimately update access time without changing the record.
	after.Atim = stat.Atim
	if stat != after || !nativeObjectACLFree(file.Name(), &after) {
		return nil, errNativeMountSessionChanged
	}
	receipt.fileIdentity, err = nativeMountSessionFileID(dir, &stat)
	if err != nil {
		return nil, err
	}
	return &receipt, nil
}

// encoding/json otherwise accepts duplicate keys, including nested keys, and
// permits an earlier identity field to be silently replaced by a later one.
func validateNativeMountSessionJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var readValue func(int) error
	readValue = func(depth int) error {
		if depth > 32 {
			return errNativeMountSessionInvalid
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if token == nil {
			return errNativeMountSessionInvalid
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			keys := make(map[string]bool)
			for decoder.More() {
				token, err := decoder.Token()
				key, stringKey := token.(string)
				for _, character := range key {
					if character > 127 {
						return errNativeMountSessionInvalid
					}
				}
				key = strings.ToLower(key) // Decoder's struct matching is case insensitive.
				if err != nil || !stringKey || keys[key] {
					return errNativeMountSessionInvalid
				}
				keys[key] = true
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errNativeMountSessionInvalid
		}
		ending, err := decoder.Token()
		if err != nil || (delimiter == '{' && ending != json.Delim('}')) || (delimiter == '[' && ending != json.Delim(']')) {
			return errNativeMountSessionInvalid
		}
		return nil
	}
	if err := readValue(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errNativeMountSessionInvalid
	}
	return nil
}

// The caller must hold the desktop service lock and this session's live bridge
// lease (or recovery lease). The mutex serializes this process's receipt calls;
// it cannot serialize a cooperating helper that omitted those lifecycle locks.
// Replacing a valid same-root receipt requires the caller's independent proof
// that the previous kernel session no longer owns the attachment.
func writeNativeMountSession(stateDir string, receipt nativeMountSessionReceipt) error {
	if !receipt.valid(receipt.Mount.Root) {
		return errNativeMountSessionInvalid
	}
	data, err := json.Marshal(receipt)
	if err != nil || len(data)+1 > nativeMountSessionLimit {
		return errNativeMountSessionInvalid
	}
	data = append(data, '\n')
	nativeMountSessionMu.Lock()
	defer nativeMountSessionMu.Unlock()
	dir, err := openNativeMountSessionDirectory(stateDir, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	previous, err := readNativeMountSessionAt(dir, receipt.Mount.Root)
	if err != nil {
		return err // Preserve an unknown or malformed destination.
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	name := ".mount-session-" + hex.EncodeToString(random[:]) + ".tmp"
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name))
	defer file.Close()
	defer unix.Unlinkat(int(dir.Fd()), name, 0)
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&0o7777 != 0o600 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || !nativeObjectACLFree(file.Name(), &stat) {
		return errNativeMountSessionUnsafe
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	current, err := readNativeMountSessionAt(dir, receipt.Mount.Root)
	if err != nil || (previous == nil) != (current == nil) || (previous != nil &&
		(!previous.sameSession(*current) || previous.fileIdentity != current.fileIdentity)) {
		return errNativeMountSessionChanged
	}
	destination := filepath.Base(nativeMountSessionPath("", receipt.Mount.Root))
	if previous == nil {
		// An absent destination must remain absent through the atomic publish.
		err = unix.RenameatxNp(int(dir.Fd()), name, int(dir.Fd()), destination, unix.RENAME_EXCL)
	} else {
		err = unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), destination)
	}
	if err != nil {
		return err
	}
	return dir.Sync()
}

// The caller retains the service and bridge/recovery lease and independently
// proves kernel absence before retiring the exact recorded session. A receipt
// obtained by read additionally binds removal to that particular receipt inode.
func removeNativeMountSession(stateDir, root string, expected nativeMountSessionReceipt) error {
	if !expected.valid(root) {
		return errNativeMountSessionInvalid
	}
	nativeMountSessionMu.Lock()
	defer nativeMountSessionMu.Unlock()
	dir, err := openNativeMountSessionDirectory(stateDir, false)
	if err != nil || dir == nil {
		return err
	}
	defer dir.Close()
	current, err := readNativeMountSessionAt(dir, root)
	if err != nil {
		return err
	}
	if current == nil {
		return dir.Sync() // Retry durability after a prior unlink succeeded.
	}
	if !expected.sameSession(*current) || (expected.fileIdentity.inode != 0 && expected.fileIdentity != current.fileIdentity) {
		return errNativeMountSessionChanged
	}
	name := filepath.Base(nativeMountSessionPath("", root))
	var stat unix.Stat_t
	if unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return errNativeMountSessionChanged
	}
	identity, err := nativeMountSessionFileID(dir, &stat)
	if err != nil || current.fileIdentity != identity || !nativeObjectACLFree(filepath.Join(dir.Name(), name), &stat) {
		return errNativeMountSessionChanged
	}
	if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil {
		return err
	}
	return dir.Sync()
}
