package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultAutomaticRecoveries = 2
	DefaultRecoveryWindow      = 24 * time.Hour
	DefaultRecoveryWindowText  = "24h"
	MaxAutomaticRecoveries     = 10
)

// RecoveryPolicy is the workspace bound on automatic work-order recovery.
// It is read by the order clock and is not part of FreezePolicy (DEC-47).
// A bound of 0 is valid and disables automatic recovery. Window is derived
// from WindowText; only WindowText crosses YAML and JSON.
type RecoveryPolicy struct {
	AutomaticRecoveries int           `yaml:"automatic_recoveries" json:"automatic_recoveries"`
	Window              time.Duration `yaml:"-" json:"-"`
	WindowText          string        `yaml:"window" json:"window"`
	set                 bool          `yaml:"-" json:"-"`
}

// DefaultRecoveryPolicy is two recoveries per order per 24 hours.
func DefaultRecoveryPolicy() RecoveryPolicy {
	return RecoveryPolicy{
		AutomaticRecoveries: DefaultAutomaticRecoveries,
		Window:              DefaultRecoveryWindow,
		WindowText:          DefaultRecoveryWindowText,
		set:                 true,
	}
}

// Present reports whether the section was supplied rather than defaulted.
func (p RecoveryPolicy) Present() bool { return p.set }

func (p *RecoveryPolicy) UnmarshalYAML(node *yaml.Node) error {
	type plain struct {
		AutomaticRecoveries int    `yaml:"automatic_recoveries"`
		Window              string `yaml:"window"`
	}
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	p.AutomaticRecoveries = decoded.AutomaticRecoveries
	p.WindowText = decoded.Window
	p.Window = 0
	p.set = true
	return nil
}

func (p *RecoveryPolicy) UnmarshalJSON(data []byte) error {
	type plain struct {
		AutomaticRecoveries int    `json:"automatic_recoveries"`
		Window              string `json:"window"`
	}
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	p.AutomaticRecoveries = decoded.AutomaticRecoveries
	p.WindowText = decoded.Window
	p.Window = 0
	p.set = true
	return nil
}

// Normalize defaults a missing section to {2, 24h} and refuses a bound
// outside 0..10 or a non-positive window. A bound of 0 stays valid.
func (p *RecoveryPolicy) Normalize() error {
	if p == nil {
		return fmt.Errorf("recovery policy is required")
	}
	if !p.set && p.AutomaticRecoveries == 0 && p.WindowText == "" && p.Window == 0 {
		*p = DefaultRecoveryPolicy()
		return nil
	}
	if p.AutomaticRecoveries < 0 || p.AutomaticRecoveries > MaxAutomaticRecoveries {
		return fmt.Errorf("recovery.automatic_recoveries must be between 0 and %d", MaxAutomaticRecoveries)
	}
	text := p.WindowText
	if text == "" && p.Window > 0 {
		text = p.Window.String()
	}
	if text == "" {
		return fmt.Errorf("recovery.window must be a positive duration")
	}
	parsed, err := time.ParseDuration(text)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("recovery.window must be a positive duration")
	}
	p.Window = parsed
	p.WindowText = text
	p.set = true
	return nil
}

// ReadClockPolicy parses only recovery and work_order_queue_timeout from a
// stored workspace document. Missing keys take the same defaults as workspace
// config validation. The clock calls it on each tick.
func ReadClockPolicy(configYAML []byte) (RecoveryPolicy, time.Duration, error) {
	var document struct {
		Recovery                  RecoveryPolicy `yaml:"recovery"`
		WorkOrderQueueTimeoutText string         `yaml:"work_order_queue_timeout"`
	}
	if len(bytes.TrimSpace(configYAML)) > 0 {
		if err := yaml.Unmarshal(configYAML, &document); err != nil {
			return RecoveryPolicy{}, 0, fmt.Errorf("parse clock policy: %w", err)
		}
	}
	if err := document.Recovery.Normalize(); err != nil {
		return RecoveryPolicy{}, 0, err
	}
	text := document.WorkOrderQueueTimeoutText
	if text == "" {
		text = DefaultWorkOrderQueueTimeoutText
	}
	timeout, err := time.ParseDuration(text)
	if err != nil || timeout <= 0 {
		return RecoveryPolicy{}, 0, fmt.Errorf("work_order_queue_timeout must be a positive duration")
	}
	return document.Recovery, timeout, nil
}
