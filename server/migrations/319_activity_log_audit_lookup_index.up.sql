-- Lookup index for audit-style dedupe queries on activity_log: "did this
-- actor already record this action recently in this workspace?" (see
-- ExistsRecentActivityByActorAction). activity_log is only indexed by
-- issue_id today, so without this index the dedupe probe would be a
-- sequential scan on every bulk-export request.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_activity_log_ws_actor_action_created
    ON activity_log (workspace_id, actor_id, action, created_at DESC);
