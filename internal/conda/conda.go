package conda

import (
	"os"
	"path/filepath"
	"strings"
)

// Condarc is the read-only conda configuration for a user environment that
// lives under /workspace.
func Condarc() string {
	return `channels:
  - conda-forge
channel_priority: strict
envs_dirs:
  - /workspace/envs
pkgs_dirs:
  - /workspace/.conda/pkgs
auto_activate_base: false
`
}

// Path joins the node bin directory and the moniforge bin directory. Empty
// parts are omitted. The node directory comes first.
func Path(nodeBin, moniforgeBin string) string {
	var parts []string
	if nodeBin != "" {
		parts = append(parts, nodeBin)
	}
	if moniforgeBin != "" {
		parts = append(parts, moniforgeBin)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// EnvScript is the dw-env bash script. It refuses the name base and any name
// that contains / or ...
func EnvScript() string {
	return `#!/bin/bash
set -u
usage() {
  echo "usage: dw-env create|install|run <name> [args...]" >&2
  exit 1
}
refuse() {
  echo "dw-env: refusing name $1" >&2
  exit 1
}
if [ "$#" -lt 2 ]; then
  usage
fi
cmd="$1"
name="$2"
shift 2
case "$name" in
  base)
    refuse "$name"
    ;;
esac
if [[ "$name" == *"/"* || "$name" == *".."* ]]; then
  refuse "$name"
fi
prefix="/workspace/envs/${name}"
case "$cmd" in
  create)
    exec mamba create -y -p "$prefix" "$@"
    ;;
  install)
    exec mamba install -y -p "$prefix" "$@"
    ;;
  run)
    if [ "${1:-}" = "--" ]; then
      shift
    fi
    export PATH="${prefix}/bin:${PATH:-}"
    exec "$@"
    ;;
  *)
    usage
    ;;
esac
`
}

// Install writes .condarc into the user home and dw-env into binDir.
func Install(home, binDir string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(home, ".condarc"), []byte(Condarc()), 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(binDir, "dw-env")
	if err := os.WriteFile(path, []byte(EnvScript()), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}
