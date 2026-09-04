package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	defaultConveyordShutdownTimeout = 25 * time.Second
	shutdownHardStopReserve         = 5 * time.Second
)

type shutdownHTTPServer interface {
	Shutdown(context.Context) error
}

type shutdownQueue interface {
	Stop(context.Context) error
	StopAndCancel(context.Context) error
}

type conveyordShutdown struct {
	Timeout       time.Duration
	HTTP          shutdownHTTPServer
	Queue         shutdownQueue
	CancelHTTP    context.CancelFunc
	CancelService context.CancelFunc
	CloseStore    func()
	Logf          func(string, ...any)
	now           func() time.Time
}

func resolveConveyordShutdownTimeout(flagValue time.Duration, flagExplicit bool, getenv func(string) string) (time.Duration, string, error) {
	value := flagValue
	source := "default"
	if !flagExplicit {
		if raw := getenv("CONVEYOR_SHUTDOWN_TIMEOUT"); raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil {
				return 0, "", fmt.Errorf("invalid CONVEYOR_SHUTDOWN_TIMEOUT %q: expected a positive duration: %w", raw, err)
			}
			value = parsed
			source = "CONVEYOR_SHUTDOWN_TIMEOUT"
		}
	} else {
		source = "flag"
	}
	if value <= 0 {
		return 0, "", fmt.Errorf("shutdown timeout must be a positive duration")
	}
	return value, source, nil
}

func shutdownDrainWindow(total time.Duration) time.Duration {
	reserve := shutdownHardStopReserve
	if half := total / 2; reserve > half {
		reserve = half
	}
	return total - reserve
}

func (s conveyordShutdown) Run() {
	now := s.now
	if now == nil {
		now = time.Now
	}
	logf := s.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	started := now()
	phase := func(name string, err error) {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logf("shutdown phase=%s elapsed=%s error=%v", name, now().Sub(started), err)
			return
		}
		logf("shutdown phase=%s elapsed=%s", name, now().Sub(started))
	}

	deadline := started.Add(s.Timeout)
	overallCtx, cancelOverall := context.WithDeadline(context.Background(), deadline)
	defer cancelOverall()
	if s.CancelService != nil {
		s.CancelService()
	}
	phase("admission-stopped", nil)

	drainDeadline := started.Add(shutdownDrainWindow(s.Timeout))
	drainCtx, cancelDrain := context.WithDeadline(context.Background(), drainDeadline)
	defer cancelDrain()
	queueDone := make(chan error, 1)
	if s.Queue != nil {
		go func() { queueDone <- s.Queue.Stop(drainCtx) }()
	} else {
		queueDone <- nil
	}
	phase("queue-fetch-stopped", nil)

	if s.CancelHTTP != nil {
		s.CancelHTTP()
	}
	httpDone := make(chan error, 1)
	if s.HTTP != nil {
		go func() { httpDone <- s.HTTP.Shutdown(overallCtx) }()
	} else {
		httpDone <- nil
	}
	phase("http-drain-started", nil)

	var queueErr, httpErr error
	queueFinished, httpFinished := false, false
	for !queueFinished || !httpFinished {
		select {
		case queueErr = <-queueDone:
			queueFinished = true
			queueDone = nil
		case httpErr = <-httpDone:
			httpFinished = true
			httpDone = nil
		case <-drainCtx.Done():
			goto hardStop
		}
	}

hardStop:
	cancelDrain()
	if !queueFinished {
		select {
		case queueErr = <-queueDone:
			queueFinished = true
		case <-overallCtx.Done():
			queueErr = overallCtx.Err()
		}
	}
	phase("queue-graceful-stop", queueErr)
	if s.Queue != nil {
		phase("queue-hard-stop", s.Queue.StopAndCancel(overallCtx))
	} else {
		phase("queue-hard-stop", nil)
	}
	if !httpFinished {
		select {
		case httpErr = <-httpDone:
		case <-overallCtx.Done():
			httpErr = overallCtx.Err()
		}
	}
	phase("http-drain-complete", httpErr)
	if s.CloseStore != nil {
		s.CloseStore()
	}
	phase("store-closed", nil)
}
