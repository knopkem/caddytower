package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"caddytower/internal/auth"
	"caddytower/internal/config"
	"caddytower/internal/store"
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
	auth    *auth.Service
}

type readinessChecker interface {
	Ping(context.Context) error
}

func New(cfg config.Config, webUI *ui.UI, logger *slog.Logger, build version.Info, ready readinessChecker, authService *auth.Service) *Server {
	return &Server{
		cfg:     cfg,
		ui:      webUI,
		logger:  logger,
		version: build,
		ready:   ready,
		auth:    authService,
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

	router.Get("/healthz", s.handleHealth)
	router.Get("/readyz", s.handleReady)
	router.Get("/-/version", s.handleVersion)
	router.Handle("/assets/*", http.StripPrefix("/assets/", assets))
	router.Get("/", s.handleRoot)

	if s.auth != nil {
		router.Get("/setup", s.handleSetupForm)
		router.Post("/setup", s.handleSetupSubmit)
		router.Get("/login", s.handleLoginForm)
		router.Post("/login", s.handleLoginSubmit)
		router.Post("/logout", s.handleLogout)
	}

	return router
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		required, err := s.auth.BootstrapRequired(r.Context())
		if err != nil {
			s.logger.Error("check bootstrap status", "error", err)
			http.Error(w, "failed to check bootstrap status", http.StatusInternalServerError)
			return
		}
		if required {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}

		currentUser, ok, err := s.currentUser(w, r)
		if err != nil {
			s.logger.Error("authenticate request", "error", err)
			http.Error(w, "failed to authenticate request", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		s.renderDashboard(w, currentUser.Email, s.auth.EnsureCSRFCookie(w, r))
		return
	}

	s.renderDashboard(w, "", "")
}

func (s *Server) renderDashboard(w http.ResponseWriter, currentUser, csrfToken string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	data := ui.HomePageData{
		GeneratedAt: time.Now().UTC(),
		PageTitle:   "CaddyTower | Dashboard",
		Headline:    "CaddyTower dashboard",
		CSRFToken:   csrfToken,
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
		CurrentUser: currentUser,
	}

	if err := s.ui.Render(w, "home.gohtml", data); err != nil {
		s.logger.Error("render home", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	required, err := s.auth.BootstrapRequired(r.Context())
	if err != nil {
		s.logger.Error("check bootstrap status", "error", err)
		http.Error(w, "failed to check bootstrap status", http.StatusInternalServerError)
		return
	}
	if !required {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	email := strings.TrimSpace(r.URL.Query().Get("email"))
	if email == "" {
		email = "admin@example.com"
	}

	enrollment, err := s.auth.GenerateEnrollment(email)
	if err != nil {
		s.logger.Error("generate enrollment", "error", err)
		http.Error(w, "failed to generate enrollment", http.StatusInternalServerError)
		return
	}

	s.renderSetup(w, r, ui.SetupPageData{
		PageTitle:  "CaddyTower | Setup",
		Headline:   "Create the first admin user",
		Email:      email,
		ManualKey:  enrollment.ManualKey,
		OTPAuthURL: enrollment.URL,
	})
}

func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	token, _, err := s.auth.CreateInitialUser(
		r.Context(),
		r.FormValue("email"),
		r.FormValue("password"),
		r.FormValue("confirm_password"),
		r.FormValue("totp_secret"),
		r.FormValue("code"),
		auth.ClientIP(r),
		r.UserAgent(),
	)
	if err != nil {
		data := ui.SetupPageData{
			PageTitle:    "CaddyTower | Setup",
			Headline:     "Create the first admin user",
			Email:        strings.TrimSpace(r.FormValue("email")),
			ManualKey:    strings.TrimSpace(r.FormValue("totp_secret")),
			OTPAuthURL:   s.auth.BuildEnrollment(r.FormValue("email"), r.FormValue("totp_secret")).URL,
			ErrorMessage: err.Error(),
		}
		s.renderSetup(w, r, data)
		return
	}

	s.auth.SetSessionCookie(w, r, token)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, data ui.SetupPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data.CSRFToken = s.auth.EnsureCSRFCookie(w, r)
	if err := s.ui.Render(w, "setup.gohtml", data); err != nil {
		s.logger.Error("render setup", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	required, err := s.auth.BootstrapRequired(r.Context())
	if err != nil {
		s.logger.Error("check bootstrap status", "error", err)
		http.Error(w, "failed to check bootstrap status", http.StatusInternalServerError)
		return
	}
	if required {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}

	if currentUser, ok, err := s.currentUser(w, r); err == nil && ok && currentUser.Email != "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	} else if err != nil {
		s.logger.Error("authenticate request", "error", err)
		http.Error(w, "failed to authenticate request", http.StatusInternalServerError)
		return
	}

	s.renderLogin(w, r, ui.LoginPageData{
		PageTitle: "CaddyTower | Sign in",
		Headline:  "Sign in to CaddyTower",
	})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	token, _, err := s.auth.Login(
		r.Context(),
		r.FormValue("email"),
		r.FormValue("password"),
		r.FormValue("code"),
		auth.ClientIP(r),
		r.UserAgent(),
	)
	if err != nil {
		s.renderLogin(w, r, ui.LoginPageData{
			PageTitle:    "CaddyTower | Sign in",
			Headline:     "Sign in to CaddyTower",
			Email:        strings.TrimSpace(r.FormValue("email")),
			ErrorMessage: err.Error(),
		})
		return
	}

	s.auth.SetSessionCookie(w, r, token)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, data ui.LoginPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data.CSRFToken = s.auth.EnsureCSRFCookie(w, r)
	if err := s.ui.Render(w, "login.gohtml", data); err != nil {
		s.logger.Error("render login", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	if cookie, err := r.Cookie(s.auth.SessionCookieName()); err == nil {
		if err := s.auth.Logout(r.Context(), cookie.Value); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.logger.Error("logout", "error", err)
			http.Error(w, "failed to logout", http.StatusInternalServerError)
			return
		}
	}

	s.auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) currentUser(w http.ResponseWriter, r *http.Request) (auth.User, bool, error) {
	cookie, err := r.Cookie(s.auth.SessionCookieName())
	if err != nil {
		return auth.User{}, false, nil
	}

	user, err := s.auth.Authenticate(r.Context(), cookie.Value)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.auth.ClearSessionCookie(w)
			return auth.User{}, false, nil
		}
		return auth.User{}, false, err
	}

	return user, true, nil
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
