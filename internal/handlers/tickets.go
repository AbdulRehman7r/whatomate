package handlers

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// ticketRow is a flat row result from the tickets JOIN contacts/users query,
// used by both ListTickets and the single-ticket fetchers.
type ticketRow struct {
	ID              uuid.UUID           `gorm:"column:id"`
	Number          int64               `gorm:"column:number"`
	OrganizationID  uuid.UUID           `gorm:"column:organization_id"`
	ContactID       uuid.UUID           `gorm:"column:contact_id"`
	Status          models.TicketStatus `gorm:"column:status"`
	AssignedUserID  *uuid.UUID          `gorm:"column:assigned_user_id"`
	AssignedAt      *time.Time          `gorm:"column:assigned_at"`
	CreatedByUserID *uuid.UUID          `gorm:"column:created_by_user_id"`
	ClosedByUserID  *uuid.UUID          `gorm:"column:closed_by_user_id"`
	ClosedAt        *time.Time          `gorm:"column:closed_at"`
	ReopenedAt      *time.Time          `gorm:"column:reopened_at"`
	Attributes      models.JSONB        `gorm:"column:attributes;type:jsonb"`
	CreatedAt       time.Time           `gorm:"column:created_at"`
	UpdatedAt       time.Time           `gorm:"column:updated_at"`

	ContactName      *string `gorm:"column:contact_name"`
	PhoneNumber      *string `gorm:"column:phone_number"`
	AssignedUserName *string `gorm:"column:assigned_user_name"`
}

const ticketRowSelect = "tickets.*, contacts.profile_name AS contact_name, contacts.phone_number AS phone_number, assigned_user.full_name AS assigned_user_name"

// ticketJoinQuery returns the base tickets query joined with contact and
// assigned-user display fields, scoped to the organization.
func (a *App) ticketJoinQuery(orgID uuid.UUID) *gorm.DB {
	return a.DB.Table("tickets").
		Joins("LEFT JOIN contacts ON contacts.id = tickets.contact_id").
		Joins("LEFT JOIN users AS assigned_user ON assigned_user.id = tickets.assigned_user_id").
		Where("tickets.organization_id = ?", orgID)
}

