package fleet

import (
	"reflect"
	"sort"
	"strconv"
)

// CoordinatorPurpose labels the coordinator projection wherever it is reported.
// Steward consumes it before building its catalog; enrollment remains explicit.
const CoordinatorPurpose = "authored coordinator configuration; enrollment remains explicit"

// FenceAdvice is what an enrollment plan says instead of a digest.
//
// An enrollment is fenced against a catalog revision the coordinator computes
// over data UpKeeper does not hold, so any digest rendered here would be a
// number the coordinator will always reject. Printing one that merely looks
// like a fence is worse than printing none: it invites somebody to pass it and
// then to distrust the refusal. The plan names the command that takes the real
// one instead.
const FenceAdvice = "UpKeeper cannot compute the coordinator's catalog revision; " +
	"read it from `t3-steward backlog workers --json` and pass it to " +
	"`t3-steward worker enroll --catalog-revision <revision>`, or run " +
	"`t3-steward worker enroll <worker> --current-catalog` on the coordinator host, " +
	"which reads it itself"

// ProjectAddedAdvice is what the plan says under a project that the previous
// release did not carry. The coordinator loads such a project with default
// local bindings (no credentials, resource locks or directory resources) until
// its operator binds it explicitly in backlog_v2.projects, so the plan says
// that outright rather than leaving it to be discovered from a warning in the
// coordinator's log.
const ProjectAddedAdvice = "will be loaded by the coordinator with default local bindings " +
	"(no credentials, resource locks or directory resources) unless backlog_v2.projects " +
	"binds it explicitly"

// ReloadRefusalAdvice is what the plan says under any change that alters a
// worker's catalog revision: a project it is eligible for, or its own entry
// (models, quota pools, executor slots, capabilities, cpu class, provider
// instances). The coordinator re-reads the projection on SIGHUP but refuses
// the reload while a worker whose catalog changes still holds a retained
// assignment, and its receipt (read by the convergence, and by "t3-steward
// coordinator identity --json") names each one and how to unblock it.
const ReloadRefusalAdvice = "the coordinator refuses the reload while a worker whose catalog changes " +
	"holds a retained assignment (one not completed or released); its reload receipt names each " +
	"one and the command that unblocks it, and the convergence fails with that receipt"

// reenrollAdvice names the one command that brings a worker's enrollment up to
// a changed catalog, and where it runs. A project change alters the catalog
// digest of every worker it is eligible for, so each of them is stale until
// re-enrolled; naming the command per worker is what turns the diff into the
// list of actions an operator runs next.
func reenrollAdvice(worker string) string {
	return "worker " + worker + " needs re-enrollment for the changed catalog: run " +
		"`t3-steward worker enroll " + worker + " --current-catalog --reason TEXT` on the coordinator host"
}

// ModelSets is what one host is authorized to run and, separately, what was
// observed on it.
//
// Desired is always present, and an empty Desired means "no models are
// authorized here". Observed is present only when something genuinely observed
// that host. Collapsing those two answers is how "no route" became
// unexplainable, so HasObservation is reported rather than inferred from an
// empty map.
type ModelSets struct {
	Desired        map[string][]string
	Observed       map[string][]string
	HasObservation bool
}

// Models reports the desired and observed model sets of one host.
//
// It never computes an effective set. The effective set is the intersection of
// authorization, installation, authentication, the host's advertised cache and
// quota policy, and only the coordinator holds the accepted snapshot that
// answer must come from.
func (d *Document) Models(name string) (ModelSets, error) {
	profile, err := d.Resolved(name)
	if err != nil {
		return ModelSets{}, err
	}
	sets := ModelSets{Desired: map[string][]string{}}
	for instance, allowed := range profile.Models {
		sets.Desired[instance] = sortedUnique(allowed)
	}
	observation, present := d.hostObservation(name)
	if !present {
		return sets, nil
	}
	sets.HasObservation = true
	sets.Observed = map[string][]string{}
	models, _ := observation["models"].(map[string]any)
	for instance, value := range models {
		values, err := stringList(value)
		if err != nil {
			continue
		}
		sets.Observed[instance] = sortedUnique(values)
	}
	return sets, nil
}

func (d *Document) hostObservation(name string) (map[string]any, bool) {
	if d.observed == nil {
		return nil, false
	}
	hosts, _ := d.observed["hosts"].(map[string]any)
	entry, present := hosts[name].(map[string]any)
	return entry, present
}

