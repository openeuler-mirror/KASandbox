package cli_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestShellScriptsUseLF(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".sh" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte("\r\n")) {
			t.Errorf("%s uses CRLF line endings", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
