package sge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/seqyuan/deepwarper/internal/model"
)

// Renew extends a running job's own scheduler ceiling (h_rt) with `qalter`, so a
// long-lived instance is not killed by SGE while the platform still wants it.
//
// inst.LeaseExpiresAt is the authority for "close enough": it moves forward on
// every renewal, so a scan tick cannot renew twice for the same window. h_rt
// counts from job start, not from now, so the new value is elapsed + renewFor.
//
// A failure here is a countdown, never a kill: the error is reported and the
// record is left alone. Many sites refuse `qalter` on a running job, which is
// exactly why renewal is opt-in and the warning exists.
func (b *Backend) Renew(ctx context.Context, inst *model.Instance) (bool, error) {
	if inst == nil || strings.TrimSpace(inst.BackendRef) == "" || inst.LeaseExpiresAt.IsZero() {
		return false, nil
	}
	if b.RenewBefore <= 0 {
		return false, nil
	}
	now := b.now()
	if now.Before(inst.LeaseExpiresAt.Add(-b.RenewBefore)) {
		return false, nil
	}
	renewFor := b.RenewFor
	if renewFor <= 0 {
		if d, err := parseWalltime(inst.Walltime); err == nil {
			renewFor = d
		}
	}
	if renewFor <= 0 {
		return false, errors.New("h_rt renewal is on but neither renewFor nor the instance walltime gives a duration")
	}
	elapsed := now.Sub(inst.StartedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	lease := (elapsed + renewFor).Truncate(time.Second)

	// `qalter -l` REPLACES the resource list, so h_vmem (and any GPU request) must
	// be restated or the scheduler rejects the change for a running job ("former
	// resource request on consumable h_vmem ... missing in new resource
	// request"). resourceLimits is the same renderer qsub used, which is what
	// keeps the two in step.
	limits, err := b.resourceLimits(inst.CPU, inst.Memory, formatHRT(lease), inst.GPU)
	if err != nil {
		return false, err
	}
	if len(limits) == 0 {
		return false, errors.New("h_rt renewal produced no resource list")
	}
	args := append(limits, inst.BackendRef)
	out, err := b.runner().Run(ctx, b.bin("qalter"), args...)
	if err != nil {
		return false, fmt.Errorf("qalter %s %s: %w: %s", inst.BackendRef, strings.Join(args, " "), err, firstLine(out))
	}
	inst.LeaseExpiresAt = inst.StartedAt.Add(lease)
	inst.LeaseWarnedAt = time.Time{} // re-arm the warning for the new window
	return true, nil
}

// LeaseWarning reports whether a live instance is close enough to its scheduler
// ceiling to deserve a warning, with a human-readable reason. Deduplication is
// the manager's job (inst.LeaseWarnedAt).
func (b *Backend) LeaseWarning(inst *model.Instance) (string, bool) {
	if inst == nil || inst.LeaseExpiresAt.IsZero() || b.WarnBefore <= 0 {
		return "", false
	}
	remaining := inst.LeaseExpiresAt.Sub(b.now())
	if remaining > b.WarnBefore {
		return "", false
	}
	if remaining < 0 {
		remaining = 0
	}
	msg := fmt.Sprintf("the job's h_rt ceiling expires in %s (at %s UTC)",
		remaining.Round(time.Second), inst.LeaseExpiresAt.UTC().Format(time.RFC3339))
	if b.RenewBefore > 0 {
		msg += "; renewal is enabled but has not moved the lease"
	} else {
		msg += "; renewal is disabled, so the scheduler will end the job"
	}
	return msg, true
}
