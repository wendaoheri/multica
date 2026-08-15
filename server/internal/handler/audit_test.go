package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// deleteWorkspaceAuditRows removes every activity_log row the tests in
// this file may have written for the shared fixture workspace. Tests
// also register it as a cleanup; running it at the START of
// dedupe-sensitive tests keeps them independent of execution order
// (bulk_export dedupe looks back 30 minutes across the whole table).
func deleteWorkspaceAuditRows(t *testing.T, actions ...string) {
	t.Helper()
	ctx := context.Background()
	for _, action := range actions {
		if _, err := testPool.Exec(ctx,
			`DELETE FROM activity_log WHERE workspace_id = $1 AND action = $2`,
			testWorkspaceID, action,
		); err != nil {
			t.Fatalf("clean audit rows for %q: %v", action, err)
		}
	}
}

func countAuditRows(t *testing.T, action string) int {
	t.Helper()
	var count int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM activity_log WHERE workspace_id = $1 AND action = $2`,
		testWorkspaceID, action,
	).Scan(&count); err != nil {
		t.Fatalf("count audit rows for %q: %v", action, err)
	}
	return count
}

func auditRowDetails(t *testing.T, action string) map[string]any {
	t.Helper()
	var raw []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT details FROM activity_log WHERE workspace_id = $1 AND action = $2 ORDER BY created_at DESC LIMIT 1`,
		testWorkspaceID, action,
	).Scan(&raw); err != nil {
		t.Fatalf("load audit details for %q: %v", action, err)
	}
	var details map[string]any
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatalf("unmarshal audit details for %q: %v", action, err)
	}
	return details
}

// seedAuditTestMember inserts a fresh user + member row with the given
// role and returns the member id. Cleaned up automatically.
func seedAuditTestMember(t *testing.T, email, role string) string {
	t.Helper()
	ctx := context.Background()

	var userID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id
	`, "Audit Test Member", email).Scan(&userID); err != nil {
		t.Fatalf("insert audit test user: %v", err)
	}

	var memberID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, $3) RETURNING id
	`, testWorkspaceID, userID, role).Scan(&memberID); err != nil {
		t.Fatalf("insert audit test member: %v", err)
	}

	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE id = $1`, memberID)
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID)
	})
	return memberID
}

func TestUpdateMemberRecordsRoleChangeAudit(t *testing.T) {
	deleteWorkspaceAuditRows(t, auditActionMemberRoleChanged)
	memberID := seedAuditTestMember(t, "audit-role-change@multica.ai", "member")

	w := httptest.NewRecorder()
	req := newRequest("PATCH", "/api/workspaces/"+testWorkspaceID+"/members/"+memberID, map[string]any{
		"role": "admin",
	})
	req = withURLParams(req, "id", testWorkspaceID, "memberId", memberID)
	testHandler.UpdateMember(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateMember: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() { deleteWorkspaceAuditRows(t, auditActionMemberRoleChanged) })

	if got := countAuditRows(t, auditActionMemberRoleChanged); got != 1 {
		t.Fatalf("expected exactly 1 member_role_changed audit row, got %d", got)
	}
	details := auditRowDetails(t, auditActionMemberRoleChanged)
	if details["member_id"] != memberID {
		t.Fatalf("audit member_id = %v, want %s", details["member_id"], memberID)
	}
	if details["from_role"] != "member" || details["to_role"] != "admin" {
		t.Fatalf("audit roles = %v -> %v, want member -> admin", details["from_role"], details["to_role"])
	}
}

func TestDeleteMemberRecordsRemovalAudit(t *testing.T) {
	deleteWorkspaceAuditRows(t, auditActionMemberRemoved)
	memberID := seedAuditTestMember(t, "audit-removal@multica.ai", "member")

	w := httptest.NewRecorder()
	req := newRequest("DELETE", "/api/workspaces/"+testWorkspaceID+"/members/"+memberID, nil)
	req = withURLParams(req, "id", testWorkspaceID, "memberId", memberID)
	testHandler.DeleteMember(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteMember: expected 204, got %d: %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() { deleteWorkspaceAuditRows(t, auditActionMemberRemoved) })

	if got := countAuditRows(t, auditActionMemberRemoved); got != 1 {
		t.Fatalf("expected exactly 1 member_removed audit row, got %d", got)
	}
	details := auditRowDetails(t, auditActionMemberRemoved)
	if details["member_id"] != memberID || details["role"] != "member" {
		t.Fatalf("unexpected member_removed details: %v", details)
	}
}

func TestInvitationLifecycleRecordsAudit(t *testing.T) {
	deleteWorkspaceAuditRows(t, auditActionInvitationCreated, auditActionInvitationRevoked)

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/workspaces/"+testWorkspaceID+"/members", map[string]any{
		"email": "audit-invitee@multica.ai",
		"role":  "member",
	})
	req = withURLParam(req, "id", testWorkspaceID)
	testHandler.CreateInvitation(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateInvitation: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var inv InvitationResponse
	json.NewDecoder(w.Body).Decode(&inv)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM workspace_invitation WHERE id = $1`, inv.ID)
		deleteWorkspaceAuditRows(t, auditActionInvitationCreated, auditActionInvitationRevoked)
	})

	if got := countAuditRows(t, auditActionInvitationCreated); got != 1 {
		t.Fatalf("expected 1 invitation_created audit row, got %d", got)
	}
	created := auditRowDetails(t, auditActionInvitationCreated)
	if created["invitation_id"] != inv.ID || created["invitee_email"] != "audit-invitee@multica.ai" || created["role"] != "member" {
		t.Fatalf("unexpected invitation_created details: %v", created)
	}

	w = httptest.NewRecorder()
	req = newRequest("DELETE", "/api/workspaces/"+testWorkspaceID+"/invitations/"+inv.ID, nil)
	req = withURLParams(req, "id", testWorkspaceID, "invitationId", inv.ID)
	testHandler.RevokeInvitation(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("RevokeInvitation: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	if got := countAuditRows(t, auditActionInvitationRevoked); got != 1 {
		t.Fatalf("expected 1 invitation_revoked audit row, got %d", got)
	}
	revoked := auditRowDetails(t, auditActionInvitationRevoked)
	if revoked["invitation_id"] != inv.ID || revoked["invitee_email"] != "audit-invitee@multica.ai" {
		t.Fatalf("unexpected invitation_revoked details: %v", revoked)
	}
}

