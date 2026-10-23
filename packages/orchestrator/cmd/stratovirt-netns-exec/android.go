//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Android restore preparation creates a private tmpfs, three disk links,
// and host-visible FIFOs before entering the network namespace.
type prepareAndroidArgs struct {
	sandboxDir    string
	rootfsSrc     string
	persistentSrc string
	sdcardSrc     string
	pipeDir       string
}

func prepareAndroidMain(args []string) {
	fs := flag.NewFlagSet("prepare-android", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var prep prepareAndroidArgs
	var probe bool
	fs.StringVar(&prep.sandboxDir, "sandbox-dir", "", "sandbox tmpfs directory")
	fs.StringVar(&prep.rootfsSrc, "rootfs-src", "", "rootfs link source")
	fs.StringVar(&prep.persistentSrc, "persistent-src", "", "persistent disk link source")
	fs.StringVar(&prep.sdcardSrc, "sdcard-src", "", "sdcard link source")
	fs.StringVar(&prep.pipeDir, "pipe-dir", "", "host-visible Android FIFO directory")
	fs.BoolVar(&probe, "probe", false, "probe prepare-android capability and exit")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "stratovirt-netns-exec: prepare-android: %v\n", err)
		os.Exit(2)
	}
	if probe {
		fmt.Println("prepare-android ok")
		return
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: stratovirt-netns-exec --prepare-android <flags> <netns-name-or-path> <command> [args...]")
		os.Exit(2)
	}
	if err := prep.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "stratovirt-netns-exec: prepare-android: %v\n", err)
		os.Exit(2)
	}
	if err := prep.run(); err != nil {
		fmt.Fprintf(os.Stderr, "stratovirt-netns-exec: prepare-android: %v\n", err)
		os.Exit(1)
	}
	joinNetns(rest[0])
	execCommand(rest[1], rest[1:])
}

func (p *prepareAndroidArgs) validate() error {
	for name, value := range map[string]string{
		"sandbox-dir": p.sandboxDir, "rootfs-src": p.rootfsSrc,
		"persistent-src": p.persistentSrc, "sdcard-src": p.sdcardSrc,
		"pipe-dir": p.pipeDir,
	} {
		if value == "" {
			return fmt.Errorf("missing required flag -%s", name)
		}
	}
	return nil
}

func (p *prepareAndroidArgs) run() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("mount --make-rprivate /: %w", err)
	}
	if err := os.MkdirAll(p.sandboxDir, 0o755); err != nil {
		return fmt.Errorf("mkdir sandbox: %w", err)
	}
	if err := unix.Mount("tmpfs", p.sandboxDir, "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount sandbox tmpfs: %w", err)
	}
	for i, src := range []string{p.rootfsSrc, p.persistentSrc, p.sdcardSrc} {
		if err := os.Symlink(src, filepath.Join(p.sandboxDir, fmt.Sprintf("android-disk-%d.raw", i))); err != nil {
			return fmt.Errorf("symlink Android disk %d: %w", i, err)
		}
	}
	if err := os.MkdirAll(p.pipeDir, 0o755); err != nil {
		return fmt.Errorf("mkdir Android pipes: %w", err)
	}
	for _, name := range []string{"keymaster_fifo_vm", "gatekeeper_fifo_vm", "oemlock_fifo_vm", "keymint_fifo_vm"} {
		for _, suffix := range []string{".in", ".out"} {
			path := filepath.Join(p.pipeDir, name+suffix)
			if err := unix.Mkfifo(path, 0o600); err != nil {
				// Host services may have already created the shared FIFO.
				info, statErr := os.Lstat(path)
				if errors.Is(err, unix.EEXIST) && statErr == nil && info.Mode()&os.ModeNamedPipe != 0 {
					continue
				}
				return fmt.Errorf("mkfifo %s: %w", path, err)
			}
		}
	}
	return nil
}
