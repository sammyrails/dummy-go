package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	tracelit "github.com/tracelit-ai/tracelit-go"
	"github.com/tracelit-ai/tracelit-go/bridge"
	tlmiddleware "github.com/tracelit-ai/tracelit-go/middleware"
)

func main() {
	ctx := context.Background()

	// Load .env file — values are ignored if the var is already set in the
	// environment, so CI/production overrides always take precedence.
	if err := godotenv.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "no .env file found, relying on environment variables")
	}

	// ── Tracelit SDK ───────────────────────────────────────────────────────
	// That's it. One call. All traces, logs, metrics, and error reporting
	// are configured automatically from environment variables or options.
	sdk, err := tracelit.New(
		tracelit.WithAPIKey(mustEnv("TRACELIT_API_KEY")),
		tracelit.WithServiceName(getEnv("TRACELIT_SERVICE_NAME", "products-crud-api")),
		tracelit.WithEnvironment(getEnv("APP_ENV", "development")),
		tracelit.WithEndpoint(mustEnv("TRACELIT_ENDPOINT")),
		tracelit.WithSampleRate(1.0),
		tracelit.WithResourceAttributes(map[string]string{
			"service.version": "1.0.0",
			"team":            "platform",
		}),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracelit init failed: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sdk.Shutdown(shutCtx); err != nil {
			fmt.Fprintf(os.Stderr, "tracelit shutdown error: %v\n", err)
		}
	}()

	// ── Structured logging ─────────────────────────────────────────────────
	// Logs go to both stderr (human-readable) and the Tracelit OTLP pipeline.
	// The bridge correlates every log with the active trace/span automatically.
	textHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	otelHandler := bridge.NewSlogHandler()
	slog.SetDefault(slog.New(&teeHandler{handlers: []slog.Handler{textHandler, otelHandler}}))

	slog.InfoContext(ctx, "tracelit sdk initialized",
		"service", getEnv("TRACELIT_SERVICE_NAME", "products-crud-api"),
		"endpoint", mustEnv("TRACELIT_ENDPOINT"),
		"environment", getEnv("APP_ENV", "development"),
	)

	// ── PostgreSQL ─────────────────────────────────────────────────────────
	dsn := mustEnv("DATABASE_URL")
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		slog.ErrorContext(ctx, "failed to open db", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		slog.ErrorContext(ctx, "db ping failed — is PostgreSQL running?", "dsn", dsn, "error", err)
		os.Exit(1)
	}
	slog.InfoContext(ctx, "database connected")

	if err := runMigrations(ctx, db); err != nil {
		slog.ErrorContext(ctx, "migrations failed", "error", err)
		os.Exit(1)
	}
	slog.InfoContext(ctx, "migrations complete")

	// ── HTTP server ────────────────────────────────────────────────────────
	mux := http.NewServeMux()
	srv := &server{db: db}
	srv.registerRoutes(mux)

	// Layer: panic recovery → request ID injection → Tracelit HTTP tracing → mux
	handler := recoverMiddleware(requestIDMiddleware(tlmiddleware.NewHTTPHandler(mux)))

	httpServer := &http.Server{
		Addr:         ":" + getEnv("PORT", "8080"),
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.InfoContext(ctx, "http server listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.ErrorContext(ctx, "server error", "error", err)
			os.Exit(1)
		}
	}()

	<-quit
	slog.InfoContext(ctx, "shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.ErrorContext(ctx, "graceful shutdown failed", "error", err)
	}
	slog.InfoContext(ctx, "server stopped")
}

// recoverMiddleware catches panics, records them as span errors, and returns
// HTTP 500 so the server stays alive.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				ctx := r.Context()
				tracelit.SpanFromContext(ctx).RecordPanic(rec, tracelit.WithSwallowPanic())
				slog.ErrorContext(ctx, "recovered from panic",
					"panic", fmt.Sprintf("%v", rec),
					"method", r.Method,
					"path", r.URL.Path,
				)
				traceID := tracelit.TraceID(ctx)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, `{"error":"internal server error","code":"PANIC","trace_id":%q}`, traceID)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requestIDMiddleware echoes the upstream X-Request-Id (or the current span ID)
// back in the response and attaches it as a span attribute for easy correlation.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = tracelit.SpanID(r.Context())
		}
		if reqID != "" {
			w.Header().Set("X-Request-Id", reqID)
			tracelit.SpanFromContext(r.Context()).SetAttribute("http.request_id", reqID)
		}
		next.ServeHTTP(w, r)
	})
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "required environment variable %q is not set (add it to .env)\n", key)
		os.Exit(1)
	}
	return v
}

// teeHandler fans slog records out to multiple handlers simultaneously.
type teeHandler struct {
	handlers []slog.Handler
}

func (t *teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range t.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (t *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range t.handlers {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (t *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(t.handlers))
	for i, h := range t.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &teeHandler{handlers: handlers}
}

func (t *teeHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(t.handlers))
	for i, h := range t.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &teeHandler{handlers: handlers}
}
