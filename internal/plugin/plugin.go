package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Entry is one plugin in the local catalog.
type Entry struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

type catalogFile struct {
	Plugins []Entry `yaml:"plugins"`
}

// LoadCatalog reads the catalog YAML. A missing file returns os.ErrNotExist.
func LoadCatalog(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f catalogFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Plugins, nil
}

// SaveCatalog writes the catalog YAML.
func SaveCatalog(path string, entries []Entry) error {
	if entries == nil {
		entries = []Entry{}
	}
	data, err := yaml.Marshal(catalogFile{Plugins: entries})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Reconcile enables every listed plugin and removes dw-plugin-* rows whose
// name is no longer listed. An empty list still removes those rows.
func Reconcile(home, catalog string, enabled []string) error {
	want := map[string]bool{}
	for _, name := range enabled {
		want[name] = true
		if err := Enable(home, catalog, name); err != nil {
			return err
		}
	}
	rows, err := readPatch(filepath.Join(home, "user-cordis.patch.yml"))
	if err != nil {
		return err
	}
	for _, row := range rows {
		id := rowID(row)
		if !strings.HasPrefix(id, "dw-plugin-") {
			continue
		}
		name := strings.TrimPrefix(id, "dw-plugin-")
		if want[name] {
			continue
		}
		if err := Disable(home, name); err != nil {
			return err
		}
	}
	return nil
}

// Enable appends a dw-plugin-<name> row to <home>/user-cordis.patch.yml.
// It does not modify profiles/*/cordis.patch.yml. An id that is already
// present is left as it is.
func Enable(home, catalogPath, name string) error {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		return fmt.Errorf("plugin: invalid name %q", name)
	}
	entries, err := LoadCatalog(catalogPath)
	if err != nil {
		return err
	}
	var plugPath string
	for _, e := range entries {
		if e.Name == name {
			plugPath = e.Path
			break
		}
	}
	if plugPath == "" {
		return fmt.Errorf("plugin: %q is not in the catalog", name)
	}
	if _, err := os.Stat(plugPath); err != nil {
		return fmt.Errorf("plugin: %q: %w", name, err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	path := filepath.Join(home, "user-cordis.patch.yml")
	rows, err := readPatch(path)
	if err != nil {
		return err
	}
	id := pluginID(name)
	for i, row := range rows {
		if rowID(row) != id {
			continue
		}
		if fmt.Sprint(row["plugin"]) == plugPath {
			return nil
		}
		rows[i]["plugin"] = plugPath
		return writePatch(path, rows)
	}
	rows = append(rows, map[string]any{"id": id, "plugin": plugPath})
	return writePatch(path, rows)
}

// Disable removes the dw-plugin-<name> row. A missing plugin is an error.
// Rows whose id does not start with dw-plugin- are never removed.
func Disable(home, name string) error {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		return fmt.Errorf("plugin: refusing to disable %q", name)
	}
	id := pluginID(name)
	if !strings.HasPrefix(id, "dw-plugin-") {
		return fmt.Errorf("plugin: refusing to disable %q", id)
	}
	path := filepath.Join(home, "user-cordis.patch.yml")
	rows, err := readPatch(path)
	if err != nil {
		return err
	}
	found := false
	kept := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		rid := rowID(row)
		if rid != id {
			kept = append(kept, row)
			continue
		}
		if !strings.HasPrefix(rid, "dw-plugin-") {
			return fmt.Errorf("plugin: refusing to disable %q", rid)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("plugin: %s is not enabled", name)
	}
	return writePatch(path, kept)
}

func pluginID(name string) string {
	return "dw-plugin-" + name
}

func readPatch(path string) ([]map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return []map[string]any{}, nil
	}
	var rows []map[string]any
	if err := yaml.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	return rows, nil
}

func writePatch(path string, rows []map[string]any) error {
	if rows == nil {
		rows = []map[string]any{}
	}
	data, err := yaml.Marshal(rows)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func rowID(row map[string]any) string {
	switch v := row["id"].(type) {
	case string:
		return v
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
}