// Report renders the desired and observed sets of every declared host, with
// the digest of each projection this document would produce for it.
//
// A host with no observation reports observed_models as nothing at all, which
// is a different document from a host whose observed set is empty.
func (d *Document) Report() (map[string]any, error) {
	hosts := map[string]any{}
	for _, name := range d.HostNames() {
		sets, err := d.Models(name)
		if err != nil {
			return nil, err
		}
		worker, err := d.WorkerBytes(name)
		if err != nil {
			return nil, err
		}
		profile, err := d.Resolved(name)
		if err != nil {
			return nil, err
		}
		entry := map[string]any{
			"profile":         d.Hosts[name].Profile,
			"cpu_class":       profile.CPUClass,
			"executor_slots":  profile.ExecutorSlots,
			"desired_models":  asAny(sets.Desired),
			"observed_models": nil,
			"worker_sha256":   Digest(worker),
			"client_sha256":   nil,
		}
		if d.NeedsClientProjection(name) {
			client, err := d.ClientBytes(name)
			if err != nil {
				return nil, err
			}
			entry["client_sha256"] = Digest(client)
		} else {
			entry["client"] = "not written; the coordinator administers itself locally"
		}
		if sets.HasObservation {
			entry["observed_models"] = asAny(sets.Observed)
		}
		hosts[name] = entry
	}
	catalog, err := d.CoordinatorBytes()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"coordinator_id":   d.Coordinator.ID,
		"coordinator_host": d.Coordinator.Host,
		"hosts":            hosts,
		// The digest is of UpKeeper's rendered coordinator configuration. It is not
		// the revision the coordinator fences an enrollment against, and it is
		// named so that it cannot be mistaken for one.
		"coordinator_projection": map[string]any{
			"purpose": CoordinatorPurpose,
			"sha256":  Digest(catalog),
		},
	}, nil
}

// EnrollmentPlan renders what an enrollment would change, and nothing else.
//
// UpKeeper may produce this plan. It may not enroll: applying it is a
// deliberate Steward admin operation with its own idempotency key and revision
// fence, because an accidental re-enrollment invalidates in-flight offers on a
// live fleet. The plan therefore always reports applied as false, and no code
// path in this repository turns it into a request.
//
// previous is the document this proposal replaces, or nil when there is none;
// against nil every authorization in the proposal is an addition, which is what
// a first enrollment is.
func (d *Document) EnrollmentPlan(previous *Document) (map[string]any, error) {
	changes, err := d.authorizationDiff(previous)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"applied":               false,
		"coordinator_id":        d.Coordinator.ID,
		"coordinator_host":      d.Coordinator.Host,
		"expected_revision":     d.observedCoordinator("revision"),
		"authorization_changes": asStrings(changes),
		"fence":                 FenceAdvice,
		"note":                  "proposal only; enrollment is an explicit Steward operation",
	}, nil
}

// observedCoordinator reads one field of what capture recorded about the
// coordinator. Only the revision is carried: a digest of the coordinator's
// catalog is not something this side can compute or check.
func (d *Document) observedCoordinator(key string) any {
	if d.observed == nil {
		return nil
	}
	coordinator, _ := d.observed["coordinator"].(map[string]any)
	value, present := coordinator[key]
	if !present {
		return nil
	}
	return value
}

// authorizationDiff lists the authorization-relevant differences between the
// previous document and this one, in a stable order. Only things that change
// what a worker may be offered or may run are listed: this is the diff a human
// has to read before enrolling, not a full document diff.
func (d *Document) authorizationDiff(previous *Document) ([]string, error) {
	changes := []string{}
	// refusable is set once any worker the coordinator already knows has its
	// catalog revision changed by this document, whether through its own
	// entry or through a project; the advice is then said once, at the end.
	refusable := false
	if previous != nil && previous.Coordinator.ID != d.Coordinator.ID {
		changes = append(changes,
			"coordinator identity "+previous.Coordinator.ID+" becomes "+d.Coordinator.ID)
	}
	for _, name := range d.HostNames() {
		proposed, err := d.Models(name)
		if err != nil {
			return nil, err
		}
		if previous == nil {
			changes = append(changes, "worker "+name+" is added")
			changes = append(changes, modelChanges(name, nil, proposed.Desired)...)
			continue
		}
		if _, known := previous.Hosts[name]; !known {
			changes = append(changes, "worker "+name+" is added")
			changes = append(changes, modelChanges(name, nil, proposed.Desired)...)
			continue
		}
		earlier, err := previous.Models(name)
		if err != nil {
			return nil, err
		}
		changes = append(changes, modelChanges(name, earlier.Desired, proposed.Desired)...)
		changes = append(changes, slotChanges(d, previous, name)...)
		changed, err := workerEntryChanged(d, previous, name)
		if err != nil {
			return nil, err
		}
		refusable = refusable || changed
	}
	if previous != nil {
		for _, name := range previous.HostNames() {
			if _, kept := d.Hosts[name]; !kept {
				changes = append(changes, "worker "+name+" is removed")
			}
		}
	}
	projectLines, touched := projectChanges(d, previous)
	changes = append(changes, projectLines...)
	if refusable || touched {
		changes = append(changes, ReloadRefusalAdvice)
	}
	return changes, nil
}

