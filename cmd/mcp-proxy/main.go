package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/drawdy/MCP-Proxy/internal/backend"
	"github.com/drawdy/MCP-Proxy/internal/config"
	"github.com/drawdy/MCP-Proxy/internal/proxy"
	"github.com/drawdy/MCP-Proxy/internal/version"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func main() {
	var (
		configPath  = flag.String("config", "config.json", "path to MCP proxy JSON configuration")
		host        = flag.String("host", "127.0.0.1", "HTTP bind address")
		port        = flag.Int("port", 3006, "HTTP listen port")
		ssePath     = flag.String("sse-path", "/sse", "MCP SSE endpoint path")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, error")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	healthEndpoints := stringList{}
	corsOrigins := stringList{"*"}
	flag.Var(&healthEndpoints, "health-endpoint", "health endpoint path (repeatable)")
	flag.Var(&corsOrigins, "cors-origin", "allowed CORS origin (repeatable; default: *)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	if *port < 1 || *port > 65535 {
		fatal("port must be between 1 and 65535")
	}
	if flag.NArg() != 0 {
		fatal("unexpected positional arguments")
	}

	logger := newLogger(*logLevel)
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("configuration failed", "error", err)
		os.Exit(1)
	}

	manager := backend.NewManager(cfg, logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	manager.Start(ctx)

	handler, err := proxy.NewHandler(manager, proxy.Options{
		SSEPath:         *ssePath,
		HealthEndpoints: healthEndpoints,
		CORSOrigins:     corsOrigins,
	})
	if err != nil {
		logger.Error("HTTP handler configuration failed", "error", err)
		manager.Close()
		os.Exit(1)
	}

	address := net.JoinHostPort(*host, strconv.Itoa(*port))
	httpServer := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("MCP Proxy listening", "address", address, "sse", *ssePath, "version", version.Version)
		serverErrors <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErrors:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server stopped unexpectedly", "error", err)
			manager.Close()
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful HTTP shutdown timed out; closing active connections", "error", err)
		_ = httpServer.Close()
	}
	manager.Close()
	logger.Info("MCP Proxy stopped")
}

func newLogger(levelName string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(levelName) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		fmt.Fprintf(os.Stderr, "invalid log level %q; using info\n", levelName)
		level = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(2)
}
