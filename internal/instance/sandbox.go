package instance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/auth"
	"github.com/seqyuan/deepwarper/internal/conda"
	"github.com/seqyuan/deepwarper/internal/mount"
	"github.com/seqyuan/deepwarper/internal/plugin"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

// prepareHome writes conda files when moniforge is configured and applies
// the user's enabled plugins to the dsh home. It does not touch
// profiles/*/cordis.patch.yml; plugin.Enable only writes user-cordis.patch.yml.
func (m *Manager) prepareHome(home, user string) error {
	if m.cfg.Runtime.Moniforge != "" {
		bin := filepath.Join(m.cfg.Server.DataDir, "bin")
		if err := conda.Install(home, bin); err != nil {
			return err
		}
	}
	if strings.TrimSpace(m.cfg.Auth.UsersDir) == "" {
		return nil
	}
	u, err := auth.Load(m.cfg.Auth.UsersDir, user)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	catalog := filepath.Join(filepath.Dir(m.cfg.Auth.UsersDir), "catalog.yaml")
	return plugin.Reconcile(home, catalog, u.Plugins)
}

// attachSandbox fills bwrap and rclone fields on a local spec. With no mounts
// and no moniforge the spec keeps today's command.
func (m *Manager) attachSandbox(spec *agent.Spec, user string) error {
	if spec == nil {
		return nil
	}
	var mounts []auth.Mount
	var u *auth.User
	cfgDir := ""
	if strings.TrimSpace(m.cfg.Auth.UsersDir) != "" {
		cfgDir = filepath.Dir(m.cfg.Auth.UsersDir)
		loaded, err := auth.Load(m.cfg.Auth.UsersDir, user)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		u = loaded
		if u != nil {
			mounts = u.Mounts
		}
	}
	if len(mounts) == 0 && m.cfg.Runtime.Moniforge == "" {
		return nil
	}
	var roots []mount.Root
	var remotes []mount.Remote
	if cfgDir != "" {
		var err error
		roots, err = mount.LoadRoots(filepath.Join(cfgDir, "roots.yaml"))
		if err != nil {
			return err
		}
		remotes, err = mount.LoadRemotes(filepath.Join(cfgDir, "remotes.yaml"))
		if err != nil {
			return err
		}
	}
	fuse := filepath.Join(m.cfg.Server.DataDir, "runtime", spec.InstanceID, "fuse")
	in := mount.Input{
		Moniforge: m.cfg.Runtime.Moniforge,
		DSHHome:   spec.Home,
		Workspace: spec.Workspace,
		Dist:      spec.DistPath,
		Roots:     roots,
		Remotes:   remotes,
		Mounts:    mounts,
		FuseDir:   fuse,
		PatchFile: filepath.Join(spec.ControlDir, "runtime.patch.yml"),
		HostBinds: append(hostBasics(), pluginBinds(m.cfg.Auth.UsersDir, u)...),
		Command:   []string{spec.DshBin, "--profile", spec.Profile},
	}
	if m.cfg.Runtime.Moniforge != "" {
		in.BinDir = filepath.Join(m.cfg.Server.DataDir, "bin")
	}
	plan, err := mount.Build(in)
	if err != nil {
		if errors.Is(err, mount.ErrEmptyPlan) {
			return nil
		}
		return err
	}
	spec.BwrapArgv = plan.BwrapArgv
	spec.Env = append([]string{}, plan.Env...)
	spec.PatchContent = runtime.RuntimePatch("/workspace")
	if err := ensureDefaultWorkspaceLink(spec.Workspace, "/workspace"); err != nil {
		return err
	}
	byName := map[string]mount.Remote{}
	for _, r := range remotes {
		byName[r.Name] = r
	}
	for _, rc := range plan.Rclone {
		spec.Rclone = append(spec.Rclone, agent.RcloneSpec{
			Name:       rc.Name,
			Argv:       rc.Argv,
			MountPoint: rc.MountPoint,
		})
		remoteName := remoteFromRclone(rc.Argv)
		remote, ok := byName[remoteName]
		if !ok {
			return errors.New("mount: rclone config remote " + remoteName + " was not loaded")
		}
		if err := mount.WriteRcloneConfig(mount.ConfigPath(fuse, rc.Name), remote); err != nil {
			return err
		}
	}
	return nil
}

func pluginBinds(usersDir string, u *auth.User) []string {
	if u == nil || len(u.Plugins) == 0 || strings.TrimSpace(usersDir) == "" {
		return nil
	}
	entries, err := plugin.LoadCatalog(filepath.Join(filepath.Dir(usersDir), "catalog.yaml"))
	if err != nil {
		return nil
	}
	on := map[string]bool{}
	for _, name := range u.Plugins {
		on[name] = true
	}
	var out []string
	for _, e := range entries {
		if on[e.Name] && filepath.IsAbs(e.Path) {
			out = append(out, e.Path)
		}
	}
	return out
}

func hostBasics() []string {
	var out []string
	for _, p := range []string{"/usr", "/bin", "/lib", "/lib64", "/etc/ssl", "/etc/pki", "/etc/resolv.conf", "/etc/hosts"} {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func remoteFromRclone(argv []string) string {
	for i, a := range argv {
		if a == "mount" && i+1 < len(argv) {
			src := argv[i+1]
			if n := strings.IndexByte(src, ':'); n >= 0 {
				return src[:n]
			}
		}
	}
	return ""
}
