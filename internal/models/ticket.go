package models

import (
	"time"

	"github.com/google/uuid"
)

// TicketStatus represents the lifecycle/assignment state of a ticket.
type TicketStatus string

const (
	TicketStatusOpen       TicketStatus = "open"       // newly created, never assigned
	TicketStatusAssigned   TicketStatus = "assigned"   // has an active assignee
	TicketStatusUnassigned TicketStatus = "unassigned" // was assigned, explicitly unassigned
	TicketStatusClosed     TicketStatus = "closed"
)

// Ticket represents a single, persistent ticket tied to a contact. Unlike
// AgentTransfer (an ephemeral chatbot<->agent handoff record), a Ticket is
// reused for the lifetime of the contact: it is opened once, cycles through
// assign/transfer/unassign, and is closed/reopened rather than recreated.
type Ticket struct {
	BaseModel
	OrganizationID  uuid.UUID    `gorm:"type:uuid;index;not null" json:"organization_id"`
	ContactID       uuid.UUID    `gorm:"type:uuid;uniqueIndex;not null" json:"contact_id"`
	Status          TicketStatus `gorm:"size:20;default:'open';index" json:"status"`
	AssignedUserID  *uuid.UUID   `gorm:"type:uuid;index" json:"assigned_user_id,omitempty"`
	AssignedAt      *time.Time   `json:"assigned_at,omitempty"`
	CreatedByUserID *uuid.UUID   `gorm:"type:uuid" json:"created_by_user_id,omitempty"` // nil = system-created
	ClosedByUserID  *uuid.UUID   `gorm:"type:uuid" json:"closed_by_user_id,omitempty"`
	ClosedAt        *time.Time   `json:"closed_at,omitempty"`
	ReopenedAt      *time.Time   `json:"reopened_at,omitempty"`
	// Attributes is free-form key/value scratch data for this ticket's
	// lifetime (e.g. captured form fields, issue category). It is cleared
	// when the ticket closes so the next ticket for the contact starts fresh.
	Attributes JSONB `gorm:"type:jsonb;default:'{}'" json:"attributes"`

	// Relations
	Organization  *Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	Contact       *Contact      `gorm:"foreignKey:ContactID" json:"contact,omitempty"`
	AssignedUser  *User         `gorm:"foreignKey:AssignedUserID" json:"assigned_user,omitempty"`
	CreatedByUser *User         `gorm:"foreignKey:CreatedByUserID" json:"created_by_user,omitempty"`
	ClosedByUser  *User         `gorm:"foreignKey:ClosedByUserID" json:"closed_by_user,omitempty"`
}