// fetchTicketRow loads a single joined ticket row matching the given condition.
func (a *App) fetchTicketRow(orgID uuid.UUID, where string, args ...any) (*ticketRow, error) {
	var row ticketRow
	err := a.ticketJoinQuery(orgID).Select(ticketRowSelect).Where(where, args...).Take(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// getOrCreateTicket returns the active (non-closed) ticket for a contact,
// creating one (status=open) if none exists. Closed tickets are skipped so
// that closing a ticket and then re-opening the conversation creates a new
// ticket with a fresh number and attributes. createdByUserID is recorded only
// on creation.
func (a *App) getOrCreateTicket(orgID, contactID uuid.UUID, createdByUserID *uuid.UUID) (*models.Ticket, error) {
	var ticket models.Ticket
	err := a.DB.Where("organization_id = ? AND contact_id = ? AND status != ?", orgID, contactID, models.TicketStatusClosed).First(&ticket).Error
	if err == nil {
		return &ticket, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	ticket = models.Ticket{
		OrganizationID:  orgID,
		ContactID:       contactID,
		Status:          models.TicketStatusOpen,
		CreatedByUserID: createdByUserID,
		Attributes:      models.JSONB{},
	}
	if err := a.DB.Create(&ticket).Error; err != nil {
		return nil, err
	}
	return &ticket, nil
}

// TicketResponse is the API representation of a ticket, flattened with
// contact and assigned-user display fields.
type TicketResponse struct {
	ID               uuid.UUID           `json:"id"`
	Number           int64               `json:"number"`
	ContactID        uuid.UUID           `json:"contact_id"`
	ContactName      string              `json:"contact_name,omitempty"`
	PhoneNumber      string              `json:"phone_number,omitempty"`
	Status           models.TicketStatus `json:"status"`
	AssignedUserID   *uuid.UUID          `json:"assigned_user_id,omitempty"`
	AssignedUserName string              `json:"assigned_user_name,omitempty"`
	AssignedAt       *time.Time          `json:"assigned_at,omitempty"`
	CreatedByUserID  *uuid.UUID          `json:"created_by_user_id,omitempty"`
	ClosedByUserID   *uuid.UUID          `json:"closed_by_user_id,omitempty"`
	ClosedAt         *time.Time          `json:"closed_at,omitempty"`
	ReopenedAt       *time.Time          `json:"reopened_at,omitempty"`
	Attributes       models.JSONB        `json:"attributes"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

func ticketRowToResponse(row *ticketRow) TicketResponse {
	resp := TicketResponse{
		ID:              row.ID,
		Number:          row.Number,
		ContactID:       row.ContactID,
		Status:          row.Status,
		AssignedUserID:  row.AssignedUserID,
		AssignedAt:      row.AssignedAt,
		CreatedByUserID: row.CreatedByUserID,
		ClosedByUserID:  row.ClosedByUserID,
		ClosedAt:        row.ClosedAt,
		ReopenedAt:      row.ReopenedAt,
		Attributes:      row.Attributes,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
	if row.ContactName != nil {
		resp.ContactName = *row.ContactName
	}
	if row.PhoneNumber != nil {
		resp.PhoneNumber = *row.PhoneNumber
	}
	if row.AssignedUserName != nil {
		resp.AssignedUserName = *row.AssignedUserName
	}
	return resp
}

// ticketAuditSnapshot returns a diff-friendly representation of a ticket for
// audit logging.
func ticketAuditSnapshot(t *models.Ticket) map[string]any {
	if t == nil {
		return nil
	}
	snap := map[string]any{
		"status": string(t.Status),
	}
	if t.AssignedUserID != nil {
		snap["assigned_user_id"] = t.AssignedUserID.String()
	}
	if len(t.Attributes) > 0 {
		snap["attributes"] = t.Attributes
	}
	return snap
}

// broadcastTicketEvent notifies the organization's connected clients of a
// ticket state change.
func (a *App) broadcastTicketEvent(ticket *models.Ticket, msgType string, actingUserID uuid.UUID, extra map[string]any) {
	if a.WSHub == nil {
		return
	}
	payload := map[string]any{
		"id":         ticket.ID.String(),
		"contact_id": ticket.ContactID.String(),
		"status":     ticket.Status,
		"by_user_id": actingUserID.String(),
	}
	if ticket.AssignedUserID != nil {
		payload["assigned_user_id"] = ticket.AssignedUserID.String()
	}
	for k, v := range extra {
		payload[k] = v
	}
	a.WSHub.BroadcastToOrg(ticket.OrganizationID, websocket.WSMessage{
		Type:    msgType,
		Payload: payload,
	})
}

// recordTicketEvent writes a ticket state-change event to two places:
//  1. The messages table (direction=system, type=activity) so it appears inline
//     in the contact's chat timeline with no extra frontend work.
//  2. The ticket_activities table for structured, queryable history.
func (a *App) recordTicketEvent(ticket *models.Ticket, action models.AuditAction, actorID uuid.UUID, targetID *uuid.UUID, note string) {
	// Resolve display names once.
	actorName := a.lookupUserName(actorID)
	var targetName string
	if targetID != nil {
		targetName = a.lookupUserName(*targetID)
	}

	// Build the human-readable content string.
	var content string
	switch action {
	case models.AuditActionAssigned:
		content = fmt.Sprintf("Ticket #%d assigned to %s by %s", ticket.Number, targetName, actorName)
	case models.AuditActionTransferred:
		content = fmt.Sprintf("Ticket #%d transferred to %s by %s", ticket.Number, targetName, actorName)
	case models.AuditActionUnassigned:
		content = fmt.Sprintf("Ticket #%d unassigned by %s", ticket.Number, actorName)
	case models.AuditActionClosed:
		content = fmt.Sprintf("Ticket #%d closed by %s", ticket.Number, actorName)
	case models.AuditActionReopened:
		content = fmt.Sprintf("Ticket #%d reopened by %s", ticket.Number, actorName)
	default:
		content = fmt.Sprintf("Ticket #%d: %s by %s", ticket.Number, action, actorName)
	}
	if note != "" {
		content += ": " + note
	}

	// 1. System message in chat timeline.
	msg := models.Message{
		OrganizationID:  ticket.OrganizationID,
		WhatsAppAccount: "",
		ContactID:       ticket.ContactID,
		Direction:       models.DirectionSystem,
		MessageType:     models.MessageTypeActivity,
		Content:         content,
		SentByUserID:    &actorID,
	}
	if err := a.DB.Create(&msg).Error; err != nil {
		a.Log.Error("Failed to create ticket activity message", "error", err)
	}

	// 2. Structured activity log.
	activity := models.TicketActivity{
		OrganizationID: ticket.OrganizationID,
		TicketID:       ticket.ID,
		ContactID:      ticket.ContactID,
		Action:         string(action),
		ActorUserID:    actorID,
		ActorUserName:  actorName,
		TargetUserID:   targetID,
		TargetUserName: targetName,
		Note:           note,
		Content:        content,
	}
	if err := a.DB.Create(&activity).Error; err != nil {
		a.Log.Error("Failed to create ticket activity log", "error", err)
	}
}

// lookupUserName fetches a user's display name, falling back to "Unknown".
func (a *App) lookupUserName(id uuid.UUID) string {
	var u models.User
	if err := a.DB.Select("full_name").First(&u, "id = ?", id).Error; err != nil {
		return "Unknown"
	}
	return u.FullName
}

// findOrgUser verifies a user belongs to the organization, mirroring the
// check used by AssignContact.
func (a *App) findOrgUser(orgID, userID uuid.UUID) error {
	var user models.User
	return a.DB.Where("id = ? AND organization_id = ?", userID, orgID).First(&user).Error
}

// ListTickets lists tickets for the organization with optional filters.
func (a *App) ListTickets(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceTickets, models.ActionRead)
	if err != nil {
		return nil
	}

	pg := parsePagination(r)

	status := string(r.RequestCtx.QueryArgs().Peek("status"))
	assignedUserIDStr := string(r.RequestCtx.QueryArgs().Peek("assigned_user_id"))
	unassignedOnly := string(r.RequestCtx.QueryArgs().Peek("unassigned")) == "true"
	search := string(r.RequestCtx.QueryArgs().Peek("search"))

	query := a.ticketJoinQuery(orgID)

	if status != "" {
		query = query.Where("tickets.status = ?", status)
	}
	if assignedUserIDStr != "" {
		assignedUserID, err := uuid.Parse(assignedUserIDStr)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid assigned_user_id", nil, "")
		}
		query = query.Where("tickets.assigned_user_id = ?", assignedUserID)
	}
	if unassignedOnly {
		query = query.Where("tickets.assigned_user_id IS NULL")
	}
	if search != "" {
		pattern := "%" + search + "%"
		query = query.Where("contacts.profile_name ILIKE ? OR contacts.phone_number ILIKE ?", pattern, pattern)
	}

	var total int64
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		a.Log.Error("Failed to count tickets", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list tickets", nil, "")
	}

	var rows []ticketRow
	if err := query.Select(ticketRowSelect).
		Order("tickets.updated_at DESC").
		Limit(pg.Limit).Offset(pg.Offset).
		Scan(&rows).Error; err != nil {
		a.Log.Error("Failed to list tickets", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list tickets", nil, "")
	}

	result := make([]TicketResponse, len(rows))
	for i := range rows {
		result[i] = ticketRowToResponse(&rows[i])
	}

	return r.SendEnvelope(listEnvelope("tickets", result, total, pg))
}

// GetTicket returns a single ticket by its ID.
func (a *App) GetTicket(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceTickets, models.ActionRead)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "ticket")
	if err != nil {
		return nil
	}

	row, err := a.fetchTicketRow(orgID, "tickets.id = ?", id)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Ticket not found", nil, "")
	}

	return r.SendEnvelope(ticketRowToResponse(row))
}

// GetOrCreateContactTicket returns the ticket for a contact, creating one if
// it doesn't exist yet. This is how callers obtain a ticket ID for a
// conversation they're viewing.
func (a *App) GetOrCreateContactTicket(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTickets, models.ActionRead)
	if err != nil {
		return nil
	}

	contactID, err := parsePathUUID(r, "id", "contact")
	if err != nil {
		return nil
	}

	if _, err := findByIDAndOrg[models.Contact](a.DB, r, contactID, orgID, "Contact"); err != nil {
		return nil
	}

	if _, err := a.getOrCreateTicket(orgID, contactID, &userID); err != nil {
		a.Log.Error("Failed to get or create ticket", "error", err, "contact_id", contactID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load ticket", nil, "")
	}

	row, err := a.fetchTicketRow(orgID, "tickets.contact_id = ? AND tickets.status != ?", contactID, models.TicketStatusClosed)
	if err != nil {
		a.Log.Error("Failed to fetch ticket after get-or-create", "error", err, "contact_id", contactID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load ticket", nil, "")
	}

	return r.SendEnvelope(ticketRowToResponse(row))
}

// AssignTicketRequest is the request body for assigning a ticket.
type AssignTicketRequest struct {
	UserID string `json:"user_id"`
}

// AssignTicket assigns (or reassigns) a ticket to a user.
func (a *App) AssignTicket(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTickets, models.ActionAssign)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "ticket")
	if err != nil {
		return nil
	}

	var req AssignTicketRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	assigneeID, err := uuid.Parse(req.UserID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "user_id is required", nil, "")
	}
	if err := a.findOrgUser(orgID, assigneeID); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "User not found", nil, "")
	}

	ticket, err := findByIDAndOrg[models.Ticket](a.DB, r, id, orgID, "Ticket")
	if err != nil {
		return nil
	}
	if ticket.Status == models.TicketStatusClosed {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Cannot assign a closed ticket. Reopen it first.", nil, "")
	}

	oldSnap := ticketAuditSnapshot(ticket)

	now := time.Now()
	ticket.AssignedUserID = &assigneeID
	ticket.AssignedAt = &now
	ticket.Status = models.TicketStatusAssigned

	if err := a.DB.Save(ticket).Error; err != nil {
		a.Log.Error("Failed to assign ticket", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to assign ticket", nil, "")
	}

	a.logAudit(orgID, userID, "ticket", ticket.ID, models.AuditActionAssigned, oldSnap, ticketAuditSnapshot(ticket))
	a.broadcastTicketEvent(ticket, websocket.TypeTicketAssigned, userID, nil)
	a.recordTicketEvent(ticket, models.AuditActionAssigned, userID, &assigneeID, "")

	row, _ := a.fetchTicketRow(orgID, "tickets.id = ?", ticket.ID)
	return r.SendEnvelope(ticketRowToResponse(row))
}

// TransferTicketRequest is the request body for transferring a ticket.
type TransferTicketRequest struct {
	UserID string `json:"user_id"`
	Note   string `json:"note"`
}

// TransferTicket moves an already-assigned ticket to a different user.
func (a *App) TransferTicket(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTickets, models.ActionAssign)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "ticket")
	if err != nil {
		return nil
	}

	var req TransferTicketRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	assigneeID, err := uuid.Parse(req.UserID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "user_id is required", nil, "")
	}
	if err := a.findOrgUser(orgID, assigneeID); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "User not found", nil, "")
	}

	ticket, err := findByIDAndOrg[models.Ticket](a.DB, r, id, orgID, "Ticket")
	if err != nil {
		return nil
	}
	if ticket.Status != models.TicketStatusAssigned {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Ticket is not currently assigned; use assign instead", nil, "")
	}

	oldSnap := ticketAuditSnapshot(ticket)

	now := time.Now()
	ticket.AssignedUserID = &assigneeID
	ticket.AssignedAt = &now

	if err := a.DB.Save(ticket).Error; err != nil {
		a.Log.Error("Failed to transfer ticket", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to transfer ticket", nil, "")
	}

	extraChanges := map[string]any{}
	if req.Note != "" {
		extraChanges["note"] = req.Note
	}
	a.logAudit(orgID, userID, "ticket", ticket.ID, models.AuditActionTransferred, oldSnap, ticketAuditSnapshot(ticket), extraChanges)

	var wsExtra map[string]any
	if req.Note != "" {
		wsExtra = map[string]any{"note": req.Note}
	}
	a.broadcastTicketEvent(ticket, websocket.TypeTicketTransferred, userID, wsExtra)
	a.recordTicketEvent(ticket, models.AuditActionTransferred, userID, &assigneeID, req.Note)

	row, _ := a.fetchTicketRow(orgID, "tickets.id = ?", ticket.ID)
	return r.SendEnvelope(ticketRowToResponse(row))
}

// UnassignTicket clears a ticket's assignee.
func (a *App) UnassignTicket(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTickets, models.ActionAssign)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "ticket")
	if err != nil {
		return nil
	}

	ticket, err := findByIDAndOrg[models.Ticket](a.DB, r, id, orgID, "Ticket")
	if err != nil {
		return nil
	}
	if ticket.Status == models.TicketStatusClosed {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Cannot unassign a closed ticket", nil, "")
	}

	oldSnap := ticketAuditSnapshot(ticket)

	ticket.AssignedUserID = nil
	ticket.AssignedAt = nil
	ticket.Status = models.TicketStatusUnassigned

	if err := a.DB.Save(ticket).Error; err != nil {
		a.Log.Error("Failed to unassign ticket", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to unassign ticket", nil, "")
	}

	a.logAudit(orgID, userID, "ticket", ticket.ID, models.AuditActionUnassigned, oldSnap, ticketAuditSnapshot(ticket))
	a.broadcastTicketEvent(ticket, websocket.TypeTicketUnassigned, userID, nil)
	a.recordTicketEvent(ticket, models.AuditActionUnassigned, userID, nil, "")

	row, _ := a.fetchTicketRow(orgID, "tickets.id = ?", ticket.ID)
	return r.SendEnvelope(ticketRowToResponse(row))
}

// CloseTicketRequest is the request body for closing a ticket.
type CloseTicketRequest struct {
	Note string `json:"note"`
}

// CloseTicket closes a ticket and clears its scratch attributes.
func (a *App) CloseTicket(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTickets, models.ActionWrite)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "ticket")
	if err != nil {
		return nil
	}

	var req CloseTicketRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	ticket, err := findByIDAndOrg[models.Ticket](a.DB, r, id, orgID, "Ticket")
	if err != nil {
		return nil
	}
	if ticket.Status == models.TicketStatusClosed {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Ticket is already closed", nil, "")
	}

	oldSnap := ticketAuditSnapshot(ticket)

	now := time.Now()
	ticket.Status = models.TicketStatusClosed
	ticket.ClosedByUserID = &userID
	ticket.ClosedAt = &now
	ticket.Attributes = models.JSONB{}

	if err := a.DB.Save(ticket).Error; err != nil {
		a.Log.Error("Failed to close ticket", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to close ticket", nil, "")
	}

	extraChanges := map[string]any{}
	if req.Note != "" {
		extraChanges["note"] = req.Note
	}
	a.logAudit(orgID, userID, "ticket", ticket.ID, models.AuditActionClosed, oldSnap, ticketAuditSnapshot(ticket), extraChanges)

	var wsExtra map[string]any
	if req.Note != "" {
		wsExtra = map[string]any{"note": req.Note}
	}
	a.broadcastTicketEvent(ticket, websocket.TypeTicketClosed, userID, wsExtra)
	a.recordTicketEvent(ticket, models.AuditActionClosed, userID, nil, req.Note)

	row, _ := a.fetchTicketRow(orgID, "tickets.id = ?", ticket.ID)
	return r.SendEnvelope(ticketRowToResponse(row))
}

// ReopenTicket reopens a closed ticket. It returns to "assigned" if it still
// has an assignee, otherwise "unassigned". Attributes remain empty — a
// reopened ticket starts its scratch data fresh.
func (a *App) ReopenTicket(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTickets, models.ActionWrite)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "ticket")
	if err != nil {
		return nil
	}

	ticket, err := findByIDAndOrg[models.Ticket](a.DB, r, id, orgID, "Ticket")
	if err != nil {
		return nil
	}
	if ticket.Status != models.TicketStatusClosed {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Only closed tickets can be reopened", nil, "")
	}

	oldSnap := ticketAuditSnapshot(ticket)

	now := time.Now()
	ticket.ReopenedAt = &now
	ticket.ClosedAt = nil
	ticket.ClosedByUserID = nil
	if ticket.AssignedUserID != nil {
		ticket.Status = models.TicketStatusAssigned
	} else {
		ticket.Status = models.TicketStatusUnassigned
	}

	if err := a.DB.Save(ticket).Error; err != nil {
		a.Log.Error("Failed to reopen ticket", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to reopen ticket", nil, "")
	}

	a.logAudit(orgID, userID, "ticket", ticket.ID, models.AuditActionReopened, oldSnap, ticketAuditSnapshot(ticket))
	a.broadcastTicketEvent(ticket, websocket.TypeTicketReopened, userID, nil)
	a.recordTicketEvent(ticket, models.AuditActionReopened, userID, nil, "")

	row, _ := a.fetchTicketRow(orgID, "tickets.id = ?", ticket.ID)
	return r.SendEnvelope(ticketRowToResponse(row))
}

// UpdateTicketAttributesRequest is the request body for merging ticket attributes.
// A key mapped to a JSON null deletes that key.
type UpdateTicketAttributesRequest struct {
	Attributes map[string]any `json:"attributes"`
}

// ListTicketActivities returns all ticket activity logs for the organization
// with optional filters: ticket_id, contact_id, actor_user_id, action.
func (a *App) ListTicketActivities(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceTickets, models.ActionRead)
	if err != nil {
		return nil
	}

	pg := parsePagination(r)

	query := a.DB.Where("organization_id = ?", orgID)

	if v := string(r.RequestCtx.QueryArgs().Peek("ticket_id")); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid ticket_id", nil, "")
		}
		query = query.Where("ticket_id = ?", id)
	}
	if v := string(r.RequestCtx.QueryArgs().Peek("contact_id")); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact_id", nil, "")
		}
		query = query.Where("contact_id = ?", id)
	}
	if v := string(r.RequestCtx.QueryArgs().Peek("actor_user_id")); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid actor_user_id", nil, "")
		}
		query = query.Where("actor_user_id = ?", id)
	}
	if v := string(r.RequestCtx.QueryArgs().Peek("action")); v != "" {
		query = query.Where("action = ?", v)
	}

	var total int64
	if err := query.Model(&models.TicketActivity{}).Session(&gorm.Session{}).Count(&total).Error; err != nil {
		a.Log.Error("Failed to count ticket activities", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list activities", nil, "")
	}

	var activities []models.TicketActivity
	if err := query.Order("created_at DESC").
		Limit(pg.Limit).Offset(pg.Offset).
		Find(&activities).Error; err != nil {
		a.Log.Error("Failed to list ticket activities", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list activities", nil, "")
	}

	return r.SendEnvelope(listEnvelope("activities", activities, total, pg))
}

// GetTicketActivities returns the structured activity log for a ticket,
// ordered oldest-first so the frontend can render a chronological timeline.
func (a *App) GetTicketActivities(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceTickets, models.ActionRead)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "ticket")
	if err != nil {
		return nil
	}

	if _, err := findByIDAndOrg[models.Ticket](a.DB, r, id, orgID, "Ticket"); err != nil {
		return nil
	}

	var activities []models.TicketActivity
	if err := a.DB.
		Where("ticket_id = ? AND organization_id = ?", id, orgID).
		Order("created_at ASC").
		Find(&activities).Error; err != nil {
		a.Log.Error("Failed to fetch ticket activities", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to fetch activities", nil, "")
	}

	return r.SendEnvelope(activities)
}

// UpdateTicketAttributes shallow-merges key/value scratch data into a
// ticket's Attributes bag. This is how chatbot flow nodes / agents persist
// data across a ticket's lifetime without it leaking into the next ticket.
func (a *App) UpdateTicketAttributes(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTickets, models.ActionWrite)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "ticket")
	if err != nil {
		return nil
	}

	var req UpdateTicketAttributesRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	ticket, err := findByIDAndOrg[models.Ticket](a.DB, r, id, orgID, "Ticket")
	if err != nil {
		return nil
	}
	if ticket.Status == models.TicketStatusClosed {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Cannot update attributes on a closed ticket", nil, "")
	}

	oldSnap := ticketAuditSnapshot(ticket)

	if ticket.Attributes == nil {
		ticket.Attributes = models.JSONB{}
	}
	for k, v := range req.Attributes {
		if v == nil {
			delete(ticket.Attributes, k)
			continue
		}
		ticket.Attributes[k] = v
	}

	if err := a.DB.Model(ticket).Update("attributes", ticket.Attributes).Error; err != nil {
		a.Log.Error("Failed to update ticket attributes", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update ticket attributes", nil, "")
	}

	a.logAudit(orgID, userID, "ticket", ticket.ID, models.AuditActionUpdated, oldSnap, ticketAuditSnapshot(ticket))

	row, _ := a.fetchTicketRow(orgID, "tickets.id = ?", ticket.ID)
	return r.SendEnvelope(ticketRowToResponse(row))
}
