package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCatalog(t *testing.T, path, name, plug string) {
	t.Helper()
	body := "plugins:\n  - name: " + name + "\n    path: " + plug + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEnableAndDisable(t *testing.T) {
	home := t.TempDir()
	office := filepath.Join(t.TempDir(), "office")
	if err := os.MkdirAll(office, 0o755); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(t.TempDir(), "catalog.yaml")
	writeCatalog(t, catalog, "office", office)

	profile := filepath.Join(home, "profiles", "default", "cordis.patch.yml")
	if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("- id: keep\n  disabled: true\n")
	if err := os.WriteFile(profile, original, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Enable(home, catalog, "office"); err != nil {
		t.Fatal(err)
	}
	if err := Enable(home, catalog, "office"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "user-cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "dw-plugin-office") != 1 {
		t.Fatalf("duplicate or missing id: %s", data)
	}
	if !strings.Contains(string(data), office) {
		t.Fatalf("plugin path missing: %s", data)
	}
	got, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("profile patch changed: %s", got)
	}
	if err := Enable(home, catalog, "missing"); err == nil {
		t.Fatal("unknown plugin was enabled")
	}

	ghost := filepath.Join(t.TempDir(), "ghost.yaml")
	writeCatalog(t, ghost, "ghost", filepath.Join(t.TempDir(), "no-such-plugin"))
	if err := Enable(home, ghost, "ghost"); err == nil {
		t.Fatal("missing plugin path was enabled")
	}

	foreign := "- id: session-log-deepseek\n  config:\n    enabled: false\n- id: dw-plugin-office\n  plugin: " + office + "\n"
	if err := os.WriteFile(filepath.Join(home, "user-cordis.patch.yml"), []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Disable(home, "office"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(home, "user-cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "dw-plugin-office") {
		t.Fatalf("plugin id remained: %s", data)
	}
	if !strings.Contains(string(data), "session-log-deepseek") || !strings.Contains(string(data), "enabled: false") {
		t.Fatalf("foreign row was removed: %s", data)
	}
	if err := Disable(home, "office"); err == nil {
		t.Fatal("disable of missing plugin succeeded")
	}
	if err := Disable(home, "session-log-deepseek"); err == nil {
		t.Fatal("disable of a non dw-plugin id succeeded")
	}
	data, err = os.ReadFile(filepath.Join(home, "user-cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "session-log-deepseek") {
		t.Fatalf("foreign id removed by refused disable: %s", data)
	}
}

func TestReconcileRemovesUnlistedPlugins(t *testing.T) {
	home := t.TempDir()
	office := filepath.Join(t.TempDir(), "office")
	if err := os.MkdirAll(office, 0o755); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(t.TempDir(), "catalog.yaml")
	writeCatalog(t, catalog, "office", office)
	body := "- id: keep\n  disabled: true\n- id: dw-plugin-office\n  plugin: " + office + "\n- id: dw-plugin-sheets\n  plugin: /opt/dw/catalog/sheets\n"
	if err := os.WriteFile(filepath.Join(home, "user-cordis.patch.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Reconcile(home, catalog, []string{"office"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "user-cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "dw-plugin-sheets") || !strings.Contains(string(data), "dw-plugin-office") || !strings.Contains(string(data), "keep") {
		t.Fatalf("reconcile: %s", data)
	}
	if err := Reconcile(home, catalog, nil); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(home, "user-cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "dw-plugin-") {
		t.Fatalf("empty list should remove plugin rows: %s", data)
	}
	if !strings.Contains(string(data), "keep") {
		t.Fatalf("foreign row removed: %s", data)
	}
}

func TestEnableReplacesCatalogPath(t *testing.T) {
	home := t.TempDir()
	first := filepath.Join(t.TempDir(), "office")
	second := filepath.Join(t.TempDir(), "office2")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(t.TempDir(), "catalog.yaml")
	writeCatalog(t, catalog, "office", first)
	if err := Enable(home, catalog, "office"); err != nil {
		t.Fatal(err)
	}
	writeCatalog(t, catalog, "office", second)
	if err := Enable(home, catalog, "office"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "user-cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "dw-plugin-office") != 1 || !strings.Contains(string(data), second) || strings.Contains(string(data), first) {
		t.Fatalf("path was not replaced: %s", data)
	}
}

func TestLoadCatalog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.yaml")
	writeCatalog(t, path, "office", "/opt/dw/catalog/office")
	entries, err := LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "office" || entries[0].Path != "/opt/dw/catalog/office" {
		t.Fatalf("catalog: %+v", entries)
	}
	if _, err := LoadCatalog(filepath.Join(dir, "missing.yaml")); !os.IsNotExist(err) {
		t.Fatalf("missing catalog: %v", err)
	}
}
