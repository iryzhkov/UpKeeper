package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

type ActivityReceipt struct {
	Host                   string `json:"host"`
	CoordinatorEpoch       string `json:"coordinator_epoch"`
	CollectionDeferred     *bool  `json:"collection_deferred"`
	RunningWithoutActivity *bool  `json:"running_without_activity"`
}

// ActivityCommand is a reviewed adapter for the coordinator's activity and
// collection predicate. Missing, failed, partial or stale evidence refuses.
func (b *ProductionBackend) observeTarget(ctx context.Context, c Config, host string, s *State) error {
	if _, err := acceptedPackage(c, s); err != nil {
		return err
	}
	if !contains(rc122Order, host) || len(c.ActivityCommand) == 0 {
		return errors.New("host observation requires reviewed activity adapter")
	}
	if err := b.timers(ctx, c, true); err != nil {
		return err
	}
	var expected identity
	if err := evidence(s, "identity-post:"+host, &expected); err != nil {
		return err
	}
	id, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if id.Health != "healthy" || id.Epoch != expected.Epoch || id.Release != expected.Release || id.Epoch == "" {
		return errors.New("observation coordinator identity or epoch changed")
	}
	ws, err := b.workers(ctx, c)
	if err != nil {
		return err
	}
	if err = drains(ws, c); err != nil {
		return err
	}
	for _, wHost := range c.ActiveWorkers {
		found := 0
		for _, w := range ws {
			if w.Requirement.WorkerID == wHost {
				found++
				if w.Health != "ready" || w.Stale || w.Snapshot.CoordinatorEpoch != id.Epoch {
					return errors.New("active worker not ready on observation epoch")
				}
			}
		}
		if found != 1 {
			return errors.New("active worker missing or duplicated during observation")
		}
	}
	if err = b.waitWorker(ctx, c, host, id.Epoch.String(), c.Version); err != nil {
		return err
	}
	args := append(append([]string{}, c.ActivityCommand[1:]...), host, id.Epoch.String())
	raw, err := b.command(ctx, c, "", c.ActivityCommand[0], args...)
	if err != nil {
		return err
	}
	var a ActivityReceipt
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&a); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("activity adapter trailing JSON")
	}
	if a.Host != host || a.CoordinatorEpoch != id.Epoch.String() || a.CollectionDeferred == nil ||
		a.RunningWithoutActivity == nil || *a.CollectionDeferred || *a.RunningWithoutActivity {
		return errors.New("activity or collection predicate failed")
	}
	var pre map[string]string
	if err = evidence(s, "host-pre:"+host, &pre); err != nil {
		return err
	}
	var assets assetEvidence
	if err = acceptedAssets(c, s, &assets); err != nil {
		return err
	}
	raw, err = b.on(ctx, c, host, snapshotScript)
	if err != nil {
		return err
	}
	if err = snapshotGate(pre, keyValues(raw), c.Version, assets.Binaries, c.InactiveServices[host]); err != nil {
		return err
	}
	final, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if final != id {
		return errors.New("coordinator changed during observation sample")
	}
	return nil
}
