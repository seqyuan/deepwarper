package cliops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/deepwarper/internal/auth"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/mount"
	"github.com/seqyuan/deepwarper/internal/plugin"
	"github.com/seqyuan/deepwarper/internal/state"
)

func usersDir(configDir string) string {
	return filepath.Join(configDir, "users")
}

// UserCreate creates <configDir>/users/<name>.yaml.
func UserCreate(configDir, name, password string) error {
	return auth.Create(usersDir(configDir), name, password)
}

// UserSetPassword updates the stored hash.
func UserSetPassword(configDir, name, password string) error {
	return auth.SetPassword(usersDir(configDir), name, password)
}

// UserDelete tombstones the account: credentials, mounts and plugins are
// cleared and the session generation advances, so recreating the same name
// cannot revive sessions issued before the delete.
func UserDelete(configDir, name string) error {
	return auth.Delete(usersDir(configDir), name)
}

// MountAddLocal records a path mount. spec is host:target:ro|rw.
func MountAddLocal(configDir, user, spec string) error {
	host, target, mode, err := parseLocalSpec(spec)
	if err != nil {
		return err
	}
	name := filepath.Base(target)
	m := auth.Mount{
		Name:   name,
		Kind:   "path",
		Host:   host,
		Target: target,
		Mode:   mode,
	}
	if err := validateMount(configDir, m); err != nil {
		return err
	}
	return addMount(configDir, user, m)
}

// MountAddRemote records a bucket mount.
func MountAddRemote(configDir, user, remote, prefix, target, mode string) error {
	if remote == "" || prefix == "" || target == "" || (mode != "ro" && mode != "rw") {
		return fmt.Errorf("remote mount requires remote, prefix, target and mode ro or rw")
	}
	if !mount.ValidName(remote) {
		return fmt.Errorf("mount: invalid remote name %q", remote)
	}
	m := auth.Mount{
		Name:   filepath.Base(target),
		Kind:   "bucket",
		Remote: remote,
		Prefix: prefix,
		Target: target,
		Mode:   mode,
	}
	if err := validateMount(configDir, m); err != nil {
		return err
	}
	return addMount(configDir, user, m)
}

func validateMount(configDir string, m auth.Mount) error {
	roots, err := mount.LoadRoots(filepath.Join(configDir, "roots.yaml"))
	if err != nil {
		return err
	}
	return mount.ValidateMount(m, roots)
}

func parseLocalSpec(spec string) (host, target, mode string, err error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 3 {
		return "", "", "", fmt.Errorf("local mount must be host:target:ro|rw")
	}
	mode = parts[len(parts)-1]
	target = parts[len(parts)-2]
	host = strings.Join(parts[:len(parts)-2], ":")
	if host == "" || target == "" || (mode != "ro" && mode != "rw") {
		return "", "", "", fmt.Errorf("local mount must be host:target:ro|rw")
	}
	return host, target, mode, nil
}

func addMount(configDir, user string, m auth.Mount) error {
	u, err := auth.Load(usersDir(configDir), user)
	if err != nil {
		return err
	}
	for _, existing := range u.Mounts {
		if existing.Name == m.Name {
			return fmt.Errorf("mount %q already exists", m.Name)
		}
	}
	u.Mounts = append(u.Mounts, m)
	return auth.Save(usersDir(configDir), u)
}

// RemoteAdd writes or replaces one remote in <configDir>/remotes.yaml.
func RemoteAdd(configDir string, remote mount.Remote) error {
	switch remote.Provider {
	case "tos", "s3":
	default:
		return fmt.Errorf("provider must be tos or s3")
	}
	if !mount.ValidName(remote.Name) {
		return fmt.Errorf("mount: invalid remote name %q", remote.Name)
	}
	if remote.Name == "" || remote.Endpoint == "" || remote.Region == "" || remote.Bucket == "" || remote.CredentialFile == "" {
		return fmt.Errorf("remote add requires name, endpoint, region, bucket and credential file")
	}
	path := filepath.Join(configDir, "remotes.yaml")
	remotes, err := mount.LoadRemotes(path)
	if err != nil {
		return err
	}
	replaced := false
	for i := range remotes {
		if remotes[i].Name == remote.Name {
			remotes[i] = remote
			replaced = true
			break
		}
	}
	if !replaced {
		remotes = append(remotes, remote)
	}
	return mount.SaveRemotes(path, remotes)
}

