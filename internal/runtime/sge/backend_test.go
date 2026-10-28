package sge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/deepwarper/internal/agent"
	"github.com/seqyuan/deepwarper/internal/model"
	"github.com/seqyuan/deepwarper/internal/rendezvous"
	"github.com/seqyuan/deepwarper/internal/runtime"
)

func TestStartWritesSpecAndSubmits(t *testing.T) {
	b := testBackend(t)
	runner := b.Runner.(*fakeRunner)
	runner.on("qsub", cmdResult{out: "Your job 12345 (\"dw-alice-7f3a1c2e\") has been submitted\n"})
	inst, dist := writableSpec(t)
	spec := hostSpec(inst, dist, 24105)

	h, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if h.Ref() != "12345" {
		t.Fatalf("ref: %q", h.Ref())
	}
	if h.PID() != 0 {
		t.Fatalf("a job has no login-node pid, got %d", h.PID())
	}
	if h.Limiter() != "sge" {
		t.Fatalf("limiter: %q", h.Limiter())
	}

	// spec.json: 0600, box paths, Port == 0 and LoginPort == the pool port.
	path := filepath.Join(spec.ControlDir, "spec.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("spec.json mode: %v", fi.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var box agent.Spec
	if err := json.Unmarshal(data, &box); err != nil {
		t.Fatal(err)
	}
	if box.Port != 0 || box.LoginPort != 24105 {
		t.Fatalf("box ports: port=%d login=%d", box.Port, box.LoginPort)
	}
	if box.Home != b.HomeBox || box.Workspace != b.WorkspaceBox {
		t.Fatalf("box paths: %+v", box)
	}

	// script: 0755, in submitDir, named after the unit.
	scriptPath := filepath.Join(b.SubmitDir, "dw-alice-7f3a1c2e.sh")
	sfi, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if sfi.Mode().Perm() != 0o755 {
		t.Fatalf("script mode: %v", sfi.Mode().Perm())
	}

	// qsub argv = header + script path.
	args, err := b.headerArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	args = append(args, scriptPath)
	calls := runner.callsFor("qsub")
	if len(calls) != 1 {
		t.Fatalf("qsub calls: %d", len(calls))
	}
	if strings.Join(calls[0].args, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("qsub argv:\n got %q\nwant %q", calls[0].args, args)
	}

	// The login node publishes the id under the same name the script uses.
	if got, _ := rendezvous.Read(spec.ControlDir, "jobid"); got != "12345" {
		t.Fatalf("jobid: %q", got)
	}
	if got, _ := rendezvous.Read(spec.ControlDir, "state"); got != "submitted" {
		t.Fatalf("state: %q", got)
	}
}

func TestStartQsubFailureLeavesNoHandle(t *testing.T) {
	b := testBackend(t)
	b.Runner.(*fakeRunner).on("qsub", cmdResult{
		out: "qsub: syntax error near line 3\ndetails follow",
		err: errors.New("exit status 2"),
	})
	inst, dist := writableSpec(t)
	spec := hostSpec(inst, dist, 24105)

	h, err := b.Start(t.Context(), spec)
	if err == nil {
		t.Fatalf("expected failure, got handle %v", h)
	}
	if !strings.Contains(err.Error(), "syntax error near line 3") {
		t.Fatalf("error must name the first output line: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(spec.ControlDir, "jobid")); !os.IsNotExist(statErr) {
		t.Fatalf("a failed submit must not publish a job id (%v)", statErr)
	}
}

func TestStartStaleRendezvousCleared(t *testing.T) {
	b := testBackend(t)
	b.Runner.(*fakeRunner).on("qsub", cmdResult{out: "12345\n"})
	inst, dist := writableSpec(t)
	spec := hostSpec(inst, dist, 24105)
	stale := rendezvous.Ready{Version: 1, Incarnation: "previous", State: "ready", Host: "127.0.0.1", Port: 9999}
	if err := rendezvous.WriteReady(spec.ControlDir, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := rendezvous.ReadReady(spec.ControlDir); !os.IsNotExist(err) {
		t.Fatalf("a stale ready.json must be gone before submit, got %v", err)
	}
}

func TestProbePhases(t *testing.T) {
	b := testBackend(t)
	inst, _ := writableSpec(t)
	runner := b.Runner.(*fakeRunner)

	cases := []struct {
		name     string
		qstat    cmdResult
		want     runtime.Phase
		wantNote string
	}{
		{"queued", cmdResult{out: readFixture(t, "qstat-queued.xml")}, runtime.PhaseQueued, "queued: position 1"},
		{"running", cmdResult{out: readFixture(t, "qstat-running.xml")}, runtime.PhaseRunning, ""},
		{"suspended", cmdResult{out: readFixture(t, "qstat-suspended.xml")}, runtime.PhaseSuspended, "suspended"},
		{"eqw", cmdResult{out: readFixture(t, "qstat-eqw.xml")}, runtime.PhaseFailed, "Eqw"},
		{"qstat failed", cmdResult{out: "", err: errors.New("exit status 1")}, runtime.PhaseUnknown, "qstat"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runner.on("qstat", c.qstat)
			b.InvalidateCache()
			job := inst
			job.BackendRef = "1837442"
			if c.name == "running" {
				job.BackendRef = "1837443"
			} else if c.name == "suspended" {
				job.BackendRef = "1837444"
			} else if c.name == "eqw" {
				job.BackendRef = "1837446"
			}
			obs, err := b.Probe(t.Context(), &job)
			if c.want == runtime.PhaseUnknown {
				if err == nil {
					t.Fatal("a qstat failure must be reported as an error")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if obs.Phase != c.want {
				t.Fatalf("phase: %s want %s (%+v)", obs.Phase, c.want, obs)
			}
			if c.wantNote != "" && !strings.Contains(obs.Note, c.wantNote) {
				t.Fatalf("note %q must mention %q", obs.Note, c.wantNote)
			}
		})
	}

	// A job the scheduler does not know needs three confirmations.
	t.Run("gone needs three readings", func(t *testing.T) {
		b := testBackend(t)
		runner := b.Runner.(*fakeRunner)
		runner.on("qstat", cmdResult{out: readFixture(t, "qstat-empty.xml")})
		job := inst
		for i := 1; i <= 2; i++ {
			b.InvalidateCache()
			obs, err := b.Probe(t.Context(), &job)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Phase != runtime.PhaseUnknown || obs.Gone() {
				t.Fatalf("reading %d must not be a death certificate: %+v", i, obs)
			}
			if !strings.Contains(obs.Note, "pending gone confirmation") {
				t.Fatalf("reading %d note: %q", i, obs.Note)
			}
		}
		b.InvalidateCache()
		obs, err := b.Probe(t.Context(), &job)
		if err != nil {
			t.Fatal(err)
		}
		if obs.Phase != runtime.PhaseGone {
			t.Fatalf("third reading must be gone: %+v", obs)
		}
	})

	// An unknown reading (a failed qstat) is not evidence and resets the count.
	t.Run("unknown resets the counter", func(t *testing.T) {
		b := testBackend(t)
		runner := b.Runner.(*fakeRunner)
		job := inst
		runner.on("qstat", cmdResult{out: readFixture(t, "qstat-empty.xml")}, cmdResult{out: readFixture(t, "qstat-empty.xml")},
			cmdResult{out: "", err: errors.New("exit status 1")},
			cmdResult{out: readFixture(t, "qstat-empty.xml")})
		for i := 0; i < 2; i++ {
			b.InvalidateCache()
			if obs, _ := b.Probe(t.Context(), &job); obs.Phase == runtime.PhaseGone {
				t.Fatalf("reading %d must not settle", i)
			}
		}
		b.InvalidateCache()
		if _, err := b.Probe(t.Context(), &job); err == nil {
			t.Fatal("expected a qstat error")
		}
		b.InvalidateCache()
		obs, _ := b.Probe(t.Context(), &job)
		if obs.Phase == runtime.PhaseGone {
			t.Fatalf("a qstat outage must reset the confirmation count: %+v", obs)
		}
	})
}

// TestProbeUsesPlainQstatXML is the `qstat -xml -j` regression guard: the -j form
// emits a document this parser cannot read, so every liveness query would answer
// "unknown".
func TestProbeUsesPlainQstatXML(t *testing.T) {
	b := testBackend(t)
	runner := b.Runner.(*fakeRunner)
	runner.on("qstat", cmdResult{out: readFixture(t, "qstat-running.xml")})
	inst, _ := writableSpec(t)
	inst.BackendRef = "1837443"
	if _, err := b.Probe(t.Context(), &inst); err != nil {
		t.Fatal(err)
	}
	calls := runner.callsFor("qstat")
	if len(calls) != 1 {
		t.Fatalf("qstat calls: %d", len(calls))
	}
	if strings.Join(calls[0].args, " ") != "-xml" {
		t.Fatalf("qstat argv must be exactly -xml, got %q", calls[0].args)
	}
}

func TestProbeCachesOneQstatPerTick(t *testing.T) {
	b := testBackend(t)
	b.Runner.(*fakeRunner).on("qstat", cmdResult{out: readFixture(t, "qstat-running.xml")})
	first, _ := writableSpec(t)
	second := first
	second.ID = "bob-default"

	if _, err := b.Probe(t.Context(), &first); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Probe(t.Context(), &second); err != nil {
		t.Fatal(err)
	}
	if n := b.Runner.(*fakeRunner).count("qstat"); n != 1 {
		t.Fatalf("one qstat per tick, got %d", n)
	}
}

func TestProbeWithoutJobIDIsUnknown(t *testing.T) {
	b := testBackend(t)
	inst, _ := writableSpec(t)
	inst.BackendRef = ""
	obs, err := b.Probe(t.Context(), &inst)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Phase != runtime.PhaseUnknown || obs.Gone() {
		t.Fatalf("a record with no job id is not evidence of death: %+v", obs)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	b := testBackend(t)
	runner := b.Runner.(*fakeRunner)
	inst, _ := writableSpec(t)

	// "not found" is the expected outcome of a race with completion.
	runner.on("qdel", cmdResult{out: "qdel: Unknown Job Id 12345", err: errors.New("exit status 1")})
	if err := b.StopInstance(t.Context(), &inst); err != nil {
		t.Fatalf("unknown job must be success: %v", err)
	}

	runner.on("qdel", cmdResult{out: "qdel: permission denied", err: errors.New("exit status 1")})
	if err := b.StopInstance(t.Context(), &inst); err == nil {
		t.Fatal("a real qdel failure must be reported")
	}

	before := runner.count("qdel")
	empty := inst
	empty.BackendRef = ""
	if err := b.StopInstance(t.Context(), &empty); err != nil {
		t.Fatal(err)
	}
	if runner.count("qdel") != before {
		t.Fatal("a record that was never submitted must not call qdel")
	}
}

func TestStopDeletesJobBeforeTunnel(t *testing.T) {
	b := testBackend(t)
	runner := b.Runner.(*fakeRunner)
	runner.on("qdel", cmdResult{out: "12345 has been deleted\n"})
	inst, _ := writableSpec(t)
	tun := &fakeTunnel{alive: true}
	b.setTunnel(inst.ID, tun)
	if err := b.Stop(t.Context(), &inst); err != nil {
		t.Fatal(err)
	}
	if runner.count("qdel") != 1 {
		t.Fatal("qdel must run")
	}
	if !tun.stopped {
		t.Fatal("the tunnel must be torn down")
	}
}

func TestRenewRestatesHvmem(t *testing.T) {
	b := testBackend(t)
	now := time.Date(2026, 3, 1, 15, 0, 0, 0, time.UTC)
	b.Now = func() time.Time { return now }
	runner := b.Runner.(*fakeRunner)
	runner.on("qalter", cmdResult{out: "modified job 12345\n"})

	inst, _ := writableSpec(t)
	inst.StartedAt = now.Add(-4 * time.Hour)
	inst.LeaseExpiresAt = inst.StartedAt.Add(8 * time.Hour) // 4h left, inside renewBefore=20m? no
	inst.LeaseExpiresAt = now.Add(10 * time.Minute)         // inside the renewal window
	inst.LeaseWarnedAt = now

	renewed, err := b.Renew(t.Context(), &inst)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed {
		t.Fatal("expected a renewal")
	}
	calls := runner.callsFor("qalter")
	if len(calls) != 1 {
		t.Fatalf("qalter calls: %d", len(calls))
	}
	args := strings.Join(calls[0].args, " ")
	// `qalter -l` REPLACES the resource list: dropping h_vmem makes SGE refuse
	// the change on a running job.
	if !strings.Contains(args, "-l h_vmem=4G,h_rt=") {
		t.Fatalf("qalter must restate h_vmem: %q", args)
	}
	if !strings.HasSuffix(args, "12345") {
		t.Fatalf("qalter must name the job id: %q", args)
	}
	if !inst.LeaseWarnedAt.IsZero() {
		t.Fatal("a renewal must re-arm the lease warning")
	}
	if want := inst.StartedAt.Add(6 * time.Hour); !inst.LeaseExpiresAt.Equal(want) {
		t.Fatalf("lease: %s want %s", inst.LeaseExpiresAt, want)
	}
}

func TestRenewDoesNotFireTwiceForSameWindow(t *testing.T) {
	b := testBackend(t)
	now := time.Date(2026, 3, 1, 15, 0, 0, 0, time.UTC)
	b.Now = func() time.Time { return now }
	b.Runner.(*fakeRunner).on("qalter", cmdResult{out: "modified\n"})
	inst, _ := writableSpec(t)
	inst.StartedAt = now.Add(-4 * time.Hour)
	inst.LeaseExpiresAt = now.Add(10 * time.Minute)

	for i := 0; i < 3; i++ {
		if _, err := b.Renew(t.Context(), &inst); err != nil {
			t.Fatal(err)
		}
	}
	if n := b.Runner.(*fakeRunner).count("qalter"); n != 1 {
		t.Fatalf("one qalter per lease window, got %d", n)
	}
}

func TestRenewalFailureDoesNotStop(t *testing.T) {
	b := testBackend(t)
	now := time.Date(2026, 3, 1, 15, 0, 0, 0, time.UTC)
	b.Now = func() time.Time { return now }
	b.Runner.(*fakeRunner).on("qalter", cmdResult{out: "qalter: denied", err: errors.New("exit status 1")})
	inst, _ := writableSpec(t)
	inst.StartedAt = now.Add(-4 * time.Hour)
	lease := now.Add(10 * time.Minute)
	inst.LeaseExpiresAt = lease

	renewed, err := b.Renew(t.Context(), &inst)
	if err == nil || renewed {
		t.Fatalf("expected a renewal error, got renewed=%v err=%v", renewed, err)
	}
	if !inst.LeaseExpiresAt.Equal(lease) {
		t.Fatalf("a failed renewal must not move the lease: %s", inst.LeaseExpiresAt)
	}
	if n := b.Runner.(*fakeRunner).count("qdel"); n != 0 {
		t.Fatalf("a failed renewal must never stop the job, qdel calls: %d", n)
	}
}

func TestLeaseWarningThresholds(t *testing.T) {
	now := time.Date(2026, 3, 1, 15, 0, 0, 0, time.UTC)
	newBackend := func(warn, renewBefore time.Duration) *Backend {
		b := testBackend(t)
		b.Now = func() time.Time { return now }
		b.WarnBefore = warn
		b.RenewBefore = renewBefore
		return b
	}
	inst, _ := writableSpec(t)

	// disabled
	inst.LeaseExpiresAt = now.Add(time.Minute)
	if _, warn := newBackend(0, time.Hour).LeaseWarning(&inst); warn {
		t.Fatal("warnBefore=0 disables the warning")
	}
	// outside the window
	inst.LeaseExpiresAt = now.Add(2 * time.Hour)
	if _, warn := newBackend(30*time.Minute, time.Hour).LeaseWarning(&inst); warn {
		t.Fatal("2h remaining is outside a 30m window")
	}
	// inside the window, renewal enabled
	inst.LeaseExpiresAt = now.Add(10 * time.Minute)
	msg, warn := newBackend(30*time.Minute, time.Hour).LeaseWarning(&inst)
	if !warn || !strings.Contains(msg, "renewal is enabled") {
		t.Fatalf("message: %q warn=%v", msg, warn)
	}
	// past the ceiling
	inst.LeaseExpiresAt = now.Add(-time.Minute)
	msg, warn = newBackend(30*time.Minute, 0).LeaseWarning(&inst)
	if !warn || !strings.Contains(msg, "renewal is disabled") || strings.Contains(msg, "in -") {
		t.Fatalf("message: %q warn=%v", msg, warn)
	}
}

func TestValidateAggregatesProblems(t *testing.T) {
	b := &Backend{Config: Config{
		PE:             "",
		RenewFor:       time.Hour,
		RenewBefore:    0,
		WarnBefore:     10 * time.Minute,
		ApptainerImage: "relative.sif",
	}}
	err := b.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, want := range []string{"submitDir", "rendezvousDir", "pe is required", "renewFor", "renewBefore", "apptainerImage"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error must mention %q: %s", want, msg)
		}
	}
}

func TestValidateRejectsControlRootOutsideRendezvous(t *testing.T) {
	b := &Backend{Config: Config{
		SubmitDir:      "/data/dw/sge/submit",
		RendezvousDir:  "/data/dw/rdv",
		ControlRoot:    "/var/lib/deepwarper/control",
		PE:             "smp",
		Tunnel:         true,
		ApptainerImage: "/data/dw/images/dw-runtime.sif",
	}}
	err := b.Validate()
	if err == nil || !strings.Contains(err.Error(), "must contain") {
		t.Fatalf("expected a containment error, got %v", err)
	}
}

func TestValidateRejectsContradictoryTunnel(t *testing.T) {
	b := &Backend{Config: Config{
		SubmitDir:      "/data/dw/sge/submit",
		RendezvousDir:  "/data/dw",
		ControlRoot:    "/data/dw/control",
		PE:             "smp",
		Tunnel:         true,
		DirectDial:     true,
		ApptainerImage: "/data/dw/images/dw-runtime.sif",
	}}
	if err := b.Validate(); err == nil {
		t.Fatal("tunnel and directDial are contradictory")
	}
	// And no tunnel without direct dial: the gateway only dials loopback.
	b.Tunnel = false
	b.DirectDial = false
	if err := b.Validate(); err == nil {
		t.Fatal("tunnel=false requires directDial=true")
	}
}

// TestPolicyKeepsTheReaperOff pins the ADR decision: on SGE the lifecycle ceiling
// is h_rt, so idle reaping is off unless the site asks for it.
func TestPolicyKeepsTheReaperOff(t *testing.T) {
	b := testBackend(t)
	if b.IdleReapAllowed() {
		t.Fatal("idle reaping must be off by default on SGE")
	}
	if b.LaunchBudget() != 0 {
		t.Fatalf("Ensure must not block on a queue wait, budget=%s", b.LaunchBudget())
	}
	b.IdleReap = true
	if !b.IdleReapAllowed() {
		t.Fatal("a site that opts in must be honoured")
	}
}

func TestLeaseExpiresAtFromWalltime(t *testing.T) {
	b := testBackend(t)
	b.Runner.(*fakeRunner).on("qsub", cmdResult{out: "12345\n"})
	inst, dist := writableSpec(t)
	spec := hostSpec(inst, dist, 24105)
	h, err := b.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	lh, ok := h.(runtime.LeaseHandle)
	if !ok {
		t.Fatal("the sge handle must expose its lease")
	}
	started := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if got := lh.LeaseExpiresAt(started); !got.Equal(started.Add(8 * time.Hour)) {
		t.Fatalf("lease: %s", got)
	}
	noWalltime := spec
	noWalltime.Walltime = ""
	h2, err := b.Start(t.Context(), noWalltime)
	if err != nil {
		t.Fatal(err)
	}
	if got := h2.(runtime.LeaseHandle).LeaseExpiresAt(started); !got.IsZero() {
		t.Fatalf("no walltime means no lease, got %s", got)
	}
}

func TestStopSettlesSuspendedJobToo(t *testing.T) {
	// A suspended job is alive, so an explicit stop must still qdel it: "do not
	// reap" is not "cannot be stopped".
	b := testBackend(t)
	b.Runner.(*fakeRunner).on("qdel", cmdResult{out: "deleted\n"})
	inst, _ := writableSpec(t)
	inst.State = model.StateSuspended
	if err := b.Stop(t.Context(), &inst); err != nil {
		t.Fatal(err)
	}
	if n := b.Runner.(*fakeRunner).count("qdel"); n != 1 {
		t.Fatalf("qdel calls: %d", n)
	}
}

// TestStartDoesNotClobberAJobThatAlreadyRan: a job can begin (and even fail)
// before qsub returns to the login node, and its newer state must survive.
func TestStartDoesNotClobberAJobThatAlreadyRan(t *testing.T) {
	b := testBackend(t)
	runner := b.Runner.(*fakeRunner)
	inst, dist := writableSpec(t)
	spec := hostSpec(inst, dist, 24105)
	// The job starts on the compute node and fails while qsub is still "running".
	runner.onRun = func(c cmdCall) {
		if c.name == "qsub" {
			_ = rendezvous.WriteString(spec.ControlDir, "state", "failed", 0o644)
		}
	}
	runner.on("qsub", cmdResult{out: "12345\n"})
	if _, err := b.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if got, _ := rendezvous.Read(spec.ControlDir, "state"); got != "failed" {
		t.Fatalf("state was overwritten with %q", got)
	}
}
