package release

import "sort"

// ReferencedHosts lists, sorted, every inventory host a release names: the
// Steward fleet's hosts, clients and coordinator, the worker configuration's
// workers and the hosts Jocasta is configured for. A release that names a
// host the inventory does not know cannot be converged, and is inconsistent
// in the repository that publishes it.
func ReferencedHosts(r Release) ([]string, error) {
	names := map[string]bool{}
	document, present, err := r.FleetIntent()
	if err != nil {
		return nil, err
	}
	if present {
		for _, name := range document.HostNames() {
			names[name] = true
		}
		for _, name := range document.ClientNames() {
			names[name] = true
		}
		if document.Coordinator.Host != "" {
			names[document.Coordinator.Host] = true
		}
	}
	for _, component := range []string{"worker-configuration", JocastaComponent} {
		entry, _ := r.Components()[component].(map[string]any)
		hosts, _ := entry["hosts"].(map[string]any)
		for name := range hosts {
			names[name] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return sorted, nil
}
