package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/go-i2p/onramp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

type Config struct {
	TunnelName          string
	SAMAddress          string
	RateLimit           float64
	BurstLimit          int
	LogLevel            string
	MetricsAddr         string
	DenyPrivateNetworks bool
	DenyLocalhost       bool
}

type Proxy struct {
	config  *Config
	proxy   *goproxy.ProxyHttpServer
	garlic  *onramp.Garlic
	logger  *logrus.Logger
	limiter *rate.Limiter
	metrics *ProxyMetrics
}

// ProxyMetrics handles Prometheus metrics
type ProxyMetrics struct {
	requestsTotal     prometheus.Counter
	requestDuration   prometheus.Histogram
	activeConnections prometheus.Gauge
}

func NewProxy(config *Config) (*Proxy, error) {
	// Initialize logger
	logger := logrus.New()
	level, err := logrus.ParseLevel(config.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("invalid log level: %w", err)
	}
	logger.SetLevel(level)

	// Initialize goproxy
	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = logger.Level == logrus.DebugLevel
	proxy.Logger = logger

	// Initialize I2P connection
	garlic, err := onramp.NewGarlic(config.TunnelName, config.SAMAddress, onramp.OPT_DEFAULTS)
	if err != nil {
		return nil, fmt.Errorf("failed to create I2P tunnel: %w", err)
	}

	// Initialize rate limiter
	limiter := rate.NewLimiter(rate.Limit(config.RateLimit), config.BurstLimit)

	// Initialize metrics
	metrics := newProxyMetrics()

	p := &Proxy{
		config:  config,
		proxy:   proxy,
		garlic:  garlic,
		logger:  logger,
		limiter: limiter,
		metrics: metrics,
	}

	p.setupMiddleware()
	return p, nil
}

func (p *Proxy) setupMiddleware() {
	var isPrivate goproxy.ReqConditionFunc = func(req *http.Request, _ *goproxy.ProxyCtx) bool {
		h := req.URL.Hostname()
		if ip := net.ParseIP(h); ip != nil {
			return ip.IsPrivate()
		}

		// In case of IPv6 without a port number Hostname() sometimes returns the invalid value.
		if ip := net.ParseIP(req.URL.Host); ip != nil {
			return ip.IsPrivate()
		}

		return false
	}

	p.proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		start := time.Now()

		// Rate limiting
		if !p.limiter.Allow() {
			p.logger.WithField("remote_addr", req.RemoteAddr).Warn("rate limit exceeded")
			return nil, goproxy.NewResponse(req,
				goproxy.ContentTypeText, http.StatusTooManyRequests,
				"rate limit exceeded")
		}

		// Metrics
		p.metrics.requestsTotal.Inc()
		p.metrics.activeConnections.Inc()
		defer p.metrics.activeConnections.Dec()
		defer func() {
			p.metrics.requestDuration.Observe(time.Since(start).Seconds())
		}()

		// Logging
		p.logger.WithFields(logrus.Fields{
			"method": req.Method,
			"url":    req.URL.String(),
			"remote": req.RemoteAddr,
		}).Info("incoming request")

		// deny localhost
		if p.config.DenyLocalhost && goproxy.IsLocalHost(req, ctx) {
			return nil, goproxy.NewResponse(req,
				goproxy.ContentTypeText, http.StatusForbidden,
				"Access to local addresses is forbidden")
		}

		//deny private destinations
		if p.config.DenyPrivateNetworks && isPrivate(req, ctx) {
			return nil, goproxy.NewResponse(req,
				goproxy.ContentTypeText, http.StatusForbidden,
				"Access to private (RFC 1918) networks is forbidden")
		}

		return req, nil
	})

}

func (p *Proxy) Run(ctx context.Context) error {
	// Start metrics server
	go p.serveMetrics(ctx)

	// Get I2P listener
	listener, err := p.garlic.Listen()
	if err != nil {
		return fmt.Errorf("failed to create I2P listener: %w", err)
	}
	defer listener.Close()

	// Log I2P address
	keys, err := p.garlic.Keys()
	if err != nil {
		return fmt.Errorf("failed to get I2P keys: %w", err)
	}
	p.logger.WithField("address", keys.Addr().Base32()).Info("I2P service started")

	// Start proxy server
	server := &http.Server{
		Handler: p.proxy,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	return server.Serve(listener)
}

func main() {
	config := &Config{}
	flag.StringVar(&config.TunnelName, "tunnel", "outproxy", "I2P tunnel name")
	flag.StringVar(&config.SAMAddress, "sam", onramp.SAM_ADDR, "SAM bridge address")
	flag.Float64Var(&config.RateLimit, "rate", 10, "Requests per second per IP")
	flag.IntVar(&config.BurstLimit, "burst", 20, "Maximum burst size")
	flag.StringVar(&config.LogLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.StringVar(&config.MetricsAddr, "metrics", ":2112", "Metrics server address")
	flag.BoolVar(&config.DenyLocalhost, "deny-localhost", true, "Deny requests to localhost")
	flag.BoolVar(&config.DenyPrivateNetworks, "deny-private-networks", true, "Deny requests to private (RFC 1918) networks")
	flag.Parse()

	logger := logrus.New()

	proxy, err := NewProxy(config)
	if err != nil {
		logger.Fatal(err)
	}
	defer proxy.garlic.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	if err := proxy.Run(ctx); err != nil && err != context.Canceled {
		logger.Fatal(err)
	}
}
