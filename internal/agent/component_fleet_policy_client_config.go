package agent

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"gopkg.in/yaml.v3"
)

// This file is read-only: config.yaml remains operator-owned. The same YAML
// decoder as Steward handles aliases, merged mappings, quoted keys and duplicate
// keys. A lexical key search cannot establish whether an override exists.
func fleetCheckExplicitPolicyClient(c *Context, intended []byte) error {
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	home, configHome := c.Home, ""
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, "HOME="); ok {
			home = value
		}
		if value, ok := strings.CutPrefix(item, "XDG_CONFIG_HOME="); ok {
			configHome = value
		}
	}
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(configHome) {
		return errors.New("policy client configuration root must be absolute")
	}
	path := filepath.Join(configHome, "t3-steward", "config.yaml")
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("policy client base configuration cannot be safely read")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > fleetCoordinatorMaxBytes || info.Mode().Perm()&0022 != 0 {
		return errors.New("policy client base configuration must be a bounded non-writable regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("policy client base configuration ownership is unknown")
	}
	body, err := io.ReadAll(io.LimitReader(file, fleetCoordinatorMaxBytes+1))
	if err != nil || len(body) > fleetCoordinatorMaxBytes {
		return errors.New("policy client base configuration read failed")
	}
	var config map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&config); err != nil && !errors.Is(err, io.EOF) {
		return errors.New("policy client base configuration is malformed")
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("policy client base configuration must contain one document")
	}
	value, present := config["backlog_v2"]
	if !present || value == nil {
		return nil
	}
	v2, ok := value.(map[string]any)
	if !ok {
		return errors.New("policy client backlog_v2 configuration is malformed")
	}
	if mode, present := v2["mode"]; present && mode == "coordinator" {
		return errors.New("intended remote policy client conflicts with coordinator mode")
	}
	value, present = v2["coordinator_client"]
	if !present || value == nil {
		return nil
	}
	explicit, ok := value.(map[string]any)
	if !ok {
		return errors.New("explicit policy client configuration is malformed")
	}
	// A defaults-only block does not select the remote carrier in Steward.
	if len(explicit) == 0 || (len(explicit) == 1 && explicit["defaults"] != nil) {
		return nil
	}
	desired, err := canonjson.DecodeObject(intended)
	if err != nil {
		return err
	}
	delete(desired, "schema_version")
	desired["credential"] = desired["credential_ref"]
	delete(desired, "credential_ref")
	delete(explicit, "defaults")
	before, err := canonjson.MarshalFile(explicit)
	if err != nil {
		return errors.New("explicit policy client configuration cannot be compared")
	}
	after, err := canonjson.MarshalFile(desired)
	if err != nil {
		return err
	}
	if !bytes.Equal(before, after) {
		return errors.New("explicit coordinator_client differs from intended fleet client; preserve operator configuration for reviewed reconciliation")
	}
	return nil
}
