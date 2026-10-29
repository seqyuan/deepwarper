package conda

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCondarcKeys(t *testing.T) {
	text := Condarc()
	for _, want := range []string{
		"conda-forge",
		"channel_priority: strict",
		"envs_dirs:",
		"/workspace/envs",
		"pkgs_dirs:",
		"/workspace/.conda/pkgs",
		"auto_activate_base: false",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("condarc missing %q:\n%s", want, text)
		}
	}
}

func TestPathOrdersNodeBeforeMoniforge(t *testing.T) {
	sep := string(os.PathListSeparator)
	got := Path("/opt/dw/bin", "/opt/moniforge/bin")
	want := "/opt/dw/bin" + sep + "/opt/moniforge/bin"
	if got != want {
		t.Fatalf("path: %q want %q", got, want)
	}
	if Path("", "/opt/moniforge/bin") != "/opt/moniforge/bin" {
		t.Fatalf("empty node: %q", Path("", "/opt/moniforge/bin"))
	}
	if Path("/opt/dw/bin", "") != "/opt/dw/bin" {
		t.Fatalf("empty moniforge: %q", Path("/opt/dw/bin", ""))
	}
	if Path("", "") != "" {
		t.Fatalf("both empty: %q", Path("", ""))
	}
}

func TestEnvScriptRefusesUnsafeNames(t *testing.T) {
	script := EnvScript()
	if !strings.Contains(script, "base") {
		t.Fatal("script does not mention the base refusal")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash missing")
	}
	path := filepath.Join(t.TempDir(), "dw-env")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"base", "../x"} {
		cmd := exec.Command("bash", path, "create", name, "foo")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("dw-env create %s succeeded: %s", name, out)
		}
	}
}

func TestInstall(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	bin := filepath.Join(t.TempDir(), "bin")
	if err := Install(home, bin); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".condarc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "conda-forge") || !strings.Contains(string(data), "/workspace/envs") {
		t.Fatalf("condarc: %s", data)
	}
	fi, err := os.Stat(filepath.Join(bin, "dw-env"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("dw-env mode: %o", fi.Mode().Perm())
	}
}
