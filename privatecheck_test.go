package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tailnet examples are put together at run time so this file holds no
// address the built-in rule matches.
var (
	tailnetIP  = "100." + "100.1.2"
	tailnetLow = "100." + "64.0.9"
	tailnetTop = "100." + "127.255.1"
	belowRange = "100." + "63.1.2"
	aboveRange = "100." + "128.1.2"
)

// privateCheck commits files to a new repository holding bin/private-check,
// writes untracked files beside them, and runs the script with patterns as
// the patterns file ("" for none). It returns the output and exit code.
func privateCheck(t *testing.T, patterns string, tracked, untracked map[string]string) (string, int) {
	t.Helper()
	for _, tool := range []string{"git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is not installed")
		}
	}
	script, err := os.ReadFile(filepath.Join("bin", "private-check"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "private-check"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if _, err := gitOut(10*time.Second, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	run("config", "user.email", "agent@example.org")
	run("config", "user.name", "agent")
	for name, text := range tracked {
		writeFile(t, filepath.Join(dir, name), text)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "files")
	for name, text := range untracked {
		writeFile(t, filepath.Join(dir, name), text)
	}
	file := filepath.Join(root, "absent")
	if patterns != "" {
		file = filepath.Join(root, "patterns")
		writeFile(t, file, patterns)
	}
	cmd := exec.Command("bash", filepath.Join(dir, "bin", "private-check"))
	cmd.Env = append(os.Environ(), "PRIVATE_PATTERNS="+file, "HOME="+root, "XDG_CONFIG_HOME="+root)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	t.Fatal(err)
	return "", 0
}

const testPatterns = "# invented values\n\nhost buildbox-zz.example\nemail someone@example.invalid\nuser zzuser\nip 10.9.8.7\n"

// Each kind in the patterns file, and the built-in tailnet range, fails
// the check and names the file, the line and the kind, never the value.
func TestPrivateCheckFails(t *testing.T) {
	cases := []struct {
		name, text, kind, value string
	}{
		{"host", "ssh BuildBox-ZZ.example to deploy\n", "host", "buildbox-zz"},
		{"email", "mail someone@example.invalid\n", "email", "someone@example"},
		{"user", "cd /home/zzuser/repos\n", "home path", "zzuser"},
		{"user at end", "HOME=/home/zzuser\n", "home path", "zzuser"},
		{"ip", "dial 10.9.8.7:22\n", "ip", "10.9.8.7"},
		{"tailnet", "hub at " + tailnetIP + ":8737\n", "tailnet address", tailnetIP},
		{"tailnet low", "x=" + tailnetLow + "\n", "tailnet address", tailnetLow},
		{"tailnet top", tailnetTop + "\n", "tailnet address", tailnetTop},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, code := privateCheck(t, testPatterns, map[string]string{
				"clean.go":  "package main\n",
				"docs/a.md": "line one\n" + c.text,
			}, nil)
			if code != 1 || !strings.Contains(out, "private-check: docs/a.md:2: "+c.kind+"\n") {
				t.Fatalf("exit %d, output:\n%s", code, out)
			}
			if strings.Contains(strings.ToLower(out), strings.ToLower(c.value)) {
				t.Fatalf("the output holds the matched value:\n%s", out)
			}
		})
	}
	// The built-in range fails with no patterns file too.
	out, code := privateCheck(t, "", map[string]string{"a.md": tailnetIP + "\n"}, nil)
	if code != 1 || !strings.Contains(out, "a.md:1: tailnet address") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
}

// A clean tree passes; near misses, untracked files and a missing
// patterns file do not fail it.
func TestPrivateCheckPasses(t *testing.T) {
	tracked := map[string]string{
		"a.md": strings.Join([]string{
			belowRange, aboveRange, "1" + tailnetIP, "v" + "1.100.64", tailnetLow + "/10",
			"/home/someone/private", "/home/zzuserx/a", "notbuildbox-zz.examples",
			"someone@example.invalidx", "110.9.8.7",
		}, "\n") + "\n",
	}
	untracked := map[string]string{
		"notes.txt":         "buildbox-zz.example " + tailnetIP + " /home/zzuser\n",
		".private-patterns": "host other\n",
	}
	if out, code := privateCheck(t, testPatterns, tracked, untracked); code != 0 {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
	out, code := privateCheck(t, "", tracked, untracked)
	if code != 0 || !strings.Contains(out, "no patterns file; checking built-in rules only") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
}

// A malformed patterns file is an error naming the line, not the value,
// and a tracked .private-patterns fails the check.
func TestPrivateCheckBadPatterns(t *testing.T) {
	out, code := privateCheck(t, "host a\nhostname secret-zz\n", map[string]string{"a.md": "x\n"}, nil)
	if code != 1 || !strings.Contains(out, "patterns file line 2: unknown kind") || strings.Contains(out, "secret-zz") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
	out, code = privateCheck(t, "user\n", map[string]string{"a.md": "x\n"}, nil)
	if code != 1 || !strings.Contains(out, "patterns file line 1: no value") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
	out, code = privateCheck(t, "", map[string]string{".private-patterns": "host x\n"}, nil)
	if code != 1 || !strings.Contains(out, ".private-patterns is tracked") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
}
