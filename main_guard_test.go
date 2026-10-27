package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain puts a claude that refuses every call first on the PATH, so a
// test makes no real Claude call by accident: a test that means to call
// a fake shims its own claude ahead of this one (#59).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentd-guard")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	guard := "#!/bin/sh\necho 'a test called claude without a fake' >&2\nexit 97\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(guard), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
