package handler

import (
	"context"
	"errors"
	"testing"
)

func TestWebhookDeliveryClaimChecksOperationFenceBeforeDatabase(t *testing.T) {
	w := NewWebhookDeliveryWorker(nil)
	want := errors.New("old generation")
	w.SetOperationGuard(func(context.Context, string, bool, func(context.Context) error) error { return want })
	worked, err := w.ProcessNext(context.Background())
	if worked || !errors.Is(err, want) {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
}
