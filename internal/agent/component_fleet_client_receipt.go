package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
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
)

const fleetClientReceiptPath = fleet.StateDirectory + "/client-ownership.json"
const fleetClientReceiptLimit = 8 << 20

// Pending is durable before activation. A crash can leave either Active or
// Pending on disk; both are known states, neither permits unexplained content.
// Prior is the original ownership boundary, retained across subsequent applies.
// Restored receipts remain as tombstones so repeated rollback is idempotent.
type fleetClientReceipt struct {
	Version     int     `json:"version"`
	Destination string  `json:"destination"`
	PriorExists bool    `json:"prior_exists"`
	Prior       []byte  `json:"prior"`
	Active      string  `json:"active"`
	Pending     *string `json:"pending"`
	Restored    bool    `json:"restored"`
}

func fleetContentHash(body []byte) string {
	if body == nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Check each existing path component, including ancestors of missing suffixes.
// EvalSymlinks on the complete parent alone cannot prove containment when the
// parent has not been created yet. Owned paths never traverse symlinks.
func fleetOwnedPathSafe(home, path string) error {
	relative, err := filepath.Rel(home, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("fleet ownership path escapes home")
	}
	current := home
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("fleet ownership path traverses a symlink")
		}
	}
	return nil
}

func fleetReadPrivateBounded(c *Context, relative string, limit int64) ([]byte, error) {
	path := c.Path(relative)
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
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || int(owner.Uid) != os.Getuid() || info.Size() > limit {
		return nil, errors.New("fleet ownership file is not bounded private regular data")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("fleet ownership file exceeds size limit")
	}
	return body, nil
}

func fleetReadClientReceipt(c *Context) (*fleetClientReceipt, error) {
	body, err := fleetReadPrivateBounded(c, fleetClientReceiptPath, fleetClientReceiptLimit)
	if err != nil || body == nil {
		return nil, err
	}
	fields, err := canonjson.DecodeObject(body)
	if err != nil || !workerSameFields(fields, []string{"version", "destination", "prior_exists", "prior", "active", "pending", "restored"}) {
		return nil, errors.New("unsupported client ownership receipt fields")
	}
	if _, ok := fields["prior_exists"].(bool); !ok {
		return nil, errors.New("invalid prior existence in receipt")
	}
	if _, ok := fields["restored"].(bool); !ok {
		return nil, errors.New("invalid restored state in receipt")
	}
	if _, ok := fields["active"].(string); !ok {
		return nil, errors.New("invalid active digest in receipt")
	}
	var receipt fleetClientReceipt
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("ownership receipt must contain one document")
	}
	validHash := func(value string) bool {
		decoded, err := hex.DecodeString(value)
		return value == "" || (err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value)
	}
	if receipt.Version != 1 || receipt.Destination != fleet.ClientDestination ||
		len(receipt.Prior) > fleetCoordinatorMaxBytes || (!receipt.PriorExists && receipt.Prior != nil) || (receipt.PriorExists && receipt.Prior == nil) ||
		!validHash(receipt.Active) || (receipt.Pending != nil && !validHash(*receipt.Pending)) {
		return nil, errors.New("unsupported client ownership receipt")
	}
	if receipt.Restored && (receipt.Pending != nil || receipt.Active != fleetContentHash(receipt.Prior)) {
		return nil, errors.New("invalid restored client ownership receipt")
	}
	return &receipt, nil
}

