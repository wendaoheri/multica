package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/deployment"
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
	fence deployment.OperationFence,
) error {
	compositionCtx, cancelComposition := context.WithCancel(ctx)
	defer cancelComposition()
	componentErr := make(chan error, 8)
	var components sync.WaitGroup
	goComponent := func(kind string, lease bool, run func(context.Context) error) {
		components.Add(1)
		go func() {
			defer components.Done()
			var err error
			if fence != nil {
				err = fence.RunFenced(compositionCtx, kind, lease, run)
			} else {
				err = run(compositionCtx)
			}
			if err != nil && err != context.Canceled {
				select {
				case componentErr <- err:
				default:
				}
			}
		}()
	}
	goComponent("runtime-sweeper", false, func(runCtx context.Context) error {
		var guard func(context.Context, string, bool, func(context.Context) error) error
		if fence != nil {
			guard = fence.RunFenced
		}
		runRuntimeSweeperGuarded(runCtx, pool, queries, liveness, taskSvc, bus, guard)
		return runCtx.Err()
	})
	goComponent("autopilot-failure-monitor", false, func(runCtx context.Context) error {
		var guard func(context.Context, string, bool, func(context.Context) error) error
		if fence != nil {
			guard = fence.RunFenced
		}
		runAutopilotFailureMonitorGuarded(runCtx, queries, bus, envFailureMonitorConfig(), guard)
		return runCtx.Err()
	})
	goComponent("db-stats", false, func(runCtx context.Context) error {
		runDBStatsLogger(runCtx, pool)
		return runCtx.Err()
	})
	if h.WebhookDeliveryWorker != nil {
		if fence != nil {
			h.WebhookDeliveryWorker.SetOperationGuard(func(c context.Context, k string, l bool, fn func(context.Context) error) error {
				return fence.RunFenced(c, k, l, fn)
			})
		}
		goComponent("webhook-delivery", true, func(runCtx context.Context) error {
			h.WebhookDeliveryWorker.Run(runCtx)
			return runCtx.Err()
		})
	}
	goComponent("pr-refresh", true, func(runCtx context.Context) error {
		if fence != nil {
			h.PRRefresh.SetOperationGuard(func(c context.Context, k string, l bool, fn func(context.Context) error) error {
				return fence.RunFenced(c, k, l, fn)
			})
		}
		h.PRRefresh.Start(runCtx)
		<-runCtx.Done()
		h.PRRefresh.Wait()
		return runCtx.Err()
	})
	if h.ChannelSupervisor != nil {
		if fence != nil {
			h.ChannelSupervisor.SetOperationGuard(func(c context.Context, k string, l bool, fn func(context.Context) error) error {
				return fence.RunFenced(c, k, l, fn)
			})
		}
		goComponent("channel-supervisor", true, func(runCtx context.Context) error {
			h.ChannelSupervisor.Run(runCtx)
			h.ChannelSupervisor.Wait()
			return runCtx.Err()
		})
	}
	if h.ChannelMediaReconciler != nil {
		h.ChannelMediaReconciler.Metrics = channelMediaMetrics
		if fence != nil {
			h.ChannelMediaReconciler.OperationGuard = func(c context.Context, k string, l bool, fn func(context.Context) error) error {
				return fence.RunFenced(c, k, l, fn)
			}
		}
		goComponent("channel-media", true, func(runCtx context.Context) error {
			h.ChannelMediaReconciler.Run(runCtx)
			return runCtx.Err()
		})
	}

	schedulerOpts := scheduler.Options{}
	if fence != nil {
		schedulerOpts.OperationGuard = func(c context.Context, k string, l bool, fn func(context.Context) error) error {
			return fence.RunFenced(c, k, l, fn)
		}
	}
	schedulerMgr := scheduler.NewManager(pool, schedulerOpts)
	if err := schedulerMgr.Register(scheduler.TaskUsageHourlyJob(pool)); err != nil {
		return err
	}
	if err := schedulerMgr.Register(scheduler.AutopilotScheduleDispatchJob(pool, queries, autopilotSvc)); err != nil {
		return err
	}
	goComponent("scheduler", true, schedulerMgr.Run)

	select {
	case <-ctx.Done():
	case err := <-componentErr:
		cancelComposition()
		components.Wait()
		return err
	}
	cancelComposition()
	components.Wait()
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
	return ctx.Err()
}
