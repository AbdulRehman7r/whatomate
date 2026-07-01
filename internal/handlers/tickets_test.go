package handlers_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// createTestTicket creates a test ticket directly in the database.
func createTestTicket(t *testing.T, app *handlers.App, orgID, contactID uuid.UUID, status models.TicketStatus, assignedUserID *uuid.UUID) *models.Ticket {
	t.Helper()

	ticket := &models.Ticket{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		ContactID:      contactID,
		Status:         status,
		AssignedUserID: assignedUserID,
		Attributes:     models.JSONB{},
	}
	require.NoError(t, app.DB.Create(ticket).Error)
	return ticket
}

func ticketEnvelope(t *testing.T, req *fastglue.Request) handlers.TicketResponse {
	t.Helper()
	var result struct {
		Data handlers.TicketResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	return result.Data
}

func TestApp_GetOrCreateContactTicket_IsIdempotent(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	req1 := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req1, org.ID, user.ID)
	testutil.SetPathParam(req1, "id", contact.ID.String())
	require.NoError(t, app.GetOrCreateContactTicket(req1))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req1))
	first := ticketEnvelope(t, req1)
	assert.Equal(t, models.TicketStatusOpen, first.Status)
	assert.Equal(t, contact.ID, first.ContactID)

	req2 := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req2, org.ID, user.ID)
	testutil.SetPathParam(req2, "id", contact.ID.String())
	require.NoError(t, app.GetOrCreateContactTicket(req2))
	second := ticketEnvelope(t, req2)

	assert.Equal(t, first.ID, second.ID, "second call should return the same ticket, not create a new one")

	var count int64
	require.NoError(t, app.DB.Model(&models.Ticket{}).Where("contact_id = ?", contact.ID).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestApp_GetOrCreateContactTicket_ContactNotFound(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", uuid.New().String())
	require.NoError(t, app.GetOrCreateContactTicket(req))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func TestApp_TicketLifecycle_AssignTransferUnassignCloseReopen(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	agentA := createTestAgent(t, app, org.ID)
	agentB := createTestAgent(t, app, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	ticket := createTestTicket(t, app, org.ID, contact.ID, models.TicketStatusOpen, nil)

	// Assign to agentA
	assignReq := testutil.NewJSONRequest(t, map[string]any{"user_id": agentA.ID.String()})
	testutil.SetAuthContext(assignReq, org.ID, admin.ID)
	testutil.SetPathParam(assignReq, "id", ticket.ID.String())
	require.NoError(t, app.AssignTicket(assignReq))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(assignReq))
	assigned := ticketEnvelope(t, assignReq)
	assert.Equal(t, models.TicketStatusAssigned, assigned.Status)
	require.NotNil(t, assigned.AssignedUserID)
	assert.Equal(t, agentA.ID, *assigned.AssignedUserID)

	// Transfer to agentB
	transferReq := testutil.NewJSONRequest(t, map[string]any{"user_id": agentB.ID.String(), "note": "handing off"})
	testutil.SetAuthContext(transferReq, org.ID, admin.ID)
	testutil.SetPathParam(transferReq, "id", ticket.ID.String())
	require.NoError(t, app.TransferTicket(transferReq))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(transferReq))
	transferred := ticketEnvelope(t, transferReq)
	assert.Equal(t, models.TicketStatusAssigned, transferred.Status)
	require.NotNil(t, transferred.AssignedUserID)
	assert.Equal(t, agentB.ID, *transferred.AssignedUserID)

	// Unassign
	unassignReq := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(unassignReq, org.ID, admin.ID)
	testutil.SetPathParam(unassignReq, "id", ticket.ID.String())
	require.NoError(t, app.UnassignTicket(unassignReq))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(unassignReq))
	unassigned := ticketEnvelope(t, unassignReq)
	assert.Equal(t, models.TicketStatusUnassigned, unassigned.Status)
	assert.Nil(t, unassigned.AssignedUserID)

	// Re-assign then close
	reassignReq := testutil.NewJSONRequest(t, map[string]any{"user_id": agentA.ID.String()})
	testutil.SetAuthContext(reassignReq, org.ID, admin.ID)
	testutil.SetPathParam(reassignReq, "id", ticket.ID.String())
	require.NoError(t, app.AssignTicket(reassignReq))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(reassignReq))

	// Set an attribute, then close and verify it gets cleared
	attrReq := testutil.NewJSONRequest(t, map[string]any{"attributes": map[string]any{"issue_type": "billing"}})
	testutil.SetAuthContext(attrReq, org.ID, admin.ID)
	testutil.SetPathParam(attrReq, "id", ticket.ID.String())
	require.NoError(t, app.UpdateTicketAttributes(attrReq))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(attrReq))
	withAttrs := ticketEnvelope(t, attrReq)
	assert.Equal(t, "billing", withAttrs.Attributes["issue_type"])

	closeReq := testutil.NewJSONRequest(t, map[string]any{"note": "resolved"})
	testutil.SetAuthContext(closeReq, org.ID, admin.ID)
	testutil.SetPathParam(closeReq, "id", ticket.ID.String())
	require.NoError(t, app.CloseTicket(closeReq))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(closeReq))
	closed := ticketEnvelope(t, closeReq)
	assert.Equal(t, models.TicketStatusClosed, closed.Status)
	require.NotNil(t, closed.ClosedAt)
	require.NotNil(t, closed.ClosedByUserID)
	assert.Equal(t, admin.ID, *closed.ClosedByUserID)
	assert.Empty(t, closed.Attributes, "attributes should be cleared on close")

	// Reopen: should return to assigned, since AssignedUserID (agentA) is still set
	reopenReq := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(reopenReq, org.ID, admin.ID)
	testutil.SetPathParam(reopenReq, "id", ticket.ID.String())
	require.NoError(t, app.ReopenTicket(reopenReq))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(reopenReq))
	reopened := ticketEnvelope(t, reopenReq)
	assert.Equal(t, models.TicketStatusAssigned, reopened.Status)
	require.NotNil(t, reopened.AssignedUserID)
	assert.Equal(t, agentA.ID, *reopened.AssignedUserID)
	assert.Nil(t, reopened.ClosedAt)
	assert.Nil(t, reopened.ClosedByUserID)
}

