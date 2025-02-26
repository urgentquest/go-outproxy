package main

import (
	"context"
    "net/http"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

func newProxyMetrics() *ProxyMetrics {
    m := &ProxyMetrics{
        requestsTotal: prometheus.NewCounter(prometheus.CounterOpts{
            Name: "i2p_outproxy_requests_total",
            Help: "Total number of HTTP requests processed",
        }),
        requestDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
            Name:    "i2p_outproxy_request_duration_seconds",
            Help:    "HTTP request duration in seconds",
            Buckets: prometheus.DefBuckets,
        }),
        activeConnections: prometheus.NewGauge(prometheus.GaugeOpts{
            Name: "i2p_outproxy_active_connections",
            Help: "Number of active connections",
        }),
    }

    prometheus.MustRegister(m.requestsTotal)
    prometheus.MustRegister(m.requestDuration)
    prometheus.MustRegister(m.activeConnections)

    return m
}

func (p *Proxy) serveMetrics(ctx context.Context) error {
    server := &http.Server{
        Addr:    p.config.MetricsAddr,
        Handler: promhttp.Handler(),
    }

    go func() {
        <-ctx.Done()
        server.Shutdown(context.Background())
    }()

    return server.ListenAndServe()
}