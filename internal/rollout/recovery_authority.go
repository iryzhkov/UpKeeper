package rollout

import (
	"errors"
	"fmt"
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
	if done {
		// An observed phase may be acknowledged while the enclosing run lost
		// its final acknowledgement. The unfinished exact run is still required.
		if a.Operation != "observe:"+host {
			return nil, errors.New("completed invocation does not authorize recovery")
		}
		_, runDone, runErr := readAttempt(c, attemptPath(c, "run:"+host), now)
		if runErr != nil || runDone {
			return nil, errors.New("completed host does not authorize recovery")
		}
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
	if s.FailedInvocation == nil && s.InFlight != a.Operation {
		return nil, errors.New("uncertain invocation lost matching dispatch fence")
	}
	return a, nil
}
