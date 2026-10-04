package agent

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

const fleetPolicyReceiptPath = fleet.StateDirectory + "/policy-ownership.json"

// Policy uses the same durable Active/Pending/Prior ownership boundary as the
// client, with a distinct destination and receipt. No receipt means unowned:
// legacy releases must not inspect, claim or remove that file.
func fleetReadPolicyReceipt(c *Context) (*fleetClientReceipt, error) {
	body, err := fleetReadPrivateBounded(c, fleetPolicyReceiptPath, fleetClientReceiptLimit)
	if err != nil || body == nil {
		return nil, err
	}
	var r fleetClientReceipt
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("policy receipt must contain one document")
	}
	// Reject duplicate keys before decoding can erase their evidence.
	fields, err := canonjson.DecodeObject(body)
	if err != nil || !workerSameFields(fields, []string{"version", "destination", "prior_exists", "prior", "active", "pending", "restored"}) {
		return nil, errors.New("unsupported policy ownership receipt fields")
	}
	if _, ok := fields["prior_exists"].(bool); !ok {
		return nil, errors.New("invalid policy receipt prior existence")
	}
	if _, ok := fields["active"].(string); !ok {
		return nil, errors.New("invalid policy receipt active hash")
	}
	if _, ok := fields["restored"].(bool); !ok {
		return nil, errors.New("invalid policy receipt restored state")
	}
	if r.Version != 1 || r.Destination != fleet.RoutePolicyDestination ||
		len(r.Prior) > fleet.RoutePolicyMaxBytes || r.PriorExists != (r.Prior != nil) ||
		!fleetPolicyHashValid(r.Active) || (r.Pending != nil && !fleetPolicyHashValid(*r.Pending)) ||
		(r.Restored && (r.Pending != nil || r.Active != fleetContentHash(r.Prior))) {
		return nil, errors.New("unsupported policy ownership receipt")
	}
	return &r, nil
}