func TestApp_AssignTicket_ClosedTicketRejected(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	agent := createTestAgent(t, app, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	ticket := createTestTicket(t, app, org.ID, contact.ID, models.TicketStatusClosed, nil)

	req := testutil.NewJSONRequest(t, map[string]any{"user_id": agent.ID.String()})
	testutil.SetAuthContext(req, org.ID, admin.ID)
	testutil.SetPathParam(req, "id", ticket.ID.String())
	require.NoError(t, app.AssignTicket(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_TransferTicket_RequiresAssigned(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	agent := createTestAgent(t, app, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	ticket := createTestTicket(t, app, org.ID, contact.ID, models.TicketStatusOpen, nil)

	req := testutil.NewJSONRequest(t, map[string]any{"user_id": agent.ID.String()})
	testutil.SetAuthContext(req, org.ID, admin.ID)
	testutil.SetPathParam(req, "id", ticket.ID.String())
	require.NoError(t, app.TransferTicket(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

	var result map[string]any
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, "Ticket is not currently assigned; use assign instead", result["message"])
}

func TestApp_ReopenTicket_OnlyValidFromClosed(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	ticket := createTestTicket(t, app, org.ID, contact.ID, models.TicketStatusOpen, nil)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, org.ID, admin.ID)
	testutil.SetPathParam(req, "id", ticket.ID.String())
	require.NoError(t, app.ReopenTicket(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_ListTickets_FiltersByStatusAndAssignedUser(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	agent := createTestAgent(t, app, org.ID)

	contactOpen := testutil.CreateTestContact(t, app.DB, org.ID)
	contactAssigned := testutil.CreateTestContact(t, app.DB, org.ID)

	createTestTicket(t, app, org.ID, contactOpen.ID, models.TicketStatusOpen, nil)
	assignedTicket := createTestTicket(t, app, org.ID, contactAssigned.ID, models.TicketStatusAssigned, &agent.ID)

	listReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(listReq, org.ID, admin.ID)
	testutil.SetQueryParam(listReq, "status", "assigned")
	require.NoError(t, app.ListTickets(listReq))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(listReq))

	var result struct {
		Data struct {
			Tickets []handlers.TicketResponse `json:"tickets"`
			Total   int64                     `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(listReq), &result))
	require.Len(t, result.Data.Tickets, 1)
	assert.Equal(t, assignedTicket.ID, result.Data.Tickets[0].ID)
	require.NotNil(t, result.Data.Tickets[0].AssignedUserID)
	assert.Equal(t, agent.ID, *result.Data.Tickets[0].AssignedUserID)
}

func TestApp_AssignTicket_PermissionDenied(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	// agent role has no tickets:* permissions
	role := testutil.CreateAgentRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	ticket := createTestTicket(t, app, org.ID, contact.ID, models.TicketStatusOpen, nil)

	req := testutil.NewJSONRequest(t, map[string]any{"user_id": user.ID.String()})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", ticket.ID.String())
	require.NoError(t, app.AssignTicket(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_GetOrCreateContactTicket_AfterClose_CreatesNewTicket(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	// Create and close the first ticket
	first := createTestTicket(t, app, org.ID, contact.ID, models.TicketStatusClosed, nil)
	assert.Greater(t, first.Number, int64(0), "ticket number should be auto-assigned")

	// GetOrCreate should create a brand-new ticket since the existing one is closed
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", contact.ID.String())
	require.NoError(t, app.GetOrCreateContactTicket(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	second := ticketEnvelope(t, req)

	assert.NotEqual(t, first.ID, second.ID, "a new ticket should be created, not the closed one")
	assert.Greater(t, second.Number, first.Number, "new ticket should have a higher number")
	assert.Equal(t, models.TicketStatusOpen, second.Status)

	// Both tickets should exist in DB
	var count int64
	require.NoError(t, app.DB.Model(&models.Ticket{}).Where("contact_id = ?", contact.ID).Count(&count).Error)
	assert.Equal(t, int64(2), count)
}

func TestApp_UpdateTicketAttributes_MergeAndDelete(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	ticket := createTestTicket(t, app, org.ID, contact.ID, models.TicketStatusOpen, nil)

	req1 := testutil.NewJSONRequest(t, map[string]any{
		"attributes": map[string]any{"issue_type": "billing", "priority": "high"},
	})
	testutil.SetAuthContext(req1, org.ID, admin.ID)
	testutil.SetPathParam(req1, "id", ticket.ID.String())
	require.NoError(t, app.UpdateTicketAttributes(req1))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req1))
	first := ticketEnvelope(t, req1)
	assert.Equal(t, "billing", first.Attributes["issue_type"])
	assert.Equal(t, "high", first.Attributes["priority"])

	// Delete "priority" (null), update "issue_type"
	req2 := testutil.NewJSONRequest(t, map[string]any{
		"attributes": map[string]any{"issue_type": "refund", "priority": nil},
	})
	testutil.SetAuthContext(req2, org.ID, admin.ID)
	testutil.SetPathParam(req2, "id", ticket.ID.String())
	require.NoError(t, app.UpdateTicketAttributes(req2))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req2))
	second := ticketEnvelope(t, req2)
	assert.Equal(t, "refund", second.Attributes["issue_type"])
	_, hasPriority := second.Attributes["priority"]
	assert.False(t, hasPriority, "priority should have been deleted")
}
