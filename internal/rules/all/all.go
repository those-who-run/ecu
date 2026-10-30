// Package all registers every rule with internal/rules: a program that runs the
// engine imports it for its side effect. Each rule's package is here, in
// rules.Order; a new rule adds its line (internal/rules/README.md).
package all

import (
	_ "github.com/those-who-run/ecu/internal/rules/finallap"    // final_lap, paddock#93
	_ "github.com/those-who-run/ecu/internal/rules/fuel"        // fuel, paddock#92
	_ "github.com/those-who-run/ecu/internal/rules/laptime"     // lap_time, paddock#91
	_ "github.com/those-who-run/ecu/internal/rules/position"    // position, paddock#94
	_ "github.com/those-who-run/ecu/internal/rules/racesummary" // race_summary, paddock#96
	_ "github.com/those-who-run/ecu/internal/rules/startbrief"  // start_brief, paddock#95
	_ "github.com/those-who-run/ecu/internal/rules/temps"       // temps, paddock#97
)
