package rollout

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// InvocationAuthority survives terminal InFlight clearing. The operation and its
// immutable attempt identify recovery; historical receipts and Failed never do.
type InvocationAuthority struct {
	Host                   string
	Operation              string
	Attempt                string
	ConfigDigest           string
	Started                time.Time
	EffectReturned         bool
	EnclosingAttempt       string
	EnclosingStarted       time.Time
	Failure                string
	RecoveryOperation      string
	RecoveryEffectReturned bool
	RecoveryAcknowledged   bool
	RecoveryHistory        []RecoveryInvocation
}

type RecoveryInvocation struct {
	Attempt        string
	Operation      string
	Started        time.Time
	EffectReturned bool
	Acknowledged   bool
}

func startRecoveryEvidence(a *InvocationAuthority, operation, attempt string, now time.Time) {
	a.RecoveryOperation = operation
	a.RecoveryEffectReturned = false
	a.RecoveryAcknowledged = false
	a.RecoveryHistory = append(a.RecoveryHistory, RecoveryInvocation{Operation: operation, Attempt: attempt, Started: now})
}
func recoveryEffectReturned(a *InvocationAuthority) {
	a.RecoveryEffectReturned = true
	a.RecoveryHistory[len(a.RecoveryHistory)-1].EffectReturned = true
}
func recoveryAcknowledged(a *InvocationAuthority) {
	a.RecoveryAcknowledged = true
	a.RecoveryHistory[len(a.RecoveryHistory)-1].Acknowledged = true
}

func saveInvocation(c Config, s *State, operation, attempt, host string, now time.Time) error {
	s.Invocation = &InvocationAuthority{Host: host, Operation: operation, Attempt: attempt, ConfigDigest: c.Digest(), Started: now}
	s.InFlight = operation
	return s.Save()
}

// saveEnclosingAuthority is the durable boundary before the enclosing invocation's
// unsynced final append. Child completion has cleared InFlight, but the exact
// enclosing intent remains the sole authority for named recovery if that append
// is lost. The reference never authorizes forward continuation.
func saveEnclosingAuthority(c Config, s *State, action string, now time.Time) error {
	if s.Invocation == nil || s.Failed != "" || s.FailedInvocation != nil || s.InFlight != "" {
		return errors.New("enclosing completion lacks final child authority")
	}
	intent, done, err := readAttempt(c, attemptPath(c, action), now)
	if err != nil || done || intent.Host != s.Invocation.Host {
		return errors.New("enclosing completion requires exact unfinished intent")
	}
	candidate := *s.Invocation
	candidate.EnclosingAttempt = action
	candidate.EnclosingStarted = intent.Started
	_, childDone, err := readAttempt(c, attemptPath(c, candidate.Attempt), now)
	if err != nil {
		return err
	}
	valid, err := unfinishedEnclosingAuthority(c, s, &candidate, childDone, now)
	if err != nil || !valid {
		return errors.Join(errors.New("enclosing completion lacks checked child evidence"), err)
	}
	s.Invocation = &candidate
	return s.Save()
}