func TestAgentArchiveRestoreRecordsAudit(t *testing.T) {
	deleteWorkspaceAuditRows(t, auditActionAgentArchived, auditActionAgentRestored)
	agentID := createHandlerTestAgent(t, "Audit Archive Agent", nil)

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/agents/"+agentID+"/archive", nil)
	req = withURLParam(req, "id", agentID)
	testHandler.ArchiveAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ArchiveAgent: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() { deleteWorkspaceAuditRows(t, auditActionAgentArchived, auditActionAgentRestored) })

	if got := countAuditRows(t, auditActionAgentArchived); got != 1 {
		t.Fatalf("expected 1 agent_archived audit row, got %d", got)
	}
	archived := auditRowDetails(t, auditActionAgentArchived)
	if archived["agent_id"] != agentID || archived["agent_name"] != "Audit Archive Agent" {
		t.Fatalf("unexpected agent_archived details: %v", archived)
	}

	w = httptest.NewRecorder()
	req = newRequest("POST", "/api/agents/"+agentID+"/restore", nil)
	req = withURLParam(req, "id", agentID)
	testHandler.RestoreAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("RestoreAgent: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if got := countAuditRows(t, auditActionAgentRestored); got != 1 {
		t.Fatalf("expected 1 agent_restored audit row, got %d", got)
	}
}

func createAuditTestIssue(t *testing.T, title string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  title,
		"status": "todo",
	})
	testHandler.CreateIssue(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateIssue: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created IssueResponse
	json.NewDecoder(w.Body).Decode(&created)
	return created.ID
}

