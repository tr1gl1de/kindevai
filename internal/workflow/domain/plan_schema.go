package domain

import (
	"bytes"
	_ "embed"
	"encoding/json"
)

// PlanSchemaVersion is the version of the plan JSON Schema the planner is asked to follow (KIN-34). The schema is
// versioned in the repository next to the Plan type; a test keeps both in step.
const PlanSchemaVersion = "v1"

//go:embed plan.schema.json
var planSchema []byte

// PlanSchema returns the plan JSON Schema, compacted (it is part of every planner prompt).
func PlanSchema() []byte {
	var b bytes.Buffer
	if err := json.Compact(&b, planSchema); err != nil {
		return planSchema // embedded at build time; a test parses it
	}
	return b.Bytes()
}
