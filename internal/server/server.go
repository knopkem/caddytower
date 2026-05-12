package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"caddytower/internal/config"
	"caddytower/internal/ui"
	"caddytower/internal/version"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	cfg     config.Config
	ui      *ui.UI
	logger  *slog.Logger
	version version.Info
	ready   readinessChecker
}

type readinessChecker interface {
	Ping(context.Context) error
}

func New(cfg config.Config, webUI *ui.UI, logger *slog.Logger, build version.Info, ready readinessChecker) *Server {
	return &Server{
		cfg:     cfg,
		ui:      webUI,
		logger:  logger,
		version: build,
		ready:   ready,
	}
}

func (s *Server) Router() http.Handler {
	router := chi.NewRouter()
	router.Use(chimiddleware.RequestID)
	router.Use(chimiddleware.RealIP)
	router.Use(chimiddleware.Recoverer)
	router.Use(s.requestLogger)
	router.Use(securityHeaders)

	assets := http.FileServer(http.FS(s.ui.Assets()))

	router.Get("/", s.handleHome)
	router.Get("/healthz", s.handleHealth)
	router.Get("/readyz", s.handleReady)
	router.Get("/-/version", s.handleVersion)
	router.Handle("/assets/*", http.StripPrefix("/assets/", assets))

	return router
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	data := ui.HomePageData{
		GeneratedAt: time.Now().UTC(),
		PageTitle:   "CaddyTower | Scaffold ready",
		Headline:    "CaddyTower scaffold is ready",
		Version:     s.version,
		Config: ui.ConfigSummary{
			HTTPAddr:      s.cfg.HTTPAddr,
			PublicBaseURL: s.cfg.PublicBaseURL,
			DataDir:       s.cfg.DataDir,
			StateDBPath:   s.cfg.StateDBPath(),
			CaddyAdminURL: s.cfg.CaddyAdminURL,
			RootDomain:    s.cfg.RootDomain,
			DockerHost:    s.cfg.DockerHost,
			MasterKeySet:  s.cfg.MasterKey != "",
		},
	}

	if err := s.ui.Render(w, "home.gohtml", data); err != nil {
		s.logger.Error("render home", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.ready != nil {
		if err := s.ready.Ping(context.Background()); err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "not_ready",
				"error":  err.Error(),
			})
			return
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ready",
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(s.version)
}
