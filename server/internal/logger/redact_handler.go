package logger

import (
	"context"
	"log/slog"

	"github.com/multica-ai/multica/server/pkg/redact"
)

// redactHandler wraps a slog.Handler and runs every message and string
// attribute through redact.Text before delegating. It is the structural
// guarantee behind PER-284's log shipping: shipped logs must not carry
// secrets, and scrubbing at the emission point is the only layer that
// covers every future call site without relying on each caller to
// remember. Individual leak fixes (e.g. the Google OAuth token-exchange
// log, PER-282) remain valuable as point fixes; this wrapper is the
// defense-in-depth floor beneath them.
//
// Scope and honest limits:
//   - Only string values are scrubbed. A secret encoded as a number or
//     inside a custom LogValuer's non-string output would pass through;
//     LogValuer results ARE resolved first, so a LogValuer returning a
//     string is covered.
//   - Group attributes are walked recursively.
//   - redact.Text is pattern-based (server/pkg/redact), not a parser:
//     unknown secret shapes can still appear. Shipping adds a second
//     scrub layer (ops/scripts/log-ship.sh) for the same reason.
type redactHandler struct {
	inner slog.Handler
}

// NewRedactingHandler returns a handler that scrubs secrets from every
// record before forwarding to inner.
func NewRedactingHandler(inner slog.Handler) slog.Handler {
	return &redactHandler{inner: inner}
}

func (h *redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Message = redact.Text(r.Message)
	scrubbed := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		scrubbed.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, scrubbed)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = redactAttr(a)
	}
	return &redactHandler{inner: h.inner.WithAttrs(scrubbed)}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name)}
}

// redactAttr returns a copy of a with its value scrubbed. LogValuer
// values are resolved first so lazy values cannot bypass the filter;
// group values are walked recursively. Non-string leaves are returned
// unchanged (see type doc for the honest limit).
func redactAttr(a slog.Attr) slog.Attr {
	value := a.Value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		value = slog.StringValue(redact.Text(value.String()))
	case slog.KindGroup:
		attrs := value.Group()
		scrubbed := make([]slog.Attr, len(attrs))
		for i, nested := range attrs {
			scrubbed[i] = redactAttr(nested)
		}
		value = slog.GroupValue(scrubbed...)
	}
	return slog.Attr{Key: a.Key, Value: value}
}
