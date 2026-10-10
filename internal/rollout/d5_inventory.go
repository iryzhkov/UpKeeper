package rollout

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// The workflow inventory comes from the frozen Config, never from surviving
// journal keys. Named enrollment has one reviewed order.
var d5Order = []string{"agent-b", "agent-a", "omarchy-pc"}

func priorD5(c Config, s *State, host string, now time.Time) error {
	if c.Canary != "agent-b" || c.CoordinatorHost != "normandy" {
		return nil
	}
	if !contains(c.ActiveWorkers, host) || !contains(d5Order, host) {
		return errors.New("worker absent from declared D5 workflow")
	}
	var prior []string
	for _, h := range d5Order {
		if h == host {
			return requiredD5(c, s, prior, now)
		}
		if contains(c.ActiveWorkers, h) {
			prior = append(prior, h)
		}
	}
	return errors.New("unknown D5 host")
}

func requiredD5(c Config, s *State, hosts []string, now time.Time) error {
	if c.Canary != "agent-b" || c.CoordinatorHost != "normandy" {
		return nil
	}
	for _, h := range hosts {
		if !contains(c.ActiveWorkers, h) || !contains(d5Order, h) {
			return fmt.Errorf("undeclared D5 host %s", h)
		}
		if !s.Completed["enroll-worker:"+h] {
			return fmt.Errorf("mandatory D5 enrollment incomplete: %s", h)
		}
		for _, action := range []string{"phase:enroll-worker:" + h, "enroll:" + h} {
			if err := requireAttempt(c, action, now); err != nil {
				return err
			}
		}
		if h != "agent-b" && h != "agent-a" {
			continue
		}
		if !s.Completed["dropin:"+h] {
			return fmt.Errorf("mandatory D5 drop-in phase incomplete: %s", h)
		}
		if err := requireAttempt(c, "phase:dropin:"+h, now); err != nil {
			return err
		}
		var receipt dropinReceipt
		if err := decodeDiagnostic(s.Extra["dropin:"+h], &receipt); err != nil {
			return fmt.Errorf("D5 receipt %s: %w", h, err)
		}
		phase, _, err := readAttempt(c, attemptPath(c, "phase:dropin:"+h), now)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(s.Extra["dropin:"+h]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&receipt); err != nil {
			return fmt.Errorf("D5 receipt %s: %w", h, err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return errors.New("trailing D5 receipt data")
		}
		if receipt.Host != h || !receipt.Done || receipt.Restored || receipt.IntentAt.IsZero() || receipt.IntentAt.Before(phase.Started) || receipt.IntentAt.After(now) {
			return fmt.Errorf("invalid mandatory D5 receipt: %s", h)
		}
		if receipt.Absent {
			if !contains(c.DropinAbsentHosts, h) || receipt.SHA256 != "" || receipt.Mode != 0 || receipt.UID != 0 || receipt.GID != 0 {
				return fmt.Errorf("unreviewed or conflicting D5 absence: %s", h)
			}
		} else if !regexp.MustCompile("^[0-9a-f]{64}$").MatchString(receipt.SHA256) || receipt.Mode <= 0 || receipt.Mode > 0777 || receipt.UID < 0 || receipt.GID < 0 {
			return fmt.Errorf("incomplete mandatory D5 original metadata: %s", h)
		}
	}
	return nil
}
