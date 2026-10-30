package runtime

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// patchRow is one row of the generated dsh runtime patch. Only the keys dsh
// expects are ever emitted, so a path can never become a new YAML row.
type patchRow struct {
	ID     string      `yaml:"id"`
	Config patchConfig `yaml:"config"`
}

type patchConfig struct {
	DocumentsDirectory string `yaml:"documentsDirectory,omitempty"`
	DefaultDirectory   string `yaml:"defaultDirectory,omitempty"`
}

// RuntimePatch renders the dsh runtime patch that points the workspace
// controller at the workspace path dsh will see. The path is marshalled by the
// YAML encoder, so quotes, colons and newlines cannot inject rows.
func RuntimePatch(workspaceDir string) (string, error) {
	if workspaceDir == "" {
		return "", errors.New("runtime: workspace directory is empty")
	}
	if strings.ContainsAny(workspaceDir, "\x00\r\n") {
		return "", fmt.Errorf("runtime: workspace directory %q contains a NUL, CR or LF", workspaceDir)
	}
	rows := []patchRow{
		{ID: "workspace-controller", Config: patchConfig{DocumentsDirectory: workspaceDir}},
		{ID: "working-directory", Config: patchConfig{DefaultDirectory: workspaceDir}},
	}
	data, err := yaml.Marshal(rows)
	if err != nil {
		return "", fmt.Errorf("runtime: render patch: %w", err)
	}
	return string(data), nil
}
