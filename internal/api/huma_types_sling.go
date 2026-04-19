package api

// Per-domain Huma input/output types for the sling handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_sling.go.

// --- Sling types ---

// SlingInput is the Huma input for POST /v0/city/{cityName}/sling.
type SlingInput struct {
	CityScope
	Body struct {
		Rig            string            `json:"rig,omitempty" doc:"Rig name."`
		Target         string            `json:"target,omitempty" doc:"Target agent or pool."`
		Bead           string            `json:"bead,omitempty" doc:"Bead ID to sling."`
		Formula        string            `json:"formula,omitempty" doc:"Formula name for workflow launch."`
		AttachedBeadID string            `json:"attached_bead_id,omitempty" doc:"Bead ID to attach a formula to."`
		Title          string            `json:"title,omitempty" doc:"Workflow title."`
		Vars           map[string]string `json:"vars,omitempty" doc:"Formula variables."`
		ScopeKind      string            `json:"scope_kind,omitempty" doc:"Scope kind (city or rig)."`
		ScopeRef       string            `json:"scope_ref,omitempty" doc:"Scope reference."`
		Force          bool              `json:"force,omitempty" doc:"Override source workflow conflict checks."`
	}
}

// SlingConflictResponse is returned when a source bead already has a live
// graph workflow and the caller did not request force replacement.
type SlingConflictResponse struct {
	Code                string   `json:"code" doc:"Machine-readable error code." example:"conflict"`
	Message             string   `json:"message" doc:"Human-readable conflict description."`
	SourceBeadID        string   `json:"source_bead_id" doc:"Source bead whose singleton workflow is already live."`
	BlockingWorkflowIDs []string `json:"blocking_workflow_ids" nullable:"false" doc:"Live workflow root bead IDs blocking the launch."`
	Hint                string   `json:"hint" doc:"Suggested override or cleanup action."`
}
