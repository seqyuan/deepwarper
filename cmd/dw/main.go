package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/config"
	"github.com/seqyuan/deepwarper/internal/distribution"
	"github.com/seqyuan/deepwarper/internal/gateway"
	"github.com/seqyuan/deepwarper/internal/instance"
	"github.com/seqyuan/deepwarper/internal/runtime/local"
	"github.com/seqyuan/deepwarper/internal/state"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "instance":
		runInstance(os.Args[2:])
	case "doctor":
		doctor(os.Args[2:])
	case "version":
		fmt.Println(version)
	case "user":
		userCmd(os.Args[2:])
	case "passwd":
		passwdCmd(os.Args[2:])
	case "del":
		delCmd(os.Args[2:])
	case "mount":
		mountCmd(os.Args[2:])
	case "remote":
		remoteCmd(os.Args[2:])
	case "catalog":
		catalogCmd(os.Args[2:])
	case "plugin":
		pluginCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: dw serve -c config.yaml")
	fmt.Fprintln(os.Stderr, "       dw instance -spec spec.json")
	fmt.Fprintln(os.Stderr, "       dw doctor -c config.yaml")
	fmt.Fprintln(os.Stderr, "       dw user [-d dir] <name>")
	fmt.Fprintln(os.Stderr, "       dw passwd [-d dir] <name>")
	fmt.Fprintln(os.Stderr, "       dw del [-d dir] <name>")
	fmt.Fprintln(os.Stderr, "       dw mount add [-d dir] <user> --local host:target:ro|rw")
	fmt.Fprintln(os.Stderr, "       dw mount add [-d dir] <user> --remote name --prefix p --at target --ro|--rw")
	fmt.Fprintln(os.Stderr, "       dw remote add [-d dir] <name> --provider tos|s3 --endpoint url --region r --bucket b --credential-file path")
	fmt.Fprintln(os.Stderr, "       dw catalog add [-d dir] <name> <dir>")
	fmt.Fprintln(os.Stderr, "       dw plugin enable|disable [-d dir] <user> <name>")
	fmt.Fprintln(os.Stderr, "       dw version")
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("c", "", "config file")
	_ = fs.Parse(args)
	if *cfgPath == "" {
		log.Fatal("-c config.yaml is required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	logger := log.New(os.Stdout, "[dw] ", log.LstdFlags)
	// A single control plane per data directory: two `dw serve` processes would
	// interleave writes to the same records and ports.
	dataLock, err := state.LockDataDir(cfg.Server.DataDir)
	if err != nil {
		if errors.Is(err, state.ErrLocked) {
			log.Fatalf("data directory %s is already in use by another dw serve (%s)", cfg.Server.DataDir, filepath.Join(cfg.Server.DataDir, "locks", "serve.lock"))
		}
		log.Fatalf("lock data directory %s: %v", cfg.Server.DataDir, err)
	}
	defer dataLock.Release()
	mgr, err := instance.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	mgr.SetLogf(logger.Printf)
	if err := mgr.Reconcile(context.Background()); err != nil {
		logger.Printf("reconcile: %v", err)
	}
	gw, err := gateway.New(cfg, mgr, logger)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Consumed/expired tickets must not accumulate: random /_dw/launch
	// requests and normal one-time use both leave files behind.
	go gw.RunTicketJanitor(ctx, time.Minute)
	// One scan loop, not two: Reconcile must finish before Sweep sees the same
	// record, otherwise an idle sweep can stop an instance mid-adoption.
	go func() {
		ticker := time.NewTicker(cfg.ScanInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := mgr.Reconcile(ctx); err != nil {
					logger.Printf("reconcile: %v", err)
				}
				if err := mgr.Sweep(ctx); err != nil {
					logger.Printf("sweep: %v", err)
				}
			}
		}
	}()

	go func() {
		logger.Printf("listening on http://%s (subdomains of %s, dist=%s)", cfg.Server.Listen, cfg.Routing.HostSuffix, cfg.Runtime.DefaultDistribution)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	logger.Printf("shutting down (instances keep running)")
	cancel()
	_ = srv.Close()
}

func runInstance(args []string) {
	fs := flag.NewFlagSet("instance", flag.ExitOnError)
	spec := fs.String("spec", "", "instance spec JSON")
	_ = fs.Parse(args)
	if *spec == "" {
		log.Fatal("-spec is required")
	}
	os.Exit(agent.Run(context.Background(), *spec))
}

func doctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("c", "", "config file")
	_ = fs.Parse(args)
	if *cfgPath == "" {
		log.Fatal("-c config.yaml is required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	problems := 0
	fmt.Printf("config: ok (%s)\n", *cfgPath)
	if _, err := os.Stat(cfg.Runtime.DSHBin); err != nil {
		fmt.Printf("dsh: FAIL %v\n", err)
		problems++
	} else {
		fmt.Printf("dsh: ok (%s)\n", cfg.Runtime.DSHBin)
	}
	for name, d := range cfg.Distributions {
		if err := distribution.VerifyProfile(d.Path); err != nil {
			fmt.Printf("dist %s: FAIL %v\n", name, err)
			problems++
		} else {
			fmt.Printf("dist %s: ok (%s, profile=%s)\n", name, d.Path, d.ProfileName)
		}
	}
	if cfg.Runtime.DWBin == "" {
		exe, _ := os.Executable()
		cfg.Runtime.DWBin = exe
	}
	if cfg.Runtime.DWBin == "" {
		fmt.Println("dw bin: FAIL (empty)")
		problems++
	} else if _, err := os.Stat(cfg.Runtime.DWBin); err != nil {
		fmt.Printf("dw bin: FAIL %v\n", err)
		problems++
	} else {
		fmt.Printf("dw bin: ok (%s)\n", cfg.Runtime.DWBin)
	}

	ctx := context.Background()
	localBackend := local.New(instance.LocalOptions(cfg))
	sysInfo := local.Diagnose(ctx, nil)
	if sysInfo.Available {
		fmt.Printf("systemd --user: ok (%s)\n", sysInfo.Detail)
	} else {
		fmt.Printf("systemd --user: unavailable (%s)\n", sysInfo.Detail)
	}
	if sysInfo.Version != "" {
		fmt.Printf("systemd: %s  cgroup: %s\n", sysInfo.Version, sysInfo.Cgroup)
	} else {
		fmt.Printf("systemd: version unknown  cgroup: %s\n", sysInfo.Cgroup)
	}
	fmt.Printf("linger: %s\n", sysInfo.Linger)
	resolved, resolveErr := localBackend.Resolve(ctx)
	if resolveErr != nil {
		fmt.Printf("limiter: FAIL %v\n", resolveErr)
		problems++
	} else {
		fmt.Printf("limiter: %s -> %s (systemdUser=%s)\n", cfg.Runtime.Local.Limiter, resolved.Limiter, resolved.SystemdUser)
	}
	fmt.Printf("tasksMax: %d\n", resolved.TasksMax)
	fmt.Printf("readyTimeout: %s (unit start %s, stopGrace %s, unitStdio=%s)\n",
		cfg.Runtime.ReadyTimeout, resolved.StartTimeout, resolved.StopGrace, resolved.UnitStdio)
	if cfg.Runtime.Moniforge == "" {
		fmt.Println("moniforge: not set")
	} else if _, err := os.Stat(cfg.Runtime.Moniforge); err != nil {
		fmt.Printf("moniforge: warning: %v\n", err)
	} else {
		fmt.Printf("moniforge: ok (%s)\n", cfg.Runtime.Moniforge)
	}
	if cfg.Auth.UsersDir == "" {
		fmt.Println("users dir: login off")
	} else {
		fmt.Printf("users dir: %s\n", cfg.Auth.UsersDir)
	}
	for _, name := range []string{"bwrap", "rclone"} {
		fmt.Print(toolStatus(name, exec.LookPath))
	}
	if problems > 0 {
		os.Exit(1)
	}
}

func toolStatus(name string, look func(string) (string, error)) string {
	path, err := look(name)
	if err != nil {
		return fmt.Sprintf("%s: warning: not found\n", name)
	}
	return fmt.Sprintf("%s: ok (%s)\n", name, path)
}
