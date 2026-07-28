package stratovirt

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var snapshotFiles = []string{"state", "memory"}

func packSnapshot(dir, destination string) error {
	out, err := os.Create(destination)
	if err != nil {
		return fmt.Errorf("create snapshot archive: %w", err)
	}
	defer func() {
		if out != nil {
			_ = out.Close()
		}
	}()

	tw := tar.NewWriter(out)

	for _, name := range snapshotFiles {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat snapshot file %s: %w", name, err)
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("create snapshot header %s: %w", name, err)
		}
		header.Name = name

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write snapshot header %s: %w", name, err)
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open snapshot file %s: %w", name, err)
		}
		_, copyErr := io.Copy(tw, file)
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("archive snapshot file %s: %w", name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close snapshot file %s: %w", name, closeErr)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close snapshot archive: %w", err)
	}
	closeErr := out.Close()
	out = nil
	if closeErr != nil {
		return fmt.Errorf("close snapshot file: %w", closeErr)
	}
	return nil
}

func unpackSnapshot(source, dir string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open snapshot archive: %w", err)
	}
	defer in.Close()

	tr := tar.NewReader(in)
	found := make(map[string]bool, len(snapshotFiles))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read snapshot archive: %w", err)
		}
		if !isSnapshotFile(header.Name) || header.Typeflag != tar.TypeReg {
			return fmt.Errorf("unexpected snapshot archive entry %q", header.Name)
		}

		path := filepath.Join(dir, header.Name)
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("create restored snapshot file %s: %w", header.Name, err)
		}
		_, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return fmt.Errorf("extract snapshot file %s: %w", header.Name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close restored snapshot file %s: %w", header.Name, closeErr)
		}
		found[header.Name] = true
	}

	for _, name := range snapshotFiles {
		if !found[name] {
			return fmt.Errorf("snapshot archive is missing %s", name)
		}
	}
	return nil
}

func isSnapshotFile(name string) bool {
	for _, allowed := range snapshotFiles {
		if name == allowed {
			return true
		}
	}
	return false
}
