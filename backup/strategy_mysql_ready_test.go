package backup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runReadyScript runs mysqlReadyScript against a stub client that behaves as body describes,
// with sleep stubbed out so the retry loop finishes at once.
func runReadyScript(t *testing.T, body string) (int, string, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	data := filepath.Join(dir, "data")
	for _, d := range []string{bin, filepath.Join(data, "backups")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+content+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("mariadb", body)
	write("sleep", "exit 0")

	cmd := exec.Command("bash", "-c", mysqlReadyScript("mariadb", "10.0.0.1", data))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), `MYSQL_PWD=good)pw"$x =y`)
	out, err := cmd.CombinedOutput()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, string(out), data
}

func TestMysqlReadyScriptSuccess(t *testing.T) {
	code, out, data := runReadyScript(t, `[ "$MYSQL_PWD" = 'good)pw"$x =y' ] || exit 1; echo status`)
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if strings.Contains(out, "status") {
		t.Errorf("success output should not be echoed: %q", out)
	}
	if _, err := os.Stat(filepath.Join(data, "backups")); !os.IsNotExist(err) {
		t.Errorf("stale backups dir was not removed")
	}
}

func TestMysqlReadyScriptAccessDenied(t *testing.T) {
	code, out, _ := runReadyScript(t, `echo "ERROR 1045 (28000): Access denied for user 'root'@'10.0.0.2' (using password: YES)" >&2; exit 1`)
	if code != mysqlAccessDeniedExit {
		t.Fatalf("exit %d, want %d; output %q", code, mysqlAccessDeniedExit, out)
	}
	if strings.Contains(out, "Connection attempt") {
		t.Errorf("access denied should not retry: %q", out)
	}
	if !strings.Contains(out, "ERROR 1045") {
		t.Errorf("client error not echoed: %q", out)
	}
}

func TestMysqlReadyScriptRetriesThenFails(t *testing.T) {
	code, out, _ := runReadyScript(t, `echo "ERROR 2003 (HY000): Can't connect" >&2; exit 1`)
	if code != 1 {
		t.Fatalf("exit %d, want 1; output %q", code, out)
	}
	for _, want := range []string{"Connection attempt 10", "ERROR 2003", "Failed to connect"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %q", want, out)
		}
	}
}