func unfinishedEnclosingAuthority(c Config, s *State, a *InvocationAuthority, childDone bool, now time.Time) (bool, error) {
	if a.EnclosingAttempt == "" {
		if !a.EnclosingStarted.IsZero() {
			return false, errors.New("enclosing authority has conflicting start time")
		}
		return false, nil
	}
	host := a.Host
	expected := "run:" + host
	if a.Operation == "dropin:"+host || a.Operation == "enroll-worker:"+host {
		expected = "enroll:" + host
	} else if a.Operation != "observe:"+host {
		return false, errors.New("enclosing authority is not the final child phase")
	}
	intent, enclosingDone, err := readAttempt(c, attemptPath(c, expected), now)
	if err != nil || enclosingDone || intent.Host != host || a.EnclosingAttempt != expected ||
		a.EnclosingStarted.IsZero() || !a.EnclosingStarted.Equal(intent.Started) || a.Started.Before(intent.Started) ||
		!childDone || !a.EffectReturned || a.Attempt != "phase:"+a.Operation {
		return false, errors.New("missing or conflicting unfinished enclosing authority")
	}
	if expected == "run:"+host {
		var receipt ObservationReceipt
		duration := 15 * time.Minute
		if host == c.Canary {
			duration = time.Duration(c.SoakSeconds) * time.Second
		}
		if err := decodeDiagnostic(s.Extra["observation:"+host], &receipt); err != nil {
			return false, err
		}
		var pre map[string]string
		if err := decodeDiagnostic(s.Extra["host-pre:"+host], &pre); err != nil {
			return false, err
		}
		if !regexp.MustCompile("^[0-9a-f]{64}$").MatchString(pre["binary_sha256"]) ||
			strings.TrimSpace(pre["version"]) == "" || !(pre["arch"] == "x86_64" || pre["arch"] == "aarch64") ||
			pre["t3code.service.MainPID"] == "" || pre["t3code.service.ExecMainStartTimestamp"] == "" {
			return false, errors.New("unfinished enclosing run lacks original binary and process snapshot")
		}
		for _, unit := range []string{"t3-steward.service", "t3-steward-worker.service"} {
			if pre[unit+".ActiveState"] != "active" || pre[unit+".MainPID"] == "" ||
				pre[unit+".MainPID"] == "0" || pre[unit+".ExecMainStartTimestamp"] == "" {
				return false, errors.New("unfinished enclosing run lacks original service generation")
			}
		}
		var observed, post identity
		if err := decodeDiagnostic(receipt.Identity, &observed); err != nil {
			return false, err
		}
		if err := decodeDiagnostic(s.Extra["identity-post:"+host], &post); err != nil {
			return false, err
		}
		if !s.Completed["host:"+host] || len(s.Extra["host-pre:"+host]) == 0 ||
			receipt.Host != host || receipt.ConfigDigest != c.Digest() || receipt.ReleaseCommit != s.ReleaseCommit ||
			!receipt.Started.Equal(a.Started) || receipt.Finished.After(now) ||
			receipt.Finished.Sub(receipt.Started) < duration || receipt.Samples < 1+int(duration/(60*time.Second)) ||
			len(receipt.Identity) == 0 || observed != post || observed.Health != "healthy" || observed.Epoch == json.Number("") {
			return false, errors.New("unfinished enclosing run lacks final observation evidence")
		}
		return true, nil
	}
	if !s.Completed["enroll-worker:"+host] {
		return false, errors.New("unfinished enclosing enrollment lacks completed catalog phase")
	}
	if err := requireAttempt(c, "phase:enroll-worker:"+host, now); err != nil {
		return false, err
	}
	if host != "agent-a" && host != "agent-b" {
		if a.Operation != "enroll-worker:"+host {
			return false, errors.New("enclosing enrollment final phase mismatch")
		}
		return true, nil
	}
	if a.Operation != "dropin:"+host || !s.Completed["dropin:"+host] {
		return false, errors.New("unfinished enclosing enrollment lacks final drop-in phase")
	}
	var receipt dropinReceipt
	if err := decodeDiagnostic(s.Extra["dropin:"+host], &receipt); err != nil {
		return false, err
	}
	restoredRecovery := receipt.Restored && s.FailedInvocation != nil && len(a.RecoveryHistory) > 0
	if receipt.Host != host || !(receipt.Done && !receipt.Restored || restoredRecovery) ||
		receipt.IntentAt.IsZero() || receipt.IntentAt.Before(a.Started) || receipt.IntentAt.After(now) {
		return false, errors.New("unfinished enclosing enrollment lacks original drop-in receipt")
	}
	if receipt.Absent {
		if !contains(c.DropinAbsentHosts, host) || receipt.SHA256 != "" || receipt.Mode != 0 ||
			receipt.UID != 0 || receipt.GID != 0 || receipt.Restored {
			return false, errors.New("unfinished enclosing enrollment has unreviewed drop-in absence")
		}
	} else if !regexp.MustCompile("^[0-9a-f]{64}$").MatchString(receipt.SHA256) ||
		receipt.Mode <= 0 || receipt.Mode > 0777 || receipt.UID < 0 || receipt.GID < 0 {
		return false, errors.New("unfinished enclosing enrollment lacks original drop-in metadata")
	}
	return true, nil
}

func operationHost(c Config, action string) string {
	for _, prefix := range []string{"host:", "observe:", "dropin:", "enroll-worker:", "dropin-restore:"} {
		if strings.HasPrefix(action, prefix) {
			return strings.TrimPrefix(action, prefix)
		}
	}
	if action == "config-patch" || action == "config-restore" || action == "backup" {
		return c.CoordinatorHost
	}
	return ""
}

func failInvocation(s *State) {
	if s.FailedInvocation == nil && s.Invocation != nil {
		copied := *s.Invocation
		copied.Failure = s.Failed
		s.FailedInvocation = &copied
	}
}

