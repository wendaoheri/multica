package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Mention links address more than subscriber-eligible identities: issue and
// project links render as mentions, and @all / @squad broadcast mentions
// expand to recipients elsewhere. Only member/agent mentions may become
// subscriber rows — issue_subscriber.user_type is CHECK-constrained to
// ('member','agent'), and an @all mention carries the literal ID "all",
// which is not a UUID. The log assertion is load-bearing: checking only the
// subscriber table would also pass in the broken version because PostgreSQL
// rejects the invalid rows before the listener logs and moves on — while an
// @all mention panicked the UUID parse and silently skipped every subscriber
// write after it in the same event.
func TestSubscriberMentionedSkipsNonIdentityMentions(t *testing.T) {
	queries := db.New(testPool)
	bus := events.New()
	registerSubscriberListeners(bus, testPool)

	mentionedIssueID := createTestIssue(t, testWorkspaceID, testUserID)
	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupIssueSubscribers(t, issueID)
		cleanupTestIssue(t, issueID)
		cleanupTestIssue(t, mentionedIssueID)
	})

	var agentID string
	if err := testPool.QueryRow(context.Background(), `
		SELECT id::text FROM agent
		WHERE workspace_id = $1
		ORDER BY created_at ASC
		LIMIT 1
	`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("load fixture agent: %v", err)
	}

	// A member distinct from the creator: mentioning the creator would collapse
	// into the creator's row via the (issue_id, user_type, user_id) unique key.
	mentionedUserID := createTestUser(t, "subscriber-mention-type@example.com")
	t.Cleanup(func() { cleanupTestUser(t, "subscriber-mention-type@example.com") })

	// Order matters: the issue mention and the @all mention come BEFORE the
	// member and agent mentions, so the broken version both logs a constraint
	// violation and aborts the loop before the valid mentions are written.
	description := fmt.Sprintf(
		"Relates to [MUL-X](mention://issue/%s) — [@everyone](mention://all/all), "+
			"cc [@member](mention://member/%s) and [@agent](mention://agent/%s)",
		mentionedIssueID, mentionedUserID, agentID,
	)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(previousLogger)

	bus.Publish(events.Event{
		Type:        protocol.EventIssueCreated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          issueID,
				WorkspaceID: testWorkspaceID,
				Title:       "mention type test issue",
				Status:      "todo",
				Priority:    "medium",
				CreatorType: "member",
				CreatorID:   testUserID,
				Description: &description,
			},
		},
	})

	// slog's default logger is process-global. Scope the captured records to
	// this test's unique issue so unrelated background errors cannot fail it.
	var issueLogLines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "issue_id="+issueID) {
			issueLogLines = append(issueLogLines, line)
		}
	}
	issueLogs := strings.Join(issueLogLines, "\n")
	for _, unexpected := range []string{
		"failed to add issue subscriber",
		"panic in event listener",
		"SQLSTATE 23514",
	} {
		if strings.Contains(issueLogs, unexpected) {
			t.Fatalf("mention subscription attempted a non-identity write (%q):\n%s", unexpected, issueLogs)
		}
	}
	if strings.Contains(logs.String(), "panic in event listener") {
		t.Fatalf("mention subscription panicked (likely @all UUID parse):\n%s", logs.String())
	}

	if !isSubscribed(t, queries, issueID, "member", mentionedUserID) {
		t.Fatal("expected @mentioned member to be subscribed")
	}
	if !isSubscribed(t, queries, issueID, "agent", agentID) {
		t.Fatal("expected @mentioned agent to be subscribed")
	}
	// Creator + member + agent only: the issue mention and @all must not
	// produce subscriber rows.
	if count := subscriberCount(t, queries, issueID); count != 3 {
		t.Fatalf("subscriber count = %d, want 3 (creator + member + agent)", count)
	}
}

// The issue:updated path re-parses the description for NEW mentions and must
// apply the same identity filter — the daily-inspection constraint violation
// fired on an existing issue whose description was edited, not a fresh one.
func TestSubscriberMentionedOnUpdateSkipsNonIdentityMentions(t *testing.T) {
	queries := db.New(testPool)
	bus := events.New()
	registerSubscriberListeners(bus, testPool)

	mentionedIssueID := createTestIssue(t, testWorkspaceID, testUserID)
	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupIssueSubscribers(t, issueID)
		cleanupTestIssue(t, issueID)
		cleanupTestIssue(t, mentionedIssueID)
	})

	description := fmt.Sprintf("Now relates to [MUL-X](mention://issue/%s), "+
		"cc [@member](mention://member/%s)", mentionedIssueID, testUserID)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(previousLogger)

	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          issueID,
				WorkspaceID: testWorkspaceID,
				Title:       "mention type test issue",
				Status:      "todo",
				Priority:    "medium",
				CreatorType: "member",
				CreatorID:   testUserID,
				Description: &description,
			},
			"description_changed": true,
		},
	})

	var issueLogLines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "issue_id="+issueID) {
			issueLogLines = append(issueLogLines, line)
		}
	}
	issueLogs := strings.Join(issueLogLines, "\n")
	for _, unexpected := range []string{
		"failed to add issue subscriber",
		"panic in event listener",
		"SQLSTATE 23514",
	} {
		if strings.Contains(issueLogs, unexpected) {
			t.Fatalf("mention subscription on update attempted a non-identity write (%q):\n%s", unexpected, issueLogs)
		}
	}

	if !isSubscribed(t, queries, issueID, "member", testUserID) {
		t.Fatal("expected newly @mentioned member to be subscribed after issue:updated")
	}
	if count := subscriberCount(t, queries, issueID); count != 1 {
		t.Fatalf("subscriber count = %d, want 1 (mentioned member only)", count)
	}
}

// cleanupIssueSubscribers removes subscriber rows for a test issue; the table
// has no foreign key, so deleting the issue alone would orphan them.
func cleanupIssueSubscribers(t *testing.T, issueID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`DELETE FROM issue_subscriber WHERE issue_id = $1`, issueID); err != nil {
		t.Errorf("cleanup issue_subscriber: %v", err)
	}
}
