package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

func (a *App) heartbeatLoop(ctx context.Context) {
	for {
		iv := time.Minute
		if hb := a.cfg.Load().Heartbeat; hb != nil {
			iv = hb.Interval.D()
			for _, u := range hb.URLs {
				go a.heartbeat(ctx, u)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(iv):
		}
	}
}

func (a *App) heartbeat(ctx context.Context, u string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if resp, err := (&http.Client{Transport: a.transport(false), Timeout: 30 * time.Second}).Do(req); err != nil {
		slog.Error("heartbeat failed", "host", req.URL.Host, "err", errText(err))
	} else {
		resp.Body.Close()
	}
}
