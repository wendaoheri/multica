package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Workspace-level audit actions recorded in activity_log with a NULL
// issue_id, following the agent_env_revealed / agent_env_updated
// precedent (server/internal/handler/agent_env.go). These rows are
// forensic-only: they never appear in the issue timeline and are not
// broadcast over realtime.
//
// DeleteWorkspace is intentionally NOT in this list: the workspace
// teardown deletes the workspace's activity_log rows in the same
// transaction, so an audit row written there would be erased by the
// very operation it records. Workspace deletion is instead recorded as
// a structured application-log audit line (see DeleteWorkspace), which
// survives off-server via log shipping (PER-284).
const (
	auditActionMemberRoleChanged  = "member_role_changed"
	auditActionMemberRemoved      = "member_removed"
	auditActionInvitationCreated  = "invitation_created"
	auditActionInvitationRevoked  = "invitation_revoked"
	auditActionAgentArchived      = "agent_archived"
	auditActionAgentRestored      = "agent_restored"
	auditActionIssuesBatchUpdated = "issues_batch_updated"
	auditActionIssuesBatchDeleted = "issues_batch_deleted"
	auditActionBulkExport         = "bulk_export"
)

// bulkExportAuditWindow bounds how often one actor can produce a new
// bulk_export audit row per workspace. The client-side CSV export walks
// POST /api/issues/table/rows page by page (up to 100 rows per page),
// so an unaudited-by-default endpoint would emit one row per page; the
// window collapses a whole export session into a single row. A pair of
// racing requests can both pass the dedupe probe and write two rows —
// the probe is volume control for a detective control, not an
// exactly-once guarantee.
const bulkExportAuditWindow = 30 * time.Minute

// auditDetailsIDCap bounds the number of issue ids embedded in batch
// audit details. Batches can target hundreds of issues; the audit row
// must stay a small, bounded JSON document. The count fields carry the
// exact totals either way.
const auditDetailsIDCap = 50

// recordAudit writes one workspace-level (issue-less) audit row to
// activity_log, best-effort: a failed audit write is logged loudly but
// never fails the underlying operation. These are detective controls —
// the mutation they describe has already committed, and refusing the
// mutation because the audit trail is unavailable would trade a
// logging outage for a product outage. (Contrast GetAgentEnv, which is
// fail-closed because it guards a preventive promise: never serve
// secrets without a recorded reveal.)
//
// actorType must be one of member/agent/system (activity_log CHECK);
// actorID must parse as a UUID, otherwise the row is skipped with a
// warning rather than panicking inside the handler.
func (h *Handler) recordAudit(r *http.Request, workspaceID pgtype.UUID, actorType, actorID, action string, details map[string]any) {
	actorUUID, err := util.ParseUUID(actorID)
	if err != nil {
		slog.Warn("audit skipped: unparseable actor id",
			append(logger.RequestAttrs(r), "action", action, "actor_id", actorID, "error", err)...)
		return
	}
	if details == nil {
		details = map[string]any{}
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		slog.Warn("audit skipped: details marshal failed",
			append(logger.RequestAttrs(r), "action", action, "error", err)...)
		return
	}
	if _, err := h.Queries.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: workspaceID,
		IssueID:     pgtype.UUID{}, // workspace-level audit, not tied to an issue
		ActorType:   pgtype.Text{String: actorType, Valid: true},
		ActorID:     actorUUID,
		Action:      action,
		Details:     detailsJSON,
	}); err != nil {
		slog.Warn("audit write failed",
			append(logger.RequestAttrs(r), "action", action, "workspace_id", uuidToString(workspaceID), "error", err)...)
	}
}

// recordBulkExportAudit records a deduplicated bulk_export audit row for
// high-volume read surfaces (the unbounded open_only list branch and the
// table-rows endpoint that backs client-side CSV export). At most one row
// per actor per workspace per bulkExportAuditWindow; subsequent requests
// inside the window are silent. Best-effort like recordAudit.
func (h *Handler) recordBulkExportAudit(r *http.Request, workspaceID pgtype.UUID, endpoint string, details map[string]any) {
	userID := requestUserID(r)
	actorType, actorID := h.resolveActor(r, userID, uuidToString(workspaceID))
	actorUUID, err := util.ParseUUID(actorID)
	if err != nil {
		slog.Warn("bulk_export audit skipped: unparseable actor id",
			append(logger.RequestAttrs(r), "actor_id", actorID, "error", err)...)
		return
	}
	recent, err := h.Queries.ExistsRecentActivityByActorAction(r.Context(), db.ExistsRecentActivityByActorActionParams{
		WorkspaceID: workspaceID,
		ActorID:     actorUUID,
		Action:      auditActionBulkExport,
		CreatedAt:   pgtype.Timestamptz{Time: time.Now().UTC().Add(-bulkExportAuditWindow), Valid: true},
	})
	if err != nil {
		slog.Warn("bulk_export audit dedupe probe failed",
			append(logger.RequestAttrs(r), "workspace_id", uuidToString(workspaceID), "error", err)...)
		return
	}
	if recent {
		return
	}
	if details == nil {
		details = map[string]any{}
	}
	details["endpoint"] = endpoint
	h.recordAudit(r, workspaceID, actorType, actorID, auditActionBulkExport, details)
}
