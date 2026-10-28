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
	"strings"
	"syscall"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/config"
	"github.com/seqyuan/deepwarper/internal/distribution"
	"github.com/seqyuan/deepwarper/internal/gateway"
	"github.com/seqyuan/deepwarper/internal/instance"
	"github.com/seqyuan/deepwarper/internal/runtime/local"
	"github.com/seqyuan/deepwarper/internal/runtime/sge"
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
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: dw serve -c config.yaml | dw instance -spec spec.json | dw doctor -c config.yaml | dw version")
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
	// Instances that are not settled yet (a queued job, a launching process) are
	// re-checked on the fast interval. Without an SGE section the fast interval
	// is exactly the scan interval, so local behaviour is unchanged.
	fastEvery := cfg.ScanInterval()
	if cfg.Runtime.SGE.IsEnabled() {
		if d, err := time.ParseDuration(cfg.Runtime.SGE.PollEvery); err == nil && d > 0 && d < fastEvery {
			fastEvery = d
		}
	}
	go func() {
		ticker := time.NewTicker(fastEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := mgr.Reconcile(ctx); err != nil {
					logger.Printf("reconcile: %v", err)
				}
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(cfg.ScanInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := mgr.Sweep(ctx); err != nil {
					logger.Printf("sweep: %v", err)
				}
				if err := mgr.RenewTick(ctx); err != nil {
					logger.Printf("renew: %v", err)
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
	if ctx, ok := sgeDoctor(cfg); !ok {
		problems += ctx
	}
	if problems > 0 {
		os.Exit(1)
	}
}

// sgeDoctor reports the SGE section's validity. It never submits anything: a
// probe job and an ssh reachability check belong to a submit host, not to
// `dw doctor` on a laptop. It returns the number of problems added.
func sgeDoctor(cfg *config.Config) (int, bool) {
	if cfg.Runtime.SGE == nil || !cfg.Runtime.SGE.IsEnabled() {
		return 0, true
	}
	problems := 0
	backend, err := sge.FromConfig(cfg)
	if err != nil {
		fmt.Printf("sge: FAIL %v\n", err)
		return 1, false
	}
	queue := backend.Queue
	if queue == "" {
		queue = "(cluster default)"
	}
	fmt.Printf("sge: ok (pe=%s slots/cores=%s queue=%s project=%s)\n",
		backend.PE, backend.PEAccounting, queue, orDefault(backend.Project, "(none)"))
	fmt.Printf("sge: submitDir=%s rendezvousDir=%s\n", backend.SubmitDir, backend.RendezvousDir)
	fmt.Printf("sge: image=%s dw=%s dsh=%s home=%s workspace=%s\n",
		backend.ApptainerImage, backend.DWBox, backend.DSHBox, backend.HomeBox, backend.WorkspaceBox)
	fmt.Printf("sge: tunnel=%v directDial=%v idleReap=%v renewBefore=%s warnBefore=%s\n",
		backend.Tunnel, backend.DirectDial, backend.IdleReap,
		durOrDefault(backend.RenewBefore), durOrDefault(backend.WarnBefore))
	// Binaries are only looked up, never run: a site may point them at wrapper
	// scripts that ssh to a submit host, and those do not exist here.
	for _, b := range []struct{ label, name string }{
		{"qsub", backend.Qsub},
		{"qstat", backend.Qstat},
		{"qdel", backend.Qdel},
		{"qalter", backend.Qalter},
		{"ssh", backend.SSH},
	} {
		if path, lookErr := exec.LookPath(b.name); lookErr != nil {
			fmt.Printf("sge %s: not found on PATH (%s) — fine unless this host is the submit host\n", b.label, b.name)
		} else {
			fmt.Printf("sge %s: ok (%s)\n", b.label, path)
		}
	}
	return problems, true
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func durOrDefault(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	return d.String()
}
