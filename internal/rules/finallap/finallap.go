// Package finallap is v1's final_lap rule (rules/final_lap.py at v1.0.0,
// paddock#93): "Final lap." once a race, as the lap counter reaches the lap
// total. The decision is the race state's race.final_lap (paddock#84), on the
// same frame; the rule turns it into the callout. A timed race has no lap total
// and no final lap.
package finallap

import (
	"github.com/those-who-run/ecu/internal/race"
	"github.com/those-who-run/ecu/internal/rules"
)

// Name is the rule's name, its key in the rules file.
const Name = "final_lap"

var _ = rules.Register(Name, func(*rules.Settings) rules.Rule { return &rule{} })

// rule is FinalLapRule. The race state announces the final lap once a race
// (race.Context.FinalLap), so the rule keeps nothing of its own.
type rule struct{}

func (*rule) Reset() {}

// Evaluate raises the rule's template, with no slots, on the frame of the final
// lap.
func (*rule) Evaluate(ctx *race.Context) []rules.Callout {
	if !ctx.FinalLap {
		return nil
	}
	return []rules.Callout{{Kind: "template"}}
}
