package runtime

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type decodedPatch struct {
	ID     string `yaml:"id"`
	Config struct {
		DocumentsDirectory string `yaml:"documentsDirectory"`
		DefaultDirectory   string `yaml:"defaultDirectory"`
	} `yaml:"config"`
}

func TestRuntimePatchRoundTripsLiteralPaths(t *testing.T) {
	for _, dir := range []string{
		"/workspace",
		"/home/alice/My Workspace",
		"/home/alice/a:b",
		"/home/alice/#hash",
	} {
		text, err := RuntimePatch(dir)
		if err != nil {
			t.Fatalf("%q: %v", dir, err)
		}
		var rows []decodedPatch
		if err := yaml.Unmarshal([]byte(text), &rows); err != nil {
			t.Fatalf("%q: invalid yaml %q: %v", dir, text, err)
		}
		if len(rows) != 2 {
			t.Fatalf("%q: rows: %#v", dir, rows)
		}
		if rows[0].ID != "workspace-controller" || rows[0].Config.DocumentsDirectory != dir {
			t.Fatalf("%q: first row: %#v", dir, rows[0])
		}
		if rows[1].ID != "working-directory" || rows[1].Config.DefaultDirectory != dir {
			t.Fatalf("%q: second row: %#v", dir, rows[1])
		}
		if dir == "/workspace" && !strings.Contains(text, "documentsDirectory: /workspace") {
			t.Fatalf("plain path must stay unquoted: %s", text)
		}
	}
}

func TestRuntimePatchRejectsInjection(t *testing.T) {
	for _, dir := range []string{
		"",
		"/workspace\n- id: injected\n  config:\n    enabled: true",
		"/workspace\r",
		"/workspace\x00",
	} {
		if _, err := RuntimePatch(dir); err == nil {
			t.Fatalf("%q must be rejected", dir)
		}
	}
}
