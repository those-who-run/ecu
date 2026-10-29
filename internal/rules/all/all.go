// Package all registers every rule with internal/rules: a program that runs the
// engine imports it for its side effect. Each rule's package is here, in
// rules.Order; a new rule adds its line (internal/rules/README.md).
package all

import (
	_ "github.com/those-who-run/ecu/internal/rules/finallap" // final_lap, paddock#93
	_ "github.com/those-who-run/ecu/internal/rules/position" // position, paddock#94
)
