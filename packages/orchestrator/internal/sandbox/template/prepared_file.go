package template

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const preparedSnapshotPrefix = ".stratovirt-snapshot-"
const preparedSnapshotOwner = ".owner.lock"

// PreparedFile keeps an immutable expanded snapshot for the template lifetime.
// Each restore takes hard links, so template eviction cannot remove its files.
// Consumers must only read the snapshot or map it privately; never modify or truncate it.
type PreparedFile struct {
	File
	mu     sync.Mutex
	dir    string
	owner  *os.File
	closed bool
}

// PrepareDir expands the immutable snapshot before any restore needs it.
// The cache parent must also hold the restore directories so hard links work.
func (f *PreparedFile) PrepareDir(parent string, prepare func(string, string) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepareDir(parent, prepare)
}

func (f *PreparedFile) prepareDir(parent string, prepare func(string, string) error) error {
	if f.closed {
		return os.ErrClosed
	}
	if f.dir != "" {
		return nil
	}
	// Publish the cache only after holding its owner lock. Startup cleanup
	// must never mistake an in-progress preparation for an abandoned cache.
	staging, err := os.MkdirTemp(parent, ".stratovirt-prepare-")
	if err != nil {
		return err
	}
	owner, err := os.OpenFile(filepath.Join(staging, preparedSnapshotOwner), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return errors.Join(err, os.RemoveAll(staging))
	}
	defer func() {
		if f.owner != owner {
			_ = owner.Close()
			_ = os.RemoveAll(staging)
		}
	}()
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	dir := filepath.Join(parent, preparedSnapshotPrefix+strings.TrimPrefix(filepath.Base(staging), ".stratovirt-prepare-"))
	if err := os.Rename(staging, dir); err != nil {
		return err
	}
	if err := prepare(f.Path(), dir); err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	for _, entry := range entries {
		if entry.Name() == preparedSnapshotOwner {
			continue
		}
		if !entry.Type().IsRegular() {
			return errors.Join(fmt.Errorf("non-regular prepared snapshot entry %q", entry.Name()), os.RemoveAll(dir))
		}
		if err := os.Chmod(filepath.Join(dir, entry.Name()), 0o400); err != nil {
			return errors.Join(err, os.RemoveAll(dir))
		}
	}
	f.dir = dir
	f.owner = owner
	return nil
}

func (f *PreparedFile) LinkPreparedDir(target string, prepare func(string, string) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.prepareDir(filepath.Dir(target), prepare); err != nil {
		return err
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == preparedSnapshotOwner {
			continue
		}
		if err := os.Link(filepath.Join(f.dir, entry.Name()), filepath.Join(target, entry.Name())); err != nil {
			return fmt.Errorf("link prepared snapshot %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func (f *PreparedFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	var err error
	if f.dir != "" {
		err = os.RemoveAll(f.dir)
	}
	if f.owner != nil {
		err = errors.Join(err, f.owner.Close())
	}
	return errors.Join(err, f.File.Close())
}

// A process exit releases flock even when the normal shutdown callback did not run.
// Existing restore links and VMM mappings remain valid after an orphan is removed.
func cleanupPreparedSnapshots(parent string) error {
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var cleanupErr error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), preparedSnapshotPrefix) {
			continue
		}
		dir := filepath.Join(parent, entry.Name())
		owner, err := os.OpenFile(filepath.Join(dir, preparedSnapshotOwner), os.O_RDWR|unix.O_NOFOLLOW, 0)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("open prepared snapshot owner %s: %w", dir, err))
			continue
		}
		err = unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(err, unix.EWOULDBLOCK) {
			_ = owner.Close()
			continue
		}
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("lock prepared snapshot %s: %w", dir, errors.Join(err, owner.Close())))
			continue
		}
		removeErr := os.RemoveAll(dir)
		closeErr := owner.Close()
		if err := errors.Join(removeErr, closeErr); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove prepared snapshot %s: %w", dir, err))
		}
	}
	return cleanupErr
}
