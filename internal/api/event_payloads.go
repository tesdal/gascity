package api

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
)

// API-layer event payload types. Every API emitter takes one of these
// typed structs (or one defined in internal/extmsg) via the sealed
// events.Payload interface rather than map[string]any (Principle 7).
// The event bus stores payloads as []byte for domain-agnostic
// transport (Principle 4 edge case); the SSE projection uses the
// central events registry to decode the bytes back into the typed Go
// variant before emitting on the typed /v0/events/stream wire schema.

// MailEventPayload is the shape of every mail.* event payload
// (MailSent, MailRead, MailArchived, MailMarkedRead, MailMarkedUnread,
// MailReplied, MailDeleted). Message is nil for mark/archive/delete
// events; present for send/reply events.
type MailEventPayload struct {
	Rig     string        `json:"rig"`
	Message *mail.Message `json:"message,omitempty"`
}

// IsEventPayload marks MailEventPayload as an events.Payload variant.
func (MailEventPayload) IsEventPayload() {}

// CityCreatedPayload is emitted on city.created when the supervisor's
// POST /v0/city handler has scaffolded and registered a new city.
// Consumers subscribed to /v0/events/stream use this event to learn
// about newly-created cities before they are fully initialized. The
// matching city.ready / city.init_failed event follows once the
// supervisor reconciler finishes preparing the city (or gives up).
type CityCreatedPayload struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// IsEventPayload marks CityCreatedPayload as an events.Payload variant.
func (CityCreatedPayload) IsEventPayload() {}

// CityReadyPayload is emitted on city.ready when the supervisor
// reconciler has finished preparing a city (bead store started,
// formulas resolved, agents validated). The city is now in the
// running inventory and ready to accept work.
type CityReadyPayload struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// IsEventPayload marks CityReadyPayload as an events.Payload variant.
func (CityReadyPayload) IsEventPayload() {}

// CityInitFailedPayload is emitted on city.init_failed when the
// supervisor reconciler fails to bring up a city. The payload carries
// a human-readable error string sourced from the reconciler step that
// failed (validate rigs, startBeadsLifecycle, etc.) plus the phases
// the reconciler completed before the failure.
type CityInitFailedPayload struct {
	Name            string   `json:"name"`
	Path            string   `json:"path"`
	Error           string   `json:"error"`
	PhasesCompleted []string `json:"phases_completed,omitempty"`
}

// IsEventPayload marks CityInitFailedPayload as an events.Payload variant.
func (CityInitFailedPayload) IsEventPayload() {}

// BeadEventPayload is the shape of every bead.* event payload
// (BeadCreated, BeadUpdated, BeadClosed). The payload carries a full
// snapshot of the bead as of the event; it is emitted by the beads
// CachingStore's reconcile loop when external changes are detected.
type BeadEventPayload struct {
	Bead beads.Bead `json:"bead"`
}

// IsEventPayload marks BeadEventPayload as an events.Payload variant.
func (BeadEventPayload) IsEventPayload() {}

func init() {
	// mail.* — all seven types share one payload shape.
	events.RegisterPayload(events.MailSent, MailEventPayload{})
	events.RegisterPayload(events.MailRead, MailEventPayload{})
	events.RegisterPayload(events.MailArchived, MailEventPayload{})
	events.RegisterPayload(events.MailMarkedRead, MailEventPayload{})
	events.RegisterPayload(events.MailMarkedUnread, MailEventPayload{})
	events.RegisterPayload(events.MailReplied, MailEventPayload{})
	events.RegisterPayload(events.MailDeleted, MailEventPayload{})

	// bead.* — carry the bead snapshot.
	events.RegisterPayload(events.BeadCreated, BeadEventPayload{})
	events.RegisterPayload(events.BeadUpdated, BeadEventPayload{})
	events.RegisterPayload(events.BeadClosed, BeadEventPayload{})

	// session.* / convoy.* / controller.* / city.* / order.* /
	// provider.* — these events carry no structured payload today;
	// their semantics are fully captured by the envelope's Actor,
	// Subject, and Message fields. NoPayload registers an empty typed
	// shape so the spec still emits a discriminated-union variant
	// for the event type and the registry-coverage test passes.
	events.RegisterPayload(events.SessionWoke, events.NoPayload{})
	events.RegisterPayload(events.SessionStopped, events.NoPayload{})
	events.RegisterPayload(events.SessionCrashed, events.NoPayload{})
	events.RegisterPayload(events.SessionDraining, events.NoPayload{})
	events.RegisterPayload(events.SessionUndrained, events.NoPayload{})
	events.RegisterPayload(events.SessionQuarantined, events.NoPayload{})
	events.RegisterPayload(events.SessionIdleKilled, events.NoPayload{})
	events.RegisterPayload(events.SessionSuspended, events.NoPayload{})
	events.RegisterPayload(events.SessionUpdated, events.NoPayload{})
	events.RegisterPayload(events.ConvoyCreated, events.NoPayload{})
	events.RegisterPayload(events.ConvoyClosed, events.NoPayload{})
	events.RegisterPayload(events.ControllerStarted, events.NoPayload{})
	events.RegisterPayload(events.ControllerStopped, events.NoPayload{})
	events.RegisterPayload(events.CitySuspended, events.NoPayload{})
	events.RegisterPayload(events.CityResumed, events.NoPayload{})
	events.RegisterPayload(events.CityCreated, CityCreatedPayload{})
	events.RegisterPayload(events.CityReady, CityReadyPayload{})
	events.RegisterPayload(events.CityInitFailed, CityInitFailedPayload{})
	events.RegisterPayload(events.OrderFired, events.NoPayload{})
	events.RegisterPayload(events.OrderCompleted, events.NoPayload{})
	events.RegisterPayload(events.OrderFailed, events.NoPayload{})
	events.RegisterPayload(events.ProviderSwapped, events.NoPayload{})
}
