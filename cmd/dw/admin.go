package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/seqyuan/deepwarper/internal/cliops"
	"github.com/seqyuan/deepwarper/internal/mount"
)

func defaultConfigDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "config"
	}
	return filepath.Join(filepath.Dir(exe), "config")
}

func passwordFromEnvOrStdin() (string, error) {
	if p, ok := os.LookupEnv("DW_PASSWORD"); ok {
		return p, nil
	}
	fmt.Fprint(os.Stderr, "Password: ")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// pullFlags moves flags in front of positionals so `dw user alice -d dir`
// parses with the standard library flag package.
func pullFlags(args []string) []string {
	valued := map[string]bool{
		"-c":                true,
		"-d":                true,
		"--local":           true,
		"--remote":          true,
		"--prefix":          true,
		"--at":              true,
		"--provider":        true,
		"--endpoint":        true,
		"--region":          true,
		"--bucket":          true,
		"--credential-file": true,
	}
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			name := a
			if eq := strings.IndexByte(a, '='); eq > 0 {
				name = a[:eq]
			}
			flags = append(flags, a)
			if valued[name] && !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func userCmd(args []string) {
	fs := flag.NewFlagSet("user", flag.ExitOnError)
	dir := fs.String("d", defaultConfigDir(), "config directory")
	_ = fs.Parse(pullFlags(args))
	if fs.NArg() != 1 {
		log.Fatal("usage: dw user [-d dir] <name>")
	}
	pw, err := passwordFromEnvOrStdin()
	if err != nil {
		log.Fatal(err)
	}
	if err := cliops.UserCreate(*dir, fs.Arg(0), pw); err != nil {
		log.Fatal(err)
	}
}

func passwdCmd(args []string) {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	dir := fs.String("d", defaultConfigDir(), "config directory")
	_ = fs.Parse(pullFlags(args))
	if fs.NArg() != 1 {
		log.Fatal("usage: dw passwd [-d dir] <name>")
	}
	pw, err := passwordFromEnvOrStdin()
	if err != nil {
		log.Fatal(err)
	}
	if err := cliops.UserSetPassword(*dir, fs.Arg(0), pw); err != nil {
		log.Fatal(err)
	}
}

func delCmd(args []string) {
	fs := flag.NewFlagSet("del", flag.ExitOnError)
	dir := fs.String("d", defaultConfigDir(), "config directory")
	_ = fs.Parse(pullFlags(args))
	if fs.NArg() != 1 {
		log.Fatal("usage: dw del [-d dir] <name>")
	}
	if err := cliops.UserDelete(*dir, fs.Arg(0)); err != nil {
		log.Fatal(err)
	}
}

func mountCmd(args []string) {
	if len(args) < 1 || args[0] != "add" {
		log.Fatal("usage: dw mount add [-d dir] <user> --local host:target:ro|rw | --remote name --prefix p --at target --ro|--rw")
	}
	fs := flag.NewFlagSet("mount add", flag.ExitOnError)
	dir := fs.String("d", defaultConfigDir(), "config directory")
	localSpec := fs.String("local", "", "host:target:ro|rw")
	remote := fs.String("remote", "", "remote name")
	prefix := fs.String("prefix", "", "bucket prefix")
	at := fs.String("at", "", "mount target")
	ro := fs.Bool("ro", false, "read-only")
	rw := fs.Bool("rw", false, "read-write")
	_ = fs.Parse(pullFlags(args[1:]))
	if fs.NArg() != 1 {
		log.Fatal("usage: dw mount add [-d dir] <user> ...")
	}
	user := fs.Arg(0)
	if *localSpec != "" && *remote != "" {
		log.Fatal("pass either --local or --remote")
	}
	if *localSpec != "" {
		if err := cliops.MountAddLocal(*dir, user, *localSpec); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *remote == "" {
		log.Fatal("usage: dw mount add [-d dir] <user> --local ... | --remote ...")
	}
	mode := ""
	switch {
	case *ro && *rw:
		log.Fatal("pass only one of --ro or --rw")
	case *ro:
		mode = "ro"
	case *rw:
		mode = "rw"
	default:
		log.Fatal("remote mount requires --ro or --rw")
	}
	if err := cliops.MountAddRemote(*dir, user, *remote, *prefix, *at, mode); err != nil {
		log.Fatal(err)
	}
}

func remoteCmd(args []string) {
	if len(args) < 1 || args[0] != "add" {
		log.Fatal("usage: dw remote add [-d dir] <name> --provider tos|s3 --endpoint url --region r --bucket b --credential-file path")
	}
	fs := flag.NewFlagSet("remote add", flag.ExitOnError)
	dir := fs.String("d", defaultConfigDir(), "config directory")
	provider := fs.String("provider", "", "tos or s3")
	endpoint := fs.String("endpoint", "", "endpoint URL")
	region := fs.String("region", "", "region")
	bucket := fs.String("bucket", "", "bucket")
	cred := fs.String("credential-file", "", "credential file")
	_ = fs.Parse(pullFlags(args[1:]))
	if fs.NArg() != 1 {
		log.Fatal("usage: dw remote add [-d dir] <name> ...")
	}
	if err := cliops.RemoteAdd(*dir, mount.Remote{
		Name:           fs.Arg(0),
		Provider:       *provider,
		Endpoint:       *endpoint,
		Region:         *region,
		Bucket:         *bucket,
		CredentialFile: *cred,
	}); err != nil {
		log.Fatal(err)
	}
}

func catalogCmd(args []string) {
	if len(args) < 1 || args[0] != "add" {
		log.Fatal("usage: dw catalog add [-d dir] <name> <dir>")
	}
	fs := flag.NewFlagSet("catalog add", flag.ExitOnError)
	dir := fs.String("d", defaultConfigDir(), "config directory")
	_ = fs.Parse(pullFlags(args[1:]))
	if fs.NArg() != 2 {
		log.Fatal("usage: dw catalog add [-d dir] <name> <dir>")
	}
	if err := cliops.CatalogAdd(*dir, fs.Arg(0), fs.Arg(1)); err != nil {
		log.Fatal(err)
	}
}

func pluginCmd(args []string) {
	if len(args) < 1 || (args[0] != "enable" && args[0] != "disable") {
		log.Fatal("usage: dw plugin enable|disable [-d dir] <user> <name>")
	}
	verb := args[0]
	fs := flag.NewFlagSet("plugin "+verb, flag.ExitOnError)
	dir := fs.String("d", defaultConfigDir(), "config directory")
	cfgPath := fs.String("c", "", "serve config, for server.dataDir")
	_ = fs.Parse(pullFlags(args[1:]))
	if fs.NArg() != 2 {
		log.Fatal("usage: dw plugin enable|disable -c config.yaml [-d dir] <user> <name>")
	}
	dataDir, err := cliops.DataDirFromServeConfig(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	if verb == "enable" {
		err = cliops.PluginEnable(*dir, dataDir, fs.Arg(0), fs.Arg(1))
	} else {
		err = cliops.PluginDisable(*dir, dataDir, fs.Arg(0), fs.Arg(1))
	}
	if err != nil {
		log.Fatal(err)
	}
}
