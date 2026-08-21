package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/testfixture"
)

func main() {
	output := flag.String("out", "testdata/demo", "fixture output directory")
	force := flag.Bool("force", false, "replace an existing fixture directory")
	flag.Parse()
	abs, err := filepath.Abs(*output)
	if err != nil {
		fail(err)
	}
	if _, err := os.Stat(abs); err == nil {
		if !*force {
			fail(fmt.Errorf("output %q exists; pass --force to replace it", abs))
		}
		if err := os.RemoveAll(abs); err != nil {
			fail(err)
		}
	} else if !os.IsNotExist(err) {
		fail(err)
	}
	if err := testfixture.Create(abs); err != nil {
		fail(err)
	}
	fmt.Println(abs)
}

func fail(err error) { fmt.Fprintln(os.Stderr, "error:", err); os.Exit(1) }
