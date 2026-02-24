package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/agentfi/agentfi-go-backend/internal/agent"
	"github.com/agentfi/agentfi-go-backend/internal/auth"
	"github.com/agentfi/agentfi-go-backend/internal/invest"
	"github.com/agentfi/agentfi-go-backend/internal/mcp"
	"github.com/agentfi/agentfi-go-backend/internal/store"
	"github.com/agentfi/agentfi-go-backend/pkg/config"
)

func main() {
	// --- Config ---
	cfg, err := config.Load("config.yaml")
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	initLogger(cfg.Log.Level)
	slog.Info("config loaded", "port", cfg.Server.Port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Database ---
	pool, err := pgxpool.New(ctx, cfg.Database.DSN)
	if err != nil {
		slog.Error("failed to create db pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		slog.Error("failed to ping database", "error", err)
		os.Exit(1)
	}
	slog.Info("database connected")

	// --- Redis ---
	redisOpts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		slog.Error("failed to parse redis url", "error", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()

	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Error("failed to ping redis", "error", err)
		os.Exit(1)
	}
	slog.Info("redis connected")

	// --- Store ---
	st := store.NewStore(pool)

	// --- Router ---
	r := newRouter(st, rdb, cfg)

	// --- HTTP Server ---
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutting down server...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "error", err)
		}
		cancel()
	}()

	slog.Info("server starting", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}

func newRouter(st *store.Store, rdb *redis.Client, cfg *config.Config) *chi.Mux {
	r := chi.NewRouter()

	// Standard middleware
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	// Health check
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// --- Auth ---
	authSvc := auth.NewService(rdb, cfg.Auth.JWTSecret)
	authHandler := auth.NewHandler(authSvc)

	// --- Agent ---
	agentSvc := agent.NewService(st, cfg.LLM)
	agentHandler := agent.NewHandler(agentSvc)

	// --- MCP ---
	mcpMgr := mcp.NewManager(st)
	mcpHandler := mcp.NewHandler(mcpMgr, st)

	// --- Invest ---
	investSvc := invest.NewService(st)
	investHandler := invest.NewHandler(investSvc)

	// API routes
	r.Route("/api", func(r chi.Router) {
		// Public auth endpoints
		r.Route("/auth", func(r chi.Router) {
			r.Post("/nonce", authHandler.HandleNonce)
			r.Post("/verify", authHandler.HandleVerify)
		})

		// Protected routes (require JWT)
		r.Group(func(r chi.Router) {
			r.Use(authSvc.JWTMiddleware)
			r.Mount("/agents", agentHandler.Routes())
			r.Get("/mcp-servers", mcpHandler.HandleList)
			r.Post("/mcp-servers", mcpHandler.HandleRegister)
			r.Post("/invest", investHandler.HandleDeposit)
			r.Post("/redeem", investHandler.HandleRedeem)
			r.Get("/portfolio", investHandler.HandlePortfolio)
		})
	})

	return r
}

func initLogger(level string) {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(handler))
}