// workerEntryChanged reports whether a worker's entry in the coordinator
// projection differs between the two documents. The entry is the whole of
// what the coordinator hashes into that worker's catalog revision from this
// document, so this is the test for "the reload touches this worker" that
// the per-field diff lines above only partly cover (they do not list
// capabilities, cpu class or provider instances).
func workerEntryChanged(proposed, previous *Document, name string) (bool, error) {
	now, err := proposed.coordinatorWorkerEntry(name)
	if err != nil {
		return false, err
	}
	before, err := previous.coordinatorWorkerEntry(name)
	if err != nil {
		return false, err
	}
	return !reflect.DeepEqual(now, before), nil
}

func slotChanges(proposed, previous *Document, name string) []string {
	now, err := proposed.Resolved(name)
	if err != nil {
		return nil
	}
	before, err := previous.Resolved(name)
	if err != nil {
		return nil
	}
	changes := []string{}
	if before.ExecutorSlots != now.ExecutorSlots {
		changes = append(changes, "worker "+name+" executor slots "+
			strconv.Itoa(before.ExecutorSlots)+" becomes "+strconv.Itoa(now.ExecutorSlots))
	}
	for _, added := range difference(now.QuotaPools, before.QuotaPools) {
		changes = append(changes, "worker "+name+" joins quota pool "+added)
	}
	for _, dropped := range difference(before.QuotaPools, now.QuotaPools) {
		changes = append(changes, "worker "+name+" leaves quota pool "+dropped)
	}
	return changes
}

// projectChanges lists the project differences and reports whether any of
// them touched a worker the coordinator already knows, which is when the
// reload can be refused for retained work on that worker.
func projectChanges(proposed, previous *Document) ([]string, bool) {
	changes := []string{}
	// reenroll collects, once per worker, every worker whose catalog changes
	// because a project it is eligible for was added, removed or re-scoped.
	reenroll := map[string]bool{}
	for _, name := range proposed.ProjectNames() {
		project := proposed.Projects[name]
		var earlier Project
		known := false
		if previous != nil {
			earlier, known = previous.Projects[name]
		}
		if !known {
			changes = append(changes,
				"project "+name+" is added at "+project.Repository,
				"project "+name+" "+ProjectAddedAdvice)
			for _, worker := range project.EligibleWorkers {
				reenroll[worker] = true
			}
			continue
		}
		if earlier.Repository != project.Repository {
			changes = append(changes, "project "+name+" repository "+
				earlier.Repository+" becomes "+project.Repository)
		}
		if earlier.DefaultRef != project.DefaultRef {
			changes = append(changes, "project "+name+" default ref "+
				earlier.DefaultRef+" becomes "+project.DefaultRef)
		}
		for _, added := range difference(project.EligibleWorkers, earlier.EligibleWorkers) {
			changes = append(changes, "project "+name+" becomes eligible for "+added)
			reenroll[added] = true
		}
		for _, dropped := range difference(earlier.EligibleWorkers, project.EligibleWorkers) {
			changes = append(changes, "project "+name+" stops being eligible for "+dropped)
			reenroll[dropped] = true
		}
	}
	if previous != nil {
		for _, name := range previous.ProjectNames() {
			if _, kept := proposed.Projects[name]; !kept {
				changes = append(changes, "project "+name+" is removed")
				for _, worker := range previous.Projects[name].EligibleWorkers {
					reenroll[worker] = true
				}
			}
		}
	}
	workers := make([]string, 0, len(reenroll))
	for worker := range reenroll {
		workers = append(workers, worker)
	}
	sort.Strings(workers)
	for _, worker := range workers {
		changes = append(changes, reenrollAdvice(worker))
	}
	return changes, len(workers) != 0
}

func modelChanges(host string, before, now map[string][]string) []string {
	changes := []string{}
	instances := map[string]bool{}
	for instance := range before {
		instances[instance] = true
	}
	for instance := range now {
		instances[instance] = true
	}
	names := make([]string, 0, len(instances))
	for instance := range instances {
		names = append(names, instance)
	}
	sort.Strings(names)
	for _, instance := range names {
		for _, added := range difference(now[instance], before[instance]) {
			changes = append(changes,
				"worker "+host+" is authorized for "+instance+" model "+added)
		}
		for _, dropped := range difference(before[instance], now[instance]) {
			changes = append(changes,
				"worker "+host+" loses authorization for "+instance+" model "+dropped)
		}
	}
	return changes
}

// difference returns the sorted values of left that are not in right.
func difference(left, right []string) []string {
	present := make(map[string]bool, len(right))
	for _, value := range right {
		present[value] = true
	}
	missing := []string{}
	for _, value := range left {
		if !present[value] {
			missing = append(missing, value)
		}
	}
	sort.Strings(missing)
	return missing
}

func asAny(values map[string][]string) map[string]any {
	converted := map[string]any{}
	for key, value := range values {
		converted[key] = asStrings(value)
	}
	return converted
}

func asStrings(values []string) []any {
	converted := make([]any, 0, len(values))
	for _, value := range values {
		converted = append(converted, value)
	}
	return converted
}
