package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ilter-ai/ilter/internal/config"

	dashjobs "github.com/ilter-ai/ilter/internal/dashboard/jobs"
	"github.com/ilter-ai/ilter/internal/jobs"
	"github.com/ilter-ai/ilter/internal/jobs/triggers"
)

// initJobs initializes the job runner and dispatcher.
func (a *App) initJobs() {
	cfg := a.cfg
	if !cfg.Jobs.Enabled {
		return
	}

	jobStore := jobs.NewJobStore(a.store.DB)
	trigStore := triggers.NewStore(a.store.DB)

	var lock jobs.LockProvider
	if cfg.Jobs.RedisLockEnabled && a.rg != nil {
		lock = jobs.NewRedisLock(a.rg.Client())
	} else {
		lock = jobs.NewLocalLock()
	}

	pollInterval, _ := time.ParseDuration(cfg.Jobs.PollInterval)
	if pollInterval <= 0 {
		pollInterval = 30 * time.Second
	}
	retryDelay, _ := time.ParseDuration(cfg.Jobs.RetryDelayBase)
	if retryDelay <= 0 {
		retryDelay = 10 * time.Second
	}

	log := slog.Default()

	proxyURL := cfg.Jobs.ProxyURL
	if proxyURL == "" {
		port := cfg.Server.Port
		if port <= 0 {
			port = 8181
		}
		proxyURL = fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port)
	}

	a.jobRunner = jobs.NewJobRunner(jobStore, a.reg, a.mcpExecutor, lock, &jobs.RunnerConfig{
		APIKey:              cfg.Jobs.APIKey,
		DefaultBillingKeyID: cfg.Jobs.DefaultBillingKeyID,
		MaxConcurrentJobs:   cfg.Jobs.MaxConcurrentJobs,
		DefaultTimeoutMs:    cfg.Jobs.DefaultTimeoutMs,
		MaxAttempts:         3,
		ProxyURL:            proxyURL,
		PollInterval:        pollInterval,
		RetryDelayBase:      retryDelay,
	}, log)

	dispatcher := triggers.NewDispatcher(trigStore, a.jobRunner, 24*time.Hour, log)

	cronTrigger := triggers.NewCronTrigger(trigStore, log, lock)
	cronTrigger.SetFireFunc(dispatcher.FireFunc())
	if err := cronTrigger.Start(context.Background()); err != nil {
		log.Error("jobs: failed to start cron scheduler", "error", err)
	}
	a.cronTrigger = cronTrigger

	a.jobsHandler = dashjobs.NewJobsHandler(jobStore, trigStore, lock, &cfg.Jobs, log, a.auditor, dispatcher, cronTrigger)

	a.jobRunner.Reconcile(context.Background(), 3)
	a.jobRunner.StartPeriodicReconciler(context.Background())
}

// watchJobsFlag hot-applies the runtime feature:jobs toggle: disabling pauses
// the job runner (StopAccepting) and stops the cron scheduler; enabling
// resumes both without a restart. Registered after initJobs so the runner and
// cron trigger exist before any callback fires.
func (a *App) watchJobsFlag() {
	if a.jobRunner == nil && a.cronTrigger == nil {
		return
	}
	a.cfgCache.OnChange(func(snap *config.Snapshot) {
		enabled := snap.JobsEnabled
		if enabled {
			if a.jobRunner != nil {
				a.jobRunner.StartAccepting()
			}
			if a.cronTrigger != nil {
				if err := a.cronTrigger.Start(context.Background()); err != nil {
					slog.Error("jobs: failed to restart cron scheduler", "error", err)
				}
			}
			return
		}
		if a.jobRunner != nil {
			a.jobRunner.StopAccepting()
		}
		if a.cronTrigger != nil {
			a.cronTrigger.Stop()
		}
	})
}