func fleetSaveClientReceipt(c *Context, receipt *fleetClientReceipt) error {
	path := c.Path(fleetClientReceiptPath)
	if err := fleetOwnedPathSafe(c.Home, path); err != nil {
		return err
	}
	// Validate an existing destination before atomic replacement; never follow
	// or replace a symlink, public file or unknown receipt.
	if _, err := fleetReadClientReceipt(c); err != nil {
		return err
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if len(body) > fleetClientReceiptLimit {
		return errors.New("client ownership receipt exceeds size limit")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsatomic.WritePrivateFile(path, body)
}

func fleetReceiptMatches(receipt *fleetClientReceipt, current []byte) bool {
	hash := fleetContentHash(current)
	return hash == receipt.Active || (receipt.Pending != nil && hash == *receipt.Pending) ||
		hash == fleetContentHash(receipt.Prior) // a crash after restoration, before the tombstone
}

func fleetPrepareClientReceipt(c *Context, documents []*fleetDocument) (*fleetClientReceipt, error) {
	for _, planned := range documents {
		if planned.destination != fleet.ClientDestination || (!planned.changed && planned.remove) {
			continue
		}
		current, err := fleetReadPrivateBounded(c, fleet.ClientDestination, fleetCoordinatorMaxBytes)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(current, planned.before) || (current == nil) != (planned.before == nil) {
			return nil, errors.New("client changed since projection planning")
		}
		receipt, err := fleetReadClientReceipt(c)
		if err != nil {
			return nil, err
		}
		if receipt == nil {
			receipt = &fleetClientReceipt{Version: 1, Destination: fleet.ClientDestination,
				PriorExists: current != nil, Prior: current, Active: fleetContentHash(current)}
		} else if !fleetReceiptMatches(receipt, current) {
			return nil, errors.New("client differs from ownership receipt; preserve it for manual recovery")
		} else if !planned.changed && !receipt.Restored && receipt.Pending == nil && receipt.Active == fleetContentHash(current) {
			return nil, nil
		}
		next := fleetContentHash(planned.payload)
		if planned.remove {
			next = ""
		}
		receipt.Active = fleetContentHash(current)
		receipt.Pending = &next
		receipt.Restored = false
		if err := fleetSaveClientReceipt(c, receipt); err != nil {
			return nil, err
		}
		return receipt, nil
	}
	return nil, nil
}

func fleetCommitClientReceipt(c *Context, receipt *fleetClientReceipt) error {
	current, err := fleetReadPrivateBounded(c, fleet.ClientDestination, fleetCoordinatorMaxBytes)
	if err != nil {
		return err
	}
	if receipt.Pending == nil || fleetContentHash(current) != *receipt.Pending {
		return errors.New("activated client differs from pending ownership receipt")
	}
	receipt.Active = *receipt.Pending
	receipt.Pending = nil
	return fleetSaveClientReceipt(c, receipt)
}

func fleetRetireClient(c *Context, runID string, dryRun bool) (bool, error) {
	return fleetRetireClientWithRestore(c, runID, dryRun, func(home string, entry map[string]any, expected []byte) error {
		return fleetRestoreDocumentExpected(home, entry, expected, true)
	})
}

// restore is the filesystem boundary, also used for post-mutation fault tests.
// Expected live bytes are trusted runtime input, separate from retained data.
func fleetRetireClientWithRestore(c *Context, runID string, dryRun bool, restore func(string, map[string]any, []byte) error) (effected bool, failure error) {
	if err := fleetValidateReusedRetention(c, runID); err != nil {
		return false, err
	}
	current, err := fleetReadPrivateBounded(c, fleet.ClientDestination, fleetCoordinatorMaxBytes)
	if err != nil {
		return false, err
	}
	receipt, err := fleetReadClientReceipt(c)
	if err != nil {
		return false, err
	}
	if receipt == nil {
		if current != nil {
			return false, errors.New("client has no ownership receipt; preserve it for manual recovery")
		}
		return false, nil
	}
	if !fleetReceiptMatches(receipt, current) {
		return false, errors.New("client differs from ownership receipt; preserve it for manual recovery")
	}
	originalBytes := current
	changed := fleetContentHash(current) != fleetContentHash(receipt.Prior)
	if dryRun {
		return changed, nil
	}
	original := *receipt
	// A filesystem restore or receipt write may succeed before returning an error.
	// Keep the pre-retirement ownership state until every completion check passes.
	armed := false
	defer func() {
		if failure == nil || !armed {
			return
		}
		var recovery error
		if changed {
			directory, err := fleetOpenParent(c.Home, c.Path(fleet.ClientDestination))
			var current []byte
			if err == nil {
				current, err = fleetReadAt(directory, filepath.Base(c.Path(fleet.ClientDestination)))
				directory.Close()
				if err == nil && !fleetSameBytes(current, receipt.Prior) && !fleetSameBytes(current, originalBytes) {
					err = errors.New("client drifted during retirement; preserved")
				}
			}
			recovery = err
			if recovery == nil {
				rollback := &fleetDocument{destination: fleet.ClientDestination, target: c.Path(fleet.ClientDestination), before: current, payload: originalBytes, remove: originalBytes == nil, restorePolicy: true}
				recovery = fleetActivate(rollback)
			}
		}
		if recovery == nil {
			recovery = fleetSaveClientReceipt(c, &original)
		}
		if recovery != nil {
			failure = Failf("rollback failed: client retirement (%v); restoration (%v)", failure, recovery)
			effected = true
		} else {
			failure = RolledBackf("client retirement failed (%v); exact bytes and ownership restored", failure)
			effected = false
		}
	}()
	if changed {
		planned := &fleetDocument{destination: fleet.ClientDestination, target: c.Path(fleet.ClientDestination),
			payload: receipt.Prior, before: current, remove: !receipt.PriorExists, changed: true}
		if err := fleetRetainPrior(c, runID, []*fleetDocument{planned}); err != nil {
			return false, err
		}
		// The prior document may predate the current schema. Restore exact
		// retained bytes through the existing recovery path, not forward validation.
		entry := map[string]any{"destination": fleet.ClientDestination,
			"existed": receipt.PriorExists, "encoding": "base64/v1", "content": base64.StdEncoding.EncodeToString(receipt.Prior)}
		armed = true
		if err := restore(c.Home, entry, current); err != nil {
			return false, err
		}
		if err := fsatomic.SyncDirectory(filepath.Dir(planned.target)); err != nil {
			return false, err
		}
		written, err := fleetReadPrivateBounded(c, fleet.ClientDestination, fleetCoordinatorMaxBytes)
		if err != nil || fleetContentHash(written) != fleetContentHash(receipt.Prior) {
			return false, errors.New("restored client does not match retained prior bytes")
		}
	}
	armed = true
	receipt.Active = fleetContentHash(receipt.Prior)
	receipt.Pending = nil
	receipt.Restored = true
	return changed, fleetSaveClientReceipt(c, receipt)
}

func fleetRetireOwnedProjections(c *Context, runID string, dryRun bool) (map[string]any, error) {
	return fleetRetireOwnedProjectionsWithClient(c, runID, dryRun, fleetRetireClient)
}

func fleetRetireOwnedProjectionsWithClient(c *Context, runID string, dryRun bool, retireClient func(*Context, string, bool) (bool, error)) (map[string]any, error) {
	for _, retainedRun := range []string{runID, runID + "-policy", runID + "-client"} {
		if err := fleetValidateReusedRetention(c, retainedRun); err != nil {
			return nil, err
		}
	}
	if _, err := fleetRetirePolicy(c, runID+"-policy", true); err != nil {
		return nil, err
	}
	// Validate every retirement before any effect. A malformed coordinator
	// document must not cause a partial client restoration.
	if _, err := retireClient(c, runID+"-client", true); err != nil {
		return nil, err
	}
	if _, err := fleetRetireCoordinator(c, runID, true); err != nil {
		return nil, err
	}
	policy, err := fleetPlanPolicy(c, nil)
	if err != nil {
		return nil, err
	}
	if policy != nil && !dryRun {
		// Retire policy first so any later failure can restore its last-good bytes.
		if err := fleetWriteProjection(c, runID+"-policy", []*fleetDocument{policy}); err != nil {
			return nil, err
		}
		changed, err := retireClient(c, runID+"-client", false)
		if err != nil {
			// Client retirement already attempted transactional recovery. Replaying
			// its retained record here could overwrite the drift it refused.
			return nil, fleetRetirementPolicyFailure(c, runID, false, err)
		}
		result, err := fleetRetireCoordinator(c, runID, false)
		if err != nil {
			return nil, fleetRetirementPolicyFailure(c, runID, changed, err)
		}
		if changed || policy.changed {
			result["state"] = "updated"
		}
		return result, nil
	}
	changed, err := retireClient(c, runID+"-client", dryRun)
	if err != nil {
		return nil, err
	}
	result, err := fleetRetireCoordinator(c, runID, dryRun)
	if err != nil {
		return nil, err
	}
	policyChanged, err := fleetRetirePolicy(c, runID+"-policy", dryRun)
	if err == nil && (changed || policyChanged) {
		result["state"] = "updated"
	}
	return result, err
}
