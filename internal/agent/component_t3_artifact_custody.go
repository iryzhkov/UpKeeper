package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/redact"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// A restarted T3 becomes ready seconds after systemd reports the restart, and
// its Steward PartOf dependents restart with it. Readiness is therefore a
// bounded series of complete samples, never one early sample and never an
// unbounded wait. Tests replace the bound and use the context clock.
var (
	t3ReadinessAttempts = 6
	t3ReadinessInterval = 5 * time.Second
)

// t3RetainedSamples bounds the failed samples one transaction journal keeps:
// the first failure of each stage is kept, then the most recent ones.
const t3RetainedSamples = 12

// t3EvidenceText bounds and redacts free text before it is persisted or
// returned, so a cause quoting command output cannot leak a credential.
func t3EvidenceText(s string) string {
	if len(s) > 400 {
		s = s[:400]
	}
	return redact.Text(s)
}

// t3PredicateSample is the evidence of one readiness observation: each
// installed, served, health, unit and native predicate and which of them failed.
type t3PredicateSample map[string]any

func t3ObservedSample(obs map[string]any, err error) t3PredicateSample {
	s := t3PredicateSample{}
	if err != nil {
		s["observe_error"] = t3EvidenceText(err.Error())
		return s
	}
	for _, key := range []string{"installed", "served", "service_active", "check_ok", "running_threads", "artifact_installed", "artifact_served"} {
		if v, ok := obs[key]; ok {
			s[key] = v
		}
	}
	for _, key := range []string{"check_error", "artifact_error"} {
		if v, ok := obs[key].(string); ok && v != "" {
			s[key] = t3EvidenceText(v)
		}
	}
	return s
}

// retain keeps a failed sample in the transaction so the next journal record
// carries it. Past the bound the oldest repeat of a stage is dropped, so the
// first failure of every stage and the newest samples survive.
func (tx *t3Transaction) retain(sample t3PredicateSample) {
	tx.samples = append(tx.samples, sample)
	for len(tx.samples) > t3RetainedSamples {
		drop := 0
		seen := map[any]bool{}
		for i, s := range tx.samples {
			if seen[s["stage"]] {
				drop = i
				break
			}
			seen[s["stage"]] = true
		}
		tx.samples = append(tx.samples[:drop], tx.samples[drop+1:]...)
	}
}

// awaitReady samples check until it reports no failed predicate or the bound
// is spent. A terminal failure, such as unit or installed identity drift, can
// not become ready by waiting and ends the probe at once. Every failed sample
// is retained as transaction evidence.
func (tx *t3Transaction) awaitReady(stage string, check func() (sample t3PredicateSample, failed []string, terminal bool)) error {
	for attempt := 1; ; attempt++ {
		sample, failed, terminal := check()
		if len(failed) == 0 {
			return nil
		}
		sample["stage"], sample["attempt"], sample["failed"] = stage, attempt, failed
		sample["at"] = tx.c.now().UTC().Format(time.RFC3339Nano)
		tx.retain(sample)
		if terminal || attempt >= t3ReadinessAttempts {
			return fmt.Errorf("T3 %s verification failed after %d bounded samples: %s", stage, attempt, strings.Join(failed, ", "))
		}
		tx.c.sleep(t3ReadinessInterval)
	}
}

// t3Dependents names the units bound to the T3 service with PartOf= (systemd
// reports them as ConsistsOf=) with the invocation of each active one. A T3
// restart restarts them too, so readiness includes their return under a new
// invocation. An unanswerable query is unknown and refuses the restart rather
// than assuming there are none.
func t3Dependents(c *Context, service string) (map[string]string, error) {
	result := c.Run([]string{"systemctl", "--user", "show", service, "-p", "ConsistsOf", "--value"}, 10*time.Second, "")
	if !result.OK {
		return nil, errors.New("T3 dependent units unknown")
	}
	dependents := map[string]string{}
	for _, unit := range strings.Fields(result.Output) {
		if !c.UnitActive(unit) {
			dependents[unit] = ""
			continue
		}
		id := t3Invocation(c, unit)
		if id == "" {
			return nil, fmt.Errorf("T3 dependent unit %s identity unknown", unit)
		}
		dependents[unit] = id
	}
	return dependents, nil
}

func t3Invocation(c *Context, unit string) string {
	result := c.Run([]string{"systemctl", "--user", "show", unit, "-p", "InvocationID", "--value"}, 10*time.Second, "")
	if !result.OK {
		return ""
	}
	return strings.TrimSpace(result.Output)
}

// t3DependentsReturned adds the dependent predicate to a sample: every unit
// that was active before the restart is active again under a new invocation.
func t3DependentsReturned(c *Context, before map[string]string, sample t3PredicateSample) []string {
	failed := []string{}
	states := map[string]any{}
	for unit, previous := range before {
		if previous == "" {
			continue
		}
		active := c.UnitActive(unit)
		current := t3Invocation(c, unit)
		restarted := active && current != "" && current != previous
		states[unit] = map[string]any{"active": active, "restarted": restarted}
		if !restarted {
			failed = append(failed, "dependent "+unit)
		}
	}
	if len(states) > 0 {
		sample["dependents"] = states
	}
	return failed
}

// t3PriorServing checks that the service serves the identity the transaction
// found: retained controlled or upstream provenance when there was one, and
// otherwise the healthy prior version from the ordinary installed package.
func t3PriorServing(c *Context, spec, priorSpec map[string]any, prefix string, priorServed any, retained *release.T3Retained, upstream bool, sample t3PredicateSample) []string {
	service := adapterSpecString(spec, "service")
	obs, e := t3Observe(c, spec)
	for k, v := range t3ObservedSample(obs, e) {
		sample[k] = v
	}
	if e != nil {
		return []string{"observe"}
	}
	failed := []string{}
	if obs["service_active"] != true {
		failed = append(failed, "service_active")
	}
	if obs["check_ok"] != true {
		failed = append(failed, "check_ok")
	}
	if obs["served"] != priorServed {
		failed = append(failed, "served")
	}
	identity := true
	if upstream {
		identity = t3UpstreamServed(c, priorSpec, prefix)
		sample["upstream_served"] = identity
	}
	if retained != nil {
		identity = t3UnitSelectsOwned(c, service, prefix) && t3ServedArtifact(c, priorSpec, retained)
		sample["prior_artifact_served"] = identity
	}
	if !identity {
		failed = append(failed, "prior_identity_served")
	}
	return failed
}