func TestBatchDeleteIssuesRecordsAudit(t *testing.T) {
	deleteWorkspaceAuditRows(t, auditActionIssuesBatchDeleted)
	first := createAuditTestIssue(t, "Audit batch delete 1")
	second := createAuditTestIssue(t, "Audit batch delete 2")

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues/batch-delete?workspace_id="+testWorkspaceID, map[string]any{
		"issue_ids": []string{first, second},
	})
	testHandler.BatchDeleteIssues(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("BatchDeleteIssues: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() { deleteWorkspaceAuditRows(t, auditActionIssuesBatchDeleted) })

	if got := countAuditRows(t, auditActionIssuesBatchDeleted); got != 1 {
		t.Fatalf("expected 1 issues_batch_deleted audit row, got %d", got)
	}
	details := auditRowDetails(t, auditActionIssuesBatchDeleted)
	if details["requested"] != float64(2) || details["deleted"] != float64(2) {
		t.Fatalf("unexpected batch delete counts: %v", details)
	}
	ids, _ := details["issue_ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("expected 2 sampled issue ids, got %v", details["issue_ids"])
	}
}

func TestBatchUpdateIssuesRecordsAudit(t *testing.T) {
	deleteWorkspaceAuditRows(t, auditActionIssuesBatchUpdated)
	issueID := createAuditTestIssue(t, "Audit batch update")
	t.Cleanup(func() {
		del := newRequest("DELETE", "/api/issues/"+issueID, nil)
		del = withURLParam(del, "id", issueID)
		testHandler.DeleteIssue(httptest.NewRecorder(), del)
		deleteWorkspaceAuditRows(t, auditActionIssuesBatchUpdated)
	})

	newStatus := "in_progress"
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues/batch-update?workspace_id="+testWorkspaceID, map[string]any{
		"issue_ids": []string{issueID},
		"updates":   map[string]any{"status": newStatus},
	})
	testHandler.BatchUpdateIssues(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("BatchUpdateIssues: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if got := countAuditRows(t, auditActionIssuesBatchUpdated); got != 1 {
		t.Fatalf("expected 1 issues_batch_updated audit row, got %d", got)
	}
	details := auditRowDetails(t, auditActionIssuesBatchUpdated)
	if details["updated"] != float64(1) {
		t.Fatalf("unexpected batch update details: %v", details)
	}
	fields, _ := details["fields"].([]any)
	if len(fields) != 1 || fields[0] != "status" {
		t.Fatalf("expected fields=[status], got %v", details["fields"])
	}
}

func TestOpenOnlyListRecordsDeduplicatedBulkExportAudit(t *testing.T) {
	deleteWorkspaceAuditRows(t, auditActionBulkExport)
	t.Cleanup(func() { deleteWorkspaceAuditRows(t, auditActionBulkExport) })

	call := func() {
		w := httptest.NewRecorder()
		req := newRequest("GET", "/api/issues?open_only=true&workspace_id="+testWorkspaceID, nil)
		testHandler.ListIssues(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("ListIssues open_only: expected 200, got %d: %s", w.Code, w.Body.String())
		}
	}

	call()
	if got := countAuditRows(t, auditActionBulkExport); got != 1 {
		t.Fatalf("expected 1 bulk_export audit row after first call, got %d", got)
	}
	details := auditRowDetails(t, auditActionBulkExport)
	if details["endpoint"] != "GET /api/issues?open_only=true" {
		t.Fatalf("unexpected bulk_export endpoint: %v", details["endpoint"])
	}

	// Second call inside the dedupe window stays silent.
	call()
	if got := countAuditRows(t, auditActionBulkExport); got != 1 {
		t.Fatalf("bulk_export audit must dedupe within the window, got %d rows", got)
	}
}

func TestRecordAuditSkipsUnparseableActor(t *testing.T) {
	deleteWorkspaceAuditRows(t, "audit_test_bad_actor")
	req := newRequest("GET", "/api/health", nil)
	// Must not panic and must not write a row for an invalid actor id.
	testHandler.recordAudit(req, parseUUID(testWorkspaceID), "member", "not-a-uuid", "audit_test_bad_actor", nil)
	var count int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM activity_log WHERE workspace_id = $1 AND action = $2`,
		testWorkspaceID, "audit_test_bad_actor",
	).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no audit row for unparseable actor, got %d", count)
	}
}
