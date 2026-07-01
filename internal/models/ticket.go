package models

import (
	"time"

	"github.com/google/uuid"
)

// TicketActivity records every state-change event on a ticket (assign,
// transfer, unassign, close, reopen). It is written alongside the system
// message that appears in the contact's chat timeline, giving callers a
// structured, queryable log without parsing message content.
type TicketActivity struct {
	ID             uuid.UUID  `gorm:"type:uuid;primary_key;default:gen_random_uuid()" json:"id"`
	OrganizationID uuid.UUID  `gorm:"type:uuid;index;not null" json:"organization_id"`
	TicketID       uuid.UUID  `gorm:"type:uuid;index;not null" json:"ticket_id"`
	ContactID      uuid.UUID  `gorm:"type:uuid;index;not null" json:"contact_id"`
	Action         string     `gorm:"size:50;not null;index" json:"action"`
	ActorUserID    uuid.UUID  `gorm:"type:uuid;not null" json:"actor_user_id"`
	ActorUserName  string     `gorm:"size:255;not null" json:"actor_user_name"`
	TargetUserID   *uuid.UUID `gorm:"type:uuid" json:"target_user_id,omitempty"`
	TargetUserName string     `gorm:"size:255" json:"target_user_name,omitempty"`
	Note           string     `gorm:"type:text" json:"note,omitempty"`
	Content        string     `gorm:"type:text;not null" json:"content"`
	CreatedAt      time.Time  `gorm:"autoCreateTime" json:"created_at"`
}

func (TicketActivity) TableName() string { return "ticket_activities" }

// TicketStatus represents the lifecycle/assignment state of a ticket.
type TicketStatus string

const (
	TicketStatusOpen       TicketStatus = "open"       // newly created, never assigned
	TicketStatusAssigned   TicketStatus = "assigned"   // has an active assignee
	TicketStatusUnassigned TicketStatus = "unassigned" // was assigned, explicitly unassigned
	TicketStatusClosed     TicketStatus = "closed"
)

// Ticket represents a support ticket tied to a contact. A contact can have
// multiple tickets over time, but only one may be active (non-closed) at once.
// Closing a ticket allows a new one to be opened for the same contact.
type Ticket struct {
	BaseModel
	// Number is a globally sequential, human-readable ticket identifier (e.g. #42).
	Number         int64        `gorm:"autoIncrement;uniqueIndex" json:"number"`
	OrganizationID uuid.UUID    `gorm:"type:uuid;index;not null" json:"organization_id"`
	// Partial unique index ensures at most one non-closed ticket per contact.
	// Multiple closed tickets for the same contact are allowed (ticket history).
	ContactID      uuid.UUID    `gorm:"type:uuid;not null;index;uniqueIndex:idx_active_ticket_contact,where:status != 'closed'" json:"contact_id"`
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
