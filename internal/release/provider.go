package release

import (
	"sort"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

// ProviderSpec is one provider registration as `upkeeper provider add`
// authors it: the instance, the one quota pool its work is charged to, the
// models it is authorized for, and the hosts it is registered on.
type ProviderSpec struct {
	Instance  string
	QuotaPool string
	// Models is the desired allowlist. Empty means "*" for an instance the
	// targets do not run yet, and "keep what is authored" for one they do.
	Models []string
	Hosts  []string
}

// ProviderEdit says where a provider edit landed: the profiles it changed
// because every host of that profile was named, and the hosts whose
// exceptions it changed because only some hosts of their profile were named
// (or because an exception of theirs already overrode a provider key and
// would otherwise have masked the profile change).
type ProviderEdit struct {
	Profiles []string
	Hosts    []string
}

// providerKeys are the profile keys a provider registration touches, together
// and always together: an instance without its models is dead authorization,
// a binding without its pool is one the coordinator cannot honor.
var providerKeys = []string{"provider_instances", "models", "quota_pools", fleet.QuotaBindingsField}

// AddProvider registers one provider instance on the named hosts of the
// checkout's release and writes the manifest back, the way AddProject does:
// the release is loaded and validated, the edited component is parsed by
// fleet.Parse before a byte is written, and the bytes are the canonical
// encoding. Nothing is committed, pushed or pulled.
//
// The instance joins provider_instances, its models join models, the pool
// joins quota_pools and the instance is bound to the pool in quota_bindings,
// in one edit, so that the coordinator's projection carries the binding it
// needs without a second file on a second host. An instance the targets
// already run is bound and keeps its authored models unless Models names
// new ones; an instance already bound elsewhere is refused, because one
// instance draws from one pool.
func AddProvider(root string, spec ProviderSpec) (Release, ProviderEdit, error) {
	if !fleet.IsIdentifier(spec.Instance) {
		return Release{}, ProviderEdit{}, failure.Validation(
			"provider instance %q is not a valid instance name", spec.Instance)
	}
	if !fleet.IsIdentifier(spec.QuotaPool) {
		return Release{}, ProviderEdit{}, failure.Validation(
			"quota pool %q is not a valid pool name", spec.QuotaPool)
	}
	models := sortedUniqueStrings(spec.Models)
	for _, model := range models {
		if !fleet.IsModel(model) || model == "*" && len(models) != 1 {
			return Release{}, ProviderEdit{}, failure.Validation(
				"model %q is not a valid model identifier", model)
		}
	}
	value, document, err := loadFleetForEdit(root)
	if err != nil {
		return Release{}, ProviderEdit{}, err
	}
	if len(spec.Hosts) == 0 {
		return Release{}, ProviderEdit{}, failure.Validation(
			"provider %s needs at least one host to be registered on", spec.Instance)
	}
	if err := checkHosts(document, spec.Hosts); err != nil {
		return Release{}, ProviderEdit{}, err
	}
	// The binding conflict is refused per host before anything is edited,
	// against the host's resolved intent, so that a profile binding and an
	// exception binding are both seen.
	for _, host := range sortedUniqueStrings(spec.Hosts) {
		resolved, err := document.Resolved(host)
		if err != nil {
			return Release{}, ProviderEdit{}, err
		}
		if bound, present := resolved.QuotaBindings[spec.Instance]; present {
			if bound == spec.QuotaPool {
				return Release{}, ProviderEdit{}, failure.Validation(
					"provider %s is already bound to quota pool %s on %s", spec.Instance, bound, host)
			}
			return Release{}, ProviderEdit{}, failure.Validation(
				"provider %s is bound to quota pool %s on %s; binding it to %s would bind one "+
					"instance to two pools; remove it there first",
				spec.Instance, bound, host, spec.QuotaPool)
		}
	}
	edit := applyProviderEdit(value, document, spec.Hosts, func(body map[string]any) bool {
		instances := addString(stringsOf(body["provider_instances"]), spec.Instance)
		body["provider_instances"] = anyList(instances)
		authored, _ := body["models"].(map[string]any)
		allowlist := copyObject(authored)
		if len(models) > 0 {
			allowlist[spec.Instance] = anyList(models)
		} else if _, present := allowlist[spec.Instance]; !present {
			allowlist[spec.Instance] = anyList([]string{"*"})
		}
		body["models"] = allowlist
		body["quota_pools"] = anyList(addString(stringsOf(body["quota_pools"]), spec.QuotaPool))
		bindings, _ := body[fleet.QuotaBindingsField].(map[string]any)
		bound := copyObject(bindings)
		bound[spec.Instance] = spec.QuotaPool
		body[fleet.QuotaBindingsField] = bound
		return true
	})
	written, err := writeEditedFleet(root, value)
	if err != nil {
		return Release{}, ProviderEdit{}, err
	}
	return written, edit, nil
}

// RemoveProvider takes one provider instance, its models and its binding out
// of the named hosts, or out of every profile and host exception when no
// host is named, and writes the manifest back. Quota pools are membership
// rather than a binding and are left as authored. An instance nothing runs is
// refused by name and nothing is written.
func RemoveProvider(root, instance string, hosts []string) (Release, ProviderEdit, error) {
	value, document, err := loadFleetForEdit(root)
	if err != nil {
		return Release{}, ProviderEdit{}, err
	}
	if err := checkHosts(document, hosts); err != nil {
		return Release{}, ProviderEdit{}, err
	}
	targets := hosts
	if len(targets) == 0 {
		targets = document.HostNames()
	}
	for _, host := range sortedUniqueStrings(targets) {
		resolved, err := document.Resolved(host)
		if err != nil {
			return Release{}, ProviderEdit{}, err
		}
		if contains(resolved.ProviderInstances, instance) {
			continue
		}
		if len(hosts) == 0 {
			// Nothing is named, so the instance only has to exist somewhere.
			continue
		}
		return Release{}, ProviderEdit{}, failure.Validation(
			"provider %s is not registered on %s; it runs %s",
			instance, host, strings.Join(resolved.ProviderInstances, ", "))
	}
	edit := applyProviderEdit(value, document, targets, func(body map[string]any) bool {
		instances := stringsOf(body["provider_instances"])
		if !contains(instances, instance) {
			return false
		}
		body["provider_instances"] = anyList(removeString(instances, instance))
		authored, _ := body["models"].(map[string]any)
		allowlist := copyObject(authored)
		delete(allowlist, instance)
		body["models"] = allowlist
		if bindings, present := body[fleet.QuotaBindingsField].(map[string]any); present {
			bound := copyObject(bindings)
			delete(bound, instance)
			if len(bound) == 0 {
				delete(body, fleet.QuotaBindingsField)
			} else {
				body[fleet.QuotaBindingsField] = bound
			}
		}
		return true
	})
	if len(edit.Profiles) == 0 && len(edit.Hosts) == 0 {
		return Release{}, ProviderEdit{}, failure.Validation(
			"provider %s is not registered on any profile or host of %s", instance, File)
	}
	written, err := writeEditedFleet(root, value)
	if err != nil {
		return Release{}, ProviderEdit{}, err
	}
	return written, edit, nil
}

// checkHosts refuses an unknown host by name before the parser sees the
// document, because the parser's refusal is written for a document and this
// one is written for an operator who typed a host name.
func checkHosts(document *fleet.Document, hosts []string) error {
	for _, host := range hosts {
		if _, known := document.Hosts[host]; !known {
			return failure.Validation(
				"host %s is not a fleet host; the fleet intent declares %s",
				host, strings.Join(document.HostNames(), ", "))
		}
	}
	return nil
}

// applyProviderEdit runs one mutation over the raw bodies the named hosts
// resolve through, and reports where it landed.
//
// A profile is edited when every host of it is named, because then the edit
// is a statement about the class. Otherwise each named host gets the edit in
// its exceptions, built from the merged view of its profile and its existing
// exceptions, so the exception is a complete override of the provider keys
// and the host reads the same values whether or not the profile changes
// later. A host whose exceptions already override a provider key is edited
// even when its profile is, because the exception would mask the profile.
//
// The mutation reports whether it changed the body; a body it left alone is
// not reported as edited.
func applyProviderEdit(
	value Release, document *fleet.Document, hosts []string, mutate func(body map[string]any) bool,
) ProviderEdit {
	component := value.Component(fleet.Component)
	profiles, _ := component["profiles"].(map[string]any)
	rawHosts, _ := component["hosts"].(map[string]any)
	named := map[string]bool{}
	for _, host := range hosts {
		named[host] = true
	}
	byProfile := map[string][]string{}
	for _, name := range document.HostNames() {
		profile := document.Hosts[name].Profile
		byProfile[profile] = append(byProfile[profile], name)
	}
	edit := ProviderEdit{Profiles: []string{}, Hosts: []string{}}
	for _, profile := range document.ProfileNames() {
		members := byProfile[profile]
		if len(members) == 0 {
			continue
		}
		whole := true
		for _, host := range members {
			whole = whole && named[host]
		}
		body, _ := profiles[profile].(map[string]any)
		if whole && mutate(body) {
			edit.Profiles = append(edit.Profiles, profile)
		}
		for _, host := range members {
			if !named[host] {
				continue
			}
			entry, _ := rawHosts[host].(map[string]any)
			exceptions, _ := entry["exceptions"].(map[string]any)
			if whole && !overridesProviderKey(exceptions) {
				continue
			}
			merged := map[string]any{}
			for key, item := range body {
				merged[key] = item
			}
			for key, item := range exceptions {
				merged[key] = item
			}
			if !mutate(merged) {
				continue
			}
			if exceptions == nil {
				exceptions = map[string]any{}
				entry["exceptions"] = exceptions
			}
			for _, key := range providerKeys {
				if item, present := merged[key]; present {
					exceptions[key] = item
				} else {
					delete(exceptions, key)
				}
			}
			if len(exceptions) == 0 {
				delete(entry, "exceptions")
			}
			edit.Hosts = append(edit.Hosts, host)
		}
	}
	sort.Strings(edit.Profiles)
	sort.Strings(edit.Hosts)
	return edit
}

func overridesProviderKey(exceptions map[string]any) bool {
	for _, key := range providerKeys {
		if _, present := exceptions[key]; present {
			return true
		}
	}
	return false
}

func stringsOf(value any) []string {
	items, _ := value.([]any)
	values := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			values = append(values, text)
		}
	}
	return values
}

func anyList(values []string) []any {
	converted := make([]any, 0, len(values))
	for _, value := range values {
		converted = append(converted, value)
	}
	return converted
}

func addString(values []string, wanted string) []string {
	return sortedUniqueStrings(append(append([]string{}, values...), wanted))
}

func removeString(values []string, unwanted string) []string {
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if value != unwanted {
			kept = append(kept, value)
		}
	}
	return sortedUniqueStrings(kept)
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// copyObject copies one level of an object, so that a host's exception can
// be edited without aliasing the profile's own map.
func copyObject(object map[string]any) map[string]any {
	copied := make(map[string]any, len(object))
	for key, value := range object {
		copied[key] = value
	}
	return copied
}
