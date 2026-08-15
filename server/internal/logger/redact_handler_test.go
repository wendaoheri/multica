package logger

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

const (
	testGitHubToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	testBearerLine  = "Bearer abc123def456ghi789"
	testKeyValue    = "API_KEY=supersecretvalue123"
	testConnString  = "postgres://dbuser:dbpass@db.internal:5432/multica"
)

func newCapturingHandler(buf *bytes.Buffer) slog.Handler {
	inner := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return NewRedactingHandler(inner)
}

func assertScrubbed(t *testing.T, output string, secret string) {
	t.Helper()
	if strings.Contains(output, secret) {
		t.Fatalf("output still contains secret %q:\n%s", secret, output)
	}
}

func TestRedactingHandlerScrubsMessage(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newCapturingHandler(&buf))

	logger.Info("token exchange failed",
		"github_token", testGitHubToken,
		"auth_header", testBearerLine,
		"config", testKeyValue,
		"db", testConnString,
	)

	out := buf.String()
	assertScrubbed(t, out, testGitHubToken)
	assertScrubbed(t, out, testBearerLine)
	assertScrubbed(t, out, testKeyValue)
	assertScrubbed(t, out, "dbpass")
	if !strings.Contains(out, "[REDACTED GITHUB TOKEN]") {
		t.Fatalf("expected github token placeholder in output:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED CREDENTIAL]") {
		t.Fatalf("expected credential placeholder in output:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED CONNECTION STRING]@") {
		t.Fatalf("expected connection string placeholder in output:\n%s", out)
	}
	// Non-secret structure survives: the message and keys stay readable.
	if !strings.Contains(out, "token exchange failed") {
		t.Fatalf("message text lost in output:\n%s", out)
	}
}

func TestRedactingHandlerScrubsMessageText(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newCapturingHandler(&buf))

	logger.Error("oauth response contained " + testGitHubToken)

	out := buf.String()
	assertScrubbed(t, out, testGitHubToken)
	if !strings.Contains(out, "[REDACTED GITHUB TOKEN]") {
		t.Fatalf("expected placeholder for secret embedded in message:\n%s", out)
	}
}

func TestRedactingHandlerScrubsWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	// WithAttrs attributes must be scrubbed at attachment time, not only
	// when the record is emitted.
	logger := slog.New(newCapturingHandler(&buf)).With("token", testGitHubToken)

	logger.Info("request finished")

	out := buf.String()
	assertScrubbed(t, out, testGitHubToken)
	if !strings.Contains(out, "[REDACTED GITHUB TOKEN]") {
		t.Fatalf("expected WithAttrs secret scrubbed:\n%s", out)
	}
}

func TestRedactingHandlerScrubsGroupAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newCapturingHandler(&buf))

	logger.Info("nested",
		slog.Group("provider",
			slog.String("name", "github"),
			slog.String("access_token", testGitHubToken),
		),
	)

	out := buf.String()
	assertScrubbed(t, out, testGitHubToken)
	if !strings.Contains(out, "github") {
		t.Fatalf("non-secret group member lost in output:\n%s", out)
	}
}

func TestRedactingHandlerPreservesNonStringValues(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newCapturingHandler(&buf))

	logger.Info("counts", "status", 418, "ok", true)

	out := buf.String()
	if !strings.Contains(out, "418") || !strings.Contains(out, "true") {
		t.Fatalf("non-string values must pass through unchanged:\n%s", out)
	}
}

func TestRedactingHandlerRespectsLevelAndDelegation(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(NewRedactingHandler(inner))

	logger.Info("dropped debug-level line")
	if buf.Len() != 0 {
		t.Fatalf("wrapped handler must honor inner level filter, got:\n%s", buf.String())
	}

	logger.Warn("kept warning")
	if !strings.Contains(buf.String(), "kept warning") {
		t.Fatalf("warn record missing:\n%s", buf.String())
	}

	// Handle must propagate the inner handler's errors.
	if err := NewRedactingHandler(inner).Handle(context.Background(), slog.Record{}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
}