func fleetReadPolicy(c *Context) ([]byte, error) {
	path := c.Path(fleet.RoutePolicyDestination)
	if err := fleetOwnedPathSafe(c.Home, path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > fleet.RoutePolicyMaxBytes {
		return nil, errors.New("policy must be a bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, fleet.RoutePolicyMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > fleet.RoutePolicyMaxBytes {
		return nil, errors.New("policy exceeds 1 MiB")
	}
	return body, nil
}

func fleetSavePolicyReceipt(c *Context, r *fleetClientReceipt) error {
	path := c.Path(fleetPolicyReceiptPath)
	if err := fleetOwnedPathSafe(c.Home, path); err != nil {
		return err
	}
	if _, err := fleetReadPolicyReceipt(c); err != nil {
		return err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return fsatomic.WritePrivateFile(path, body)
}

func fleetPolicyRunner(c *Context) procx.Runner {
	return func(command procx.Command) procx.Result {
		command.Env = c.Env
		runner := c.Runner
		if runner == nil {
			runner = procx.Run
		}
		return runner(command)
	}
}

func fleetPlanPolicy(c *Context, payload []byte) (*fleetDocument, error) {
	receipt, err := fleetReadPolicyReceipt(c)
	if err != nil {
		return nil, err
	}
	if receipt != nil && receipt.Restored {
		// A tombstone records relinquished ownership, not authority over later edits.
		if payload == nil {
			return nil, nil
		}
		receipt = nil
	}
	if payload == nil && receipt == nil {
		return nil, nil
	}
	current, err := fleetReadPolicy(c)
	if err != nil {
		return nil, err
	}
	if receipt != nil && !fleetReceiptMatches(receipt, current) {
		return nil, errors.New("policy differs from ownership receipt; preserve it for manual recovery")
	}
	planned := &fleetDocument{destination: fleet.RoutePolicyDestination,
		target: c.Path(fleet.RoutePolicyDestination), before: current, payload: payload}
	if payload == nil {
		// Retirement restores the original boundary; these bytes may predate the schema.
		planned.payload = receipt.Prior
		planned.remove = !receipt.PriorExists
		planned.restorePolicy = true
	} else {
		// Preflight configured authorization before retention, ownership or live writes.
		if err := fleet.CheckRoutePolicy(payload, fleetPolicyRunner(c), true); err != nil {
			return nil, err
		}
		planned.validatePolicy = func(path string) error {
			return fleet.CheckRoutePolicyFile(path, fleetPolicyRunner(c), true)
		}
	}
	planned.changed = !bytes.Equal(current, planned.payload) || (current == nil) != (planned.payload == nil)
	if payload != nil && current != nil {
		info, err := os.Stat(planned.target)
		if err != nil {
			return nil, err
		}
		planned.changed = planned.changed || info.Mode().Perm() != 0o644
	}
	if receipt == nil {
		receipt = &fleetClientReceipt{Version: 1, Destination: fleet.RoutePolicyDestination,
			PriorExists: current != nil, Prior: current, Active: fleetContentHash(current)}
	}
	planned.policyReceipt = receipt
	return planned, nil
}

func fleetPreparePolicyReceipt(c *Context, documents []*fleetDocument) error {
	for _, planned := range documents {
		if planned.policyReceipt == nil {
			continue
		}
		current, err := fleetReadPolicy(c)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, planned.before) || (current == nil) != (planned.before == nil) {
			return errors.New("policy changed since projection planning")
		}
		r := planned.policyReceipt
		next := fleetContentHash(planned.payload)
		r.Active, r.Pending, r.Restored = fleetContentHash(current), &next, false
		if err := fleetSavePolicyReceipt(c, r); err != nil {
			return err
		}
	}
	return nil
}

func fleetCommitPolicyReceipt(c *Context, documents []*fleetDocument) error {
	for _, planned := range documents {
		if planned.policyReceipt == nil {
			continue
		}
		current, err := fleetReadPolicy(c)
		if err != nil {
			return err
		}
		r := planned.policyReceipt
		if r.Pending == nil || fleetContentHash(current) != *r.Pending {
			return errors.New("activated policy differs from pending ownership receipt")
		}
		r.Active, r.Pending, r.Restored = *r.Pending, nil, planned.restorePolicy
		if err := fleetSavePolicyReceipt(c, r); err != nil {
			return err
		}
	}
	return nil
}

func fleetRetirePolicy(c *Context, runID string, dryRun bool) (bool, error) {
	planned, err := fleetPlanPolicy(c, nil)
	if err != nil || planned == nil {
		return false, err
	}
	if !dryRun {
		if err := fleetWriteProjection(c, runID, []*fleetDocument{planned}); err != nil {
			return false, err
		}
	}
	return planned.changed, nil
}

// Reconcile after an exact retained-byte restoration, including manual rollback.
func fleetReconcilePolicyReceipt(c *Context) error {
	r, err := fleetReadPolicyReceipt(c)
	if err != nil || r == nil {
		return err
	}
	current, err := fleetReadPolicy(c)
	if err != nil {
		return err
	}
	r.Active, r.Pending = fleetContentHash(current), nil
	r.Restored = r.Active == fleetContentHash(r.Prior)
	return fleetSavePolicyReceipt(c, r)
}

func fleetProjectionCommitFailure(c *Context, runID string, documents []*fleetDocument, cause error) error {
	ownsPolicy := false
	for _, planned := range documents {
		ownsPolicy = ownsPolicy || planned.policyReceipt != nil
	}
	if !ownsPolicy {
		return cause
	} // Preserve the legacy client-only failure path.
	if err := fleetRestoreEffects(c, documents); err != nil {
		return Failf("rollback failed: ownership commit failed (%v); retained restoration failed (%v)", cause, err)
	}
	if err := fleetReconcileClientAfterPolicyRollback(c); err != nil {
		return Failf("rollback failed: ownership commit failed (%v); client receipt could not be reconciled (%v)", cause, err)
	}
	return RolledBackf("ownership commit failed (%v); restored retained projections", cause)
}

// A policy receipt commit may fail after the client receipt was committed.
// The byte rollback must put that receipt back in agreement as well.
func fleetReconcileClientAfterPolicyRollback(c *Context) error {
	r, err := fleetReadClientReceipt(c)
	if err != nil || r == nil {
		return err
	}
	current, err := fleetReadPrivateBounded(c, fleet.ClientDestination, fleetCoordinatorMaxBytes)
	if err != nil {
		return err
	}
	r.Active, r.Pending = fleetContentHash(current), nil
	r.Restored = r.Active == fleetContentHash(r.Prior)
	return fleetSaveClientReceipt(c, r)
}

func fleetRestoreRetiredProjection(c *Context, runID string, policy bool) error {
	destination := fleet.ClientDestination
	receipt, err := fleetReadClientReceipt(c)
	if policy {
		destination = fleet.RoutePolicyDestination
		receipt, err = fleetReadPolicyReceipt(c)
	}
	if err != nil {
		return err
	}
	if receipt == nil {
		return errors.New("retired projection has no receipt")
	}
	if _, err := fleetRestoreRetainedExpected(c.Home, runID, destination, receipt.Prior, true); err != nil {
		return err
	}
	if policy {
		return fleetReconcilePolicyReceipt(c)
	}
	return fleetReconcileClientAfterPolicyRollback(c)
}

func fleetRetirementPolicyFailure(c *Context, runID string, clientChanged bool, cause error) error {
	var failures []error
	if err := fleetRestoreRetiredProjection(c, runID+"-policy", true); err != nil {
		failures = append(failures, err)
	}
	if clientChanged {
		if err := fleetRestoreRetiredProjection(c, runID+"-client", false); err != nil {
			failures = append(failures, err)
		} else if err := fleetReconcileClientAfterPolicyRollback(c); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 || strings.Contains(cause.Error(), "rollback failed:") {
		return Failf("rollback failed: retirement (%v); recovery (%v); affected projections may be in a mixed state", cause, errors.Join(failures...))
	}
	return RolledBackf("retirement failed (%v); restored prior policy and client", cause)
}

func fleetPolicyResult(result map[string]any, planned *fleetDocument) map[string]any {
	if planned != nil {
		result["route_policy_projection"] = map[string]any{
			"destination": fleet.RoutePolicyDestination,
			"sha256":      fleetContentHash(planned.payload),
			"retired":     planned.restorePolicy,
		}
	}
	return result
}

func fleetPolicyHashValid(value string) bool {
	decoded, err := hex.DecodeString(value)
	return value == "" || (err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value)
}