// CatalogAdd writes or replaces one plugin in <configDir>/catalog.yaml.
func CatalogAdd(configDir, name, dir string) error {
	if name == "" || dir == "" {
		return fmt.Errorf("catalog add requires a name and a directory")
	}
	path := filepath.Join(configDir, "catalog.yaml")
	entries, err := plugin.LoadCatalog(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	replaced := false
	for i := range entries {
		if entries[i].Name == name {
			entries[i].Path = dir
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, plugin.Entry{Name: name, Path: dir})
	}
	return plugin.SaveCatalog(path, entries)
}

// DataDirFromServeConfig reads server.dataDir from the file `dw serve -c` uses.
// A relative dataDir is resolved next to that file. An empty path is an error:
// plugin edits must land in the instance home, not a second tree.
func DataDirFromServeConfig(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("dw plugin requires -c config.yaml to locate server.dataDir")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc struct {
		Server struct {
			DataDir string `yaml:"dataDir"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if strings.TrimSpace(doc.Server.DataDir) == "" {
		return "", fmt.Errorf("%s: server.dataDir is empty", path)
	}
	dir := doc.Server.DataDir
	if !filepath.IsAbs(dir) {
		dir = filepath.Clean(filepath.Join(filepath.Dir(path), dir))
	}
	return dir, nil
}

// instanceHome is the home the instance manager uses for user. An existing
// record wins (it may carry a reused or stable-slug home); otherwise the
// manager's default LocalSlug home is used so plugin edits land in the same
// tree a later launch reads.
func instanceHome(dataDir, user string) string {
	if strings.TrimSpace(dataDir) != "" {
		if store, err := state.New(dataDir); err == nil {
			if list, err := store.List(); err == nil {
				var running *model.Instance
				for _, inst := range list {
					if inst.User != user || inst.Home == "" {
						continue
					}
					if inst.State == model.StateRunning {
						running = inst
						break
					}
					if running == nil {
						running = inst
					}
				}
				if running != nil {
					return running.Home
				}
			}
		}
	}
	return filepath.Join(dataDir, "homes", model.LocalSlug(user))
}

// PluginEnable turns a catalog plugin on for the user and writes the cordis
// row under <dataDir>/homes/<slug>, the same home the instance manager uses.
func PluginEnable(configDir, dataDir, user, name string) error {
	if strings.TrimSpace(dataDir) == "" {
		return fmt.Errorf("plugin enable requires the serve data directory")
	}
	home := instanceHome(dataDir, user)
	if err := plugin.Enable(home, filepath.Join(configDir, "catalog.yaml"), name); err != nil {
		return err
	}
	u, err := auth.Load(usersDir(configDir), user)
	if err != nil {
		return err
	}
	for _, p := range u.Plugins {
		if p == name {
			return nil
		}
	}
	u.Plugins = append(u.Plugins, name)
	return auth.Save(usersDir(configDir), u)
}

// PluginDisable removes the plugin from the user record and from the
// instance home under dataDir.
func PluginDisable(configDir, dataDir, user, name string) error {
	if strings.TrimSpace(dataDir) == "" {
		return fmt.Errorf("plugin disable requires the serve data directory")
	}
	home := instanceHome(dataDir, user)
	if err := plugin.Disable(home, name); err != nil {
		return err
	}
	u, err := auth.Load(usersDir(configDir), user)
	if err != nil {
		return err
	}
	next := make([]string, 0, len(u.Plugins))
	for _, p := range u.Plugins {
		if p == name {
			continue
		}
		next = append(next, p)
	}
	u.Plugins = next
	return auth.Save(usersDir(configDir), u)
}
