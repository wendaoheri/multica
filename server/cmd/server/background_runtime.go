package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/scheduler"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// runBackgroundComposition owns every non-request-driven component. Keeping
// this composition in one function makes the web role structurally unable to
// start a scheduler, sweeper, queue consumer, channel lease, or media worker.
func runBackgroundComposition(
	ctx context.Context,
	pool *pgxpool.Pool,
	queries *db.Queries,
	h *handler.Handler,
	taskSvc *service.TaskService,
	autopilotSvc *service.AutopilotService,
	bus *events.Bus,
	liveness handler.LivenessStore,
	channelMediaMetrics *obsmetrics.ChannelMediaReconcilerMetrics,
) error {
	go runRuntimeSweeper(ctx, pool, queries, liveness, taskSvc, bus)
	go runAutopilotFailureMonitor(ctx, queries, bus, envFailureMonitorConfig())
	go runDBStatsLogger(ctx, pool)
	if h.WebhookDeliveryWorker != nil {
		go h.WebhookDeliveryWorker.Run(ctx)
	}
	h.PRRefresh.Start(ctx)
	if h.ChannelSupervisor != nil {
		go h.ChannelSupervisor.Run(ctx)
	}
	if h.ChannelMediaReconciler != nil {
		h.ChannelMediaReconciler.Metrics = channelMediaMetrics
		go h.ChannelMediaReconciler.Run(ctx)
	}

	schedulerMgr := scheduler.NewManager(pool, scheduler.Options{})
	if err := schedulerMgr.Register(scheduler.TaskUsageHourlyJob(pool)); err != nil {
		return err
	}
	if err := schedulerMgr.Register(scheduler.AutopilotScheduleDispatchJob(pool, queries, autopilotSvc)); err != nil {
		return err
	}
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- schedulerMgr.Run(ctx) }()

	<-ctx.Done()
	if h.WebhookDeliveryWorker != nil && !h.WebhookDeliveryWorker.WaitWithTimeout(5*time.Second) {
		slog.Warn("webhook delivery worker did not exit within drain timeout")
	}
	if h.ChannelSupervisor != nil {
		if !h.ChannelSupervisor.WaitWithTimeout(h.ChannelSupervisor.ShutdownTimeout()) {
			slog.Warn("channel supervisor did not exit within drain timeout")
		}
		if h.ChannelRouter != nil {
			drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if !h.ChannelRouter.Drain(drainCtx) {
				slog.Warn("channel router drain deadline reached")
			}
			cancel()
		}
	}
	select {
	case err := <-schedulerDone:
		return err
	case <-time.After(5 * time.Second):
		return context.DeadlineExceeded
	}
}
