package instance

import (
	"errors"
	"os"
	"os/exec"
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

// attachSandbox fills bwrap and rclone fields on a local spec. A distribution
// path alone is enough to sandbox: the shared profile must stay read-only so
// user plugin installs cannot write through the node_modules symlink.
func (m *Manager) attachSandbox(spec *agent.Spec, user string) error {
	if spec == nil {
		return nil
	}
	// Resolve dsh physically first: with an npm-style shim the symlink target's
	// package root is what must exist inside the sandbox. The real path is also
	// the command, so PATH lookup cannot redirect it again.
	realDsh, dshBinds := resolveDshBin(spec.DshBin)
	if realDsh != "" {
		spec.DshBin = realDsh
	}
	userPatch := filepath.Join(spec.Home, "user-cordis.patch.yml")
	if _, err := os.Stat(userPatch); err == nil {
		spec.ExtraPatches = append(spec.ExtraPatches, userPatch)
	}
	var mounts []auth.Mount
	cfgDir := ""
	if strings.TrimSpace(m.cfg.Auth.UsersDir) != "" {
		cfgDir = filepath.Dir(m.cfg.Auth.UsersDir)
		loaded, err := auth.Load(m.cfg.Auth.UsersDir, user)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if loaded != nil {
			mounts = loaded.Mounts
		}
	}
	if len(mounts) == 0 && m.cfg.Runtime.Moniforge == "" && spec.DistPath == "" {
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
		HostBinds: append(hostBasics(), dshBinds...),
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
	patch, err := runtime.RuntimePatch("/workspace")
	if err != nil {
		return err
	}
	spec.PatchContent = patch
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

func hostBasics() []string {
	var out []string
	for _, p := range []string{
		"/usr", "/bin", "/lib", "/lib64", "/etc/ssl", "/etc/pki", "/etc/resolv.conf", "/etc/hosts",
		// dsh and tool subprocesses consult the account database; without
		// these node's os.userInfo() and getpwuid() fail inside the sandbox.
		"/etc/passwd", "/etc/group", "/etc/nsswitch.conf",
	} {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// resolveDshBin returns the physical dsh path and the host directories the
// sandbox must bind so node can find the launcher and its package root. It
// never returns "/": binding the host root read-only would defeat the sandbox.
func resolveDshBin(bin string) (string, []string) {
	if strings.TrimSpace(bin) == "" {
		return bin, nil
	}
	p := bin
	if !filepath.IsAbs(p) {
		if found, err := exec.LookPath(p); err == nil {
			p = found
		}
	}
	real := p
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		real = resolved
	}
	var binds []string
	addBind := func(dir string) {
		if dir == "" || dir == "." || !filepath.IsAbs(dir) {
			return
		}
		dir = filepath.Clean(dir)
		if dir == string(filepath.Separator) {
			return
		}
		for _, b := range binds {
			if b == dir {
				return
			}
		}
		binds = append(binds, dir)
	}
	realDir := filepath.Dir(real)
	addBind(realDir)
	addBind(nearestPackageRoot(realDir))
	// npm global installs resolve into <prefix>/lib/node_modules/<pkg>/... and
	// pnpm keeps its store behind a node_modules directory: node needs the
	// install root (the absolute realpath of the symlink) inside the sandbox,
	// not only the package directory.
	addBind(nearestInstallRoot(realDir))
	// Keep the lexical shim directory too: a shebang script invoked by its
	// configured path must still exist inside the sandbox.
	if lexicalDir := filepath.Dir(bin); filepath.IsAbs(lexicalDir) {
		addBind(lexicalDir)
	}
	return real, binds
}

// nearestPackageRoot walks up from dir to the closest ancestor holding a
// package.json, bounded so a pathological filesystem cannot loop.
func nearestPackageRoot(dir string) string {
	for i := 0; i < 32; i++ {
		if dir == "" || dir == "." {
			return ""
		}
		dir = filepath.Clean(dir)
		if dir == string(filepath.Separator) {
			return ""
		}
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// nearestInstallRoot walks up from dir to the closest ancestor holding a
// node_modules or .pnpm directory, which is what npm-global and pnpm layouts
// expose as their install root. A plain lib directory is deliberately not a
// marker: a user's ~/lib (or any other generic directory) must not be bound
// read-only into the sandbox just because dsh lives underneath it. The walk is
// bounded and never returns "/".
func nearestInstallRoot(dir string) string {
	for i := 0; i < 32; i++ {
		if dir == "" || dir == "." {
			return ""
		}
		dir = filepath.Clean(dir)
		if dir == string(filepath.Separator) {
			return ""
		}
		for _, marker := range []string{"node_modules", ".pnpm"} {
			if fi, err := os.Stat(filepath.Join(dir, marker)); err == nil && fi.IsDir() {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
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
