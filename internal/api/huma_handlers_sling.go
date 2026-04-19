package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// SlingOutput is the Huma response for POST /v0/sling.
// The HTTP status code varies (200 for direct sling, 201 for workflow launch),
// so we use a custom status field.
type SlingOutput struct {
	Status int `header:"_status" doc:"HTTP status code."`
	Body   slingResponse
}

func (e *SlingConflictResponse) Error() string {
	if e == nil {
		return "source workflow conflict"
	}
	return e.Message
}

// GetStatus tells Huma to serialize sling source-workflow conflicts as HTTP 409.
func (e *SlingConflictResponse) GetStatus() int {
	return http.StatusConflict
}

// humaHandleSling is the Huma-typed handler for POST /v0/sling.
func (s *Server) humaHandleSling(ctx context.Context, input *SlingInput) (*SlingOutput, error) {
	body := slingBody{
		Rig:            input.Body.Rig,
		Target:         input.Body.Target,
		Bead:           input.Body.Bead,
		Formula:        input.Body.Formula,
		AttachedBeadID: input.Body.AttachedBeadID,
		Title:          input.Body.Title,
		Vars:           input.Body.Vars,
		ScopeKind:      input.Body.ScopeKind,
		ScopeRef:       input.Body.ScopeRef,
		Force:          input.Body.Force,
	}

	if body.Target == "" {
		return nil, huma.Error400BadRequest("target agent or pool is required")
	}

	body.ScopeKind = strings.TrimSpace(body.ScopeKind)
	body.ScopeRef = strings.TrimSpace(body.ScopeRef)

	cfg := s.state.Config()
	syncFeatureFlags(cfg)
	body.Target = qualifySlingTarget(cfg, body.Target, slingRigContext(body))
	agentCfg, ok := findAgent(cfg, body.Target)
	if !ok {
		return nil, huma.Error404NotFound("target " + body.Target + " not found")
	}

	if body.Bead == "" && body.Formula == "" {
		return nil, huma.Error400BadRequest("bead or formula is required")
	}
	if body.Bead != "" && body.Formula != "" {
		return nil, huma.Error400BadRequest("bead and formula are mutually exclusive")
	}
	if body.Bead != "" && body.AttachedBeadID != "" {
		return nil, huma.Error400BadRequest("bead and attached_bead_id are mutually exclusive")
	}

	workflowLaunchOptions := body.AttachedBeadID != "" ||
		len(body.Vars) > 0 ||
		body.Title != "" ||
		body.ScopeKind != "" ||
		body.ScopeRef != ""
	defaultFormulaLaunch := body.Formula == "" &&
		body.AttachedBeadID == "" &&
		body.Bead != "" &&
		agentCfg.EffectiveDefaultSlingFormula() != "" &&
		(len(body.Vars) > 0 || body.Title != "" || body.ScopeKind != "" || body.ScopeRef != "")
	if body.Formula == "" && body.AttachedBeadID != "" {
		return nil, huma.Error400BadRequest("formula is required when attached_bead_id is provided")
	}
	if body.Formula == "" && workflowLaunchOptions && !defaultFormulaLaunch {
		return nil, huma.Error400BadRequest("formula or target default formula is required when vars, title, or scope are provided")
	}
	if (body.ScopeKind == "") != (body.ScopeRef == "") {
		return nil, huma.Error400BadRequest("scope_kind and scope_ref must be provided together")
	}
	if body.ScopeKind != "" && body.ScopeKind != "city" && body.ScopeKind != "rig" {
		return nil, huma.Error400BadRequest("scope_kind must be 'city' or 'rig'")
	}
	if body.ScopeKind == "rig" && body.ScopeRef != "" {
		if agentCfg.Dir != body.ScopeRef {
			msg := "scope_ref " + body.ScopeRef + " conflicts with resolved target rig " + agentCfg.Dir
			if agentCfg.Dir == "" {
				msg = "scope_ref " + body.ScopeRef + " requires a rig-scoped target; resolved target " + body.Target + " is city-scoped"
			}
			return nil, huma.Error400BadRequest(msg)
		}
		if body.Rig != "" && body.Rig != body.ScopeRef {
			return nil, huma.Error400BadRequest("rig " + body.Rig + " conflicts with scope_ref " + body.ScopeRef)
		}
	}

	resp, status, code, message, conflict := s.execSlingDirect(ctx, body, agentCfg)
	if code != "" {
		if conflict != nil {
			blockingIDs := append([]string(nil), conflict.WorkflowIDs...)
			if blockingIDs == nil {
				blockingIDs = []string{}
			}
			return nil, &SlingConflictResponse{
				Code:                code,
				Message:             message,
				SourceBeadID:        conflict.SourceBeadID,
				BlockingWorkflowIDs: blockingIDs,
				Hint: fmt.Sprintf("use --force to override, or %s to clean up",
					sourceWorkflowCleanupHint(conflict.SourceBeadID, s.slingStoreRef(body.Rig, agentCfg))),
			}
		}
		if status == http.StatusNotFound {
			return nil, huma.Error404NotFound(message)
		}
		if status == http.StatusConflict {
			return nil, huma.Error409Conflict(message)
		}
		if status >= http.StatusInternalServerError {
			return nil, huma.Error500InternalServerError(message)
		}
		return nil, huma.Error400BadRequest(message)
	}

	return &SlingOutput{
		Status: status,
		Body:   *resp,
	}, nil
}