func recoveryAuthority(c Config, s *State, host string, now time.Time) (*InvocationAuthority, error) {
	if s.FailedInvocation != nil && s.Failed == "" {
		return nil, errors.New("failed invocation lacks permanent terminal failure")
	}
	a := s.FailedInvocation
	if a == nil {
		a = s.Invocation
	}
	if a == nil || a.Host != host || a.ConfigDigest != c.Digest() || a.Operation == "" || a.Attempt == "" || a.Started.IsZero() || a.Started.After(now) || operationHost(c, a.Operation) != host {
		return nil, errors.New("recovery requires exact durable failed invocation host and operation")
	}
	intent, done, err := readAttempt(c, attemptPath(c, a.Attempt), now)
	if err != nil || intent.Action != a.Attempt || !intent.Started.Equal(a.Started) {
		return nil, fmt.Errorf("recovery invocation intent missing or conflicting: %v", err)
	}
	if a.Operation != "host:"+host && a.Operation != "observe:"+host && a.Operation != "dropin:"+host && a.Operation != "enroll-worker:"+host && a.Operation != "config-patch" && a.Operation != "backup" {
		return nil, errors.New("unsupported failed operation recovery authority")
	}
	if a.Attempt == "run:"+host && a.Operation != "host:"+host || a.Attempt == "enroll:"+host && a.Operation != "dropin:"+host && a.Operation != "enroll-worker:"+host {
		return nil, errors.New("operation conflicts with immutable invocation kind")
	}
	if a.Attempt != "run:"+host && a.Attempt != "enroll:"+host && a.Attempt != "phase:"+a.Operation {
		return nil, errors.New("recovery operation differs from invocation authority")
	}
	if a.Attempt == "run:"+host || a.Attempt == "enroll:"+host {
		if intent.Host != host {
			return nil, errors.New("recovery intent host mismatch")
		}
	}
	if a.Operation == "observe:"+host || a.Operation == "config-patch" || a.Operation == "backup" {
		_, runDone, runErr := readAttempt(c, attemptPath(c, "run:"+host), now)
		if runErr != nil || runDone {
			return nil, errors.New("missing or completed enclosing invocation")
		}
	}
	if strings.HasPrefix(a.Operation, "dropin:") || strings.HasPrefix(a.Operation, "enroll-worker:") {
		_, enrolled, enrollErr := readAttempt(c, attemptPath(c, "enroll:"+host), now)
		if enrollErr != nil || enrolled {
			return nil, errors.New("missing or completed enclosing enrollment")
		}
	}
	finalAckLost, err := unfinishedEnclosingAuthority(c, s, a, done, now)
	if err != nil {
		return nil, err
	}
	if done && !finalAckLost {
		return nil, errors.New("completed invocation does not authorize recovery")
	}
	if len(a.RecoveryHistory) == 0 && (a.RecoveryOperation != "" || a.RecoveryEffectReturned || a.RecoveryAcknowledged) {
		return nil, errors.New("missing permanent recovery history")
	}
	if len(a.RecoveryHistory) > 0 {
		last := a.RecoveryHistory[len(a.RecoveryHistory)-1]
		if last.Operation != a.RecoveryOperation || last.EffectReturned != a.RecoveryEffectReturned || last.Acknowledged != a.RecoveryAcknowledged {
			return nil, errors.New("conflicting latest recovery evidence")
		}
	}
	for _, recovered := range a.RecoveryHistory {
		kind := "rollback"
		if recovered.Operation == "dropin-restore:"+host {
			kind = "dropin"
		}
		if !strings.HasPrefix(recovered.Attempt, "recovery:"+kind+":"+host+":") {
			return nil, errors.New("recovery operation conflicts with immutable intent kind")
		}
		// A lost final append is uncertainty, never forward authority. Named
		// recovery can retry the same failed invocation, preserving the old
		// intent even when its acknowledgement is absent. Corrupt bytes refuse.
		intent, _, err := readAttempt(c, attemptPath(c, recovered.Attempt), now)
		if err != nil || intent.Host != host || recovered.Started.IsZero() || !recovered.Started.Equal(intent.Started) || recovered.Started.After(now) || recovered.Acknowledged && !recovered.EffectReturned ||
			!(recovered.Operation == "rollback:"+host || recovered.Operation == "dropin-restore:"+host || host == c.CoordinatorHost && recovered.Operation == "config-restore") {
			return nil, errors.New("recovery history has missing or conflicting durable intent/effect evidence")
		}
	}
	if s.FailedInvocation == nil && s.InFlight != a.Operation && !(s.InFlight == "" && finalAckLost) {
		return nil, errors.New("uncertain invocation lost matching dispatch fence")
	}
	return a, nil
}
