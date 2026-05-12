package server

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"caddytower/internal/auth"
	"caddytower/internal/config"
	"caddytower/internal/projects"
	"caddytower/internal/store"
	"caddytower/internal/ui"
	"caddytower/internal/version"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	cfg      config.Config
	ui       *ui.UI
	logger   *slog.Logger
	version  version.Info
	ready    readinessChecker
	auth     *auth.Service
	projects *projects.Service
	limiter  *webhookRateLimiter
}

type readinessChecker interface {
	Ping(context.Context) error
}

func New(cfg config.Config, webUI *ui.UI, logger *slog.Logger, build version.Info, ready readinessChecker, authService *auth.Service, projectService *projects.Service) *Server {
	return &Server{
		cfg:      cfg,
		ui:       webUI,
		logger:   logger,
		version:  build,
		ready:    ready,
		auth:     authService,
		projects: projectService,
		limiter:  newWebhookRateLimiter(10, time.Minute),
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
	if s.projects != nil {
		router.Post("/api/webhooks/deploy/{slug}", s.handleDeployWebhook)
	}

	if s.auth != nil {
		router.Get("/setup", s.handleSetupForm)
		router.Post("/setup", s.handleSetupSubmit)
		router.Get("/login", s.handleLoginForm)
		router.Post("/login", s.handleLoginSubmit)
		router.Post("/logout", s.handleLogout)
		if s.projects != nil {
			router.Post("/settings", s.handleSettingsSubmit)
			router.Post("/adopt", s.handleAdoptProjects)
			router.Post("/projects", s.handleProjectCreate)
			router.Get("/projects/{projectID}", s.handleProjectPage)
			router.Get("/projects/{projectID}/logs/stream", s.handleProjectLogsStream)
			router.Post("/projects/{projectID}", s.handleProjectUpdate)
			router.Post("/projects/{projectID}/databases", s.handleProjectDBAttach)
			router.Post("/projects/{projectID}/databases/{attachmentID}/rotate", s.handleProjectDBRotate)
			router.Post("/projects/{projectID}/databases/{attachmentID}/delete", s.handleProjectDBDelete)
			router.Post("/projects/{projectID}/deploy", s.handleProjectRedeploy)
			router.Post("/projects/{projectID}/delete", s.handleProjectDelete)
		}
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

		s.renderDashboard(w, r, currentUser.Email, ui.ProjectFormData{
			Type:              "web",
			InternalPort:      3000,
			WatchtowerEnabled: true,
		}, "", r.URL.Query().Get("info"))
		return
	}

	s.renderDashboard(w, r, "", ui.ProjectFormData{}, "", "")
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, currentUser string, createForm ui.ProjectFormData, errorMessage, infoMessage string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	settings := ui.SettingsFormData{}
	projectItems := []ui.ProjectListItem{}
	if s.projects != nil {
		dashboard, err := s.projects.Dashboard(r.Context())
		if err != nil {
			s.logger.Error("load dashboard", "error", err)
			http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
			return
		}
		settings = ui.SettingsFormData{
			RootDomain:             dashboard.Settings.RootDomain,
			OriginHostname:         dashboard.Settings.OriginHostname,
			CloudflareZoneID:       dashboard.Settings.CloudflareZoneID,
			CloudflareTokenPresent: dashboard.Settings.CloudflareTokenPresent,
			CloudflareProxied:      dashboard.Settings.CloudflareProxied,
		}
		for _, project := range dashboard.Projects {
			projectItems = append(projectItems, projectListItem(project))
		}
	}

	if createForm.Type == "" {
		createForm.Type = "web"
	}
	if createForm.Type == "web" && createForm.InternalPort == 0 {
		createForm.InternalPort = 3000
	}

	csrfToken := ""
	if s.auth != nil {
		csrfToken = s.auth.EnsureCSRFCookie(w, r)
	}

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
		CurrentUser:  currentUser,
		ErrorMessage: errorMessage,
		InfoMessage:  infoMessage,
		Settings:     settings,
		CreateForm:   createForm,
		Projects:     projectItems,
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

func (s *Server) handleSettingsSubmit(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	err := s.projects.SaveSettings(r.Context(), projects.SettingsInput{
		RootDomain:        r.FormValue("root_domain"),
		OriginHostname:    r.FormValue("origin_hostname"),
		CloudflareZoneID:  r.FormValue("cloudflare_zone_id"),
		CloudflareToken:   r.FormValue("cloudflare_api_token"),
		CloudflareProxied: projects.ParseBoolCheckbox(r.FormValue("cloudflare_proxied")),
	}, user.ID)
	if err != nil {
		s.renderDashboard(w, r, user.Email, ui.ProjectFormData{
			Type:              "web",
			InternalPort:      3000,
			WatchtowerEnabled: true,
		}, err.Error(), "")
		return
	}

	http.Redirect(w, r, "/?info=Settings+saved", http.StatusFound)
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	input, createForm, err := projectInputFromRequest(r, "", "")
	if err != nil {
		s.renderDashboard(w, r, user.Email, createForm, err.Error(), "")
		return
	}

	project, err := s.projects.CreateProject(r.Context(), input, user.ID)
	if err != nil {
		s.renderDashboard(w, r, user.Email, createForm, err.Error(), "")
		return
	}

	http.Redirect(w, r, "/projects/"+project.ID+"?info=Project+saved", http.StatusFound)
}

func (s *Server) handleAdoptProjects(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	adopted, err := s.projects.AdoptExisting(r.Context(), user.ID)
	if err != nil {
		s.renderDashboard(w, r, user.Email, ui.ProjectFormData{
			Type:              "web",
			InternalPort:      3000,
			WatchtowerEnabled: true,
		}, err.Error(), "")
		return
	}

	message := "No adoptable projects found"
	if len(adopted) == 1 {
		message = "Adopted 1 existing project"
	} else if len(adopted) > 1 {
		message = fmt.Sprintf("Adopted %d existing projects", len(adopted))
	}
	http.Redirect(w, r, "/?info="+neturl.QueryEscape(message), http.StatusFound)
}

func (s *Server) handleProjectPage(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}

	project, _, err := s.projects.GetProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		s.renderProjectError(w, err)
		return
	}

	s.renderProjectPage(w, r, user.Email, project, ui.ProjectFormData{
		ID:                project.ID,
		Action:            "/projects/" + project.ID,
		SubmitLabel:       "Save and deploy",
		Type:              project.Type,
		Name:              project.Name,
		Slug:              project.Slug,
		ImageRef:          project.ImageRef,
		Subdomain:         project.Subdomain,
		InternalPort:      project.InternalPort,
		PortMappingsText:  projects.PortMappingsText(project.Ports),
		WatchtowerEnabled: project.WatchtowerEnabled,
		EnvText:           project.EnvText,
		SlugReadOnly:      true,
		TypeReadOnly:      true,
	}, "", r.URL.Query().Get("info"))
}

func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	projectID := chi.URLParam(r, "projectID")
	currentProject, _, err := s.projects.GetProject(r.Context(), projectID)
	if err != nil {
		s.renderProjectError(w, err)
		return
	}

	input, formData, err := projectInputFromRequest(r, projectID, currentProject.Type)
	if err != nil {
		s.renderProjectWithFallback(w, r, user.Email, projectID, formData, err.Error(), "")
		return
	}

	project, err := s.projects.UpdateProject(r.Context(), input, user.ID)
	if err != nil {
		s.renderProjectWithFallback(w, r, user.Email, projectID, formData, err.Error(), "")
		return
	}

	http.Redirect(w, r, "/projects/"+project.ID+"?info=Project+updated", http.StatusFound)
}

func (s *Server) handleProjectDBAttach(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	projectID := chi.URLParam(r, "projectID")
	project, err := s.projects.AttachDatabase(r.Context(), projects.DatabaseAttachmentInput{
		ProjectID:  projectID,
		Engine:     strings.TrimSpace(r.FormValue("engine")),
		EnvVarName: strings.TrimSpace(r.FormValue("env_var_name")),
	}, user.ID)
	if err != nil {
		s.renderProjectWithFallback(w, r, user.Email, projectID, ui.ProjectFormData{}, err.Error(), "")
		return
	}

	http.Redirect(w, r, "/projects/"+project.ID+"?info=Database+attached", http.StatusFound)
}

func (s *Server) handleProjectDBRotate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	projectID := chi.URLParam(r, "projectID")
	attachmentID, err := strconv.ParseInt(chi.URLParam(r, "attachmentID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid attachment id", http.StatusBadRequest)
		return
	}

	project, err := s.projects.RotateDatabaseAttachment(r.Context(), projectID, attachmentID, user.ID)
	if err != nil {
		s.renderProjectWithFallback(w, r, user.Email, projectID, ui.ProjectFormData{}, err.Error(), "")
		return
	}

	http.Redirect(w, r, "/projects/"+project.ID+"?info=Database+password+rotated", http.StatusFound)
}

func (s *Server) handleProjectDBDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	projectID := chi.URLParam(r, "projectID")
	attachmentID, err := strconv.ParseInt(chi.URLParam(r, "attachmentID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid attachment id", http.StatusBadRequest)
		return
	}

	project, err := s.projects.DeleteDatabaseAttachment(r.Context(), projectID, attachmentID, user.ID)
	if err != nil {
		s.renderProjectWithFallback(w, r, user.Email, projectID, ui.ProjectFormData{}, err.Error(), "")
		return
	}

	http.Redirect(w, r, "/projects/"+project.ID+"?info=Database+detached", http.StatusFound)
}

func (s *Server) handleProjectRedeploy(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	project, err := s.projects.RedeployProject(r.Context(), chi.URLParam(r, "projectID"), user.ID)
	if err != nil {
		s.renderProjectWithFallback(w, r, user.Email, chi.URLParam(r, "projectID"), ui.ProjectFormData{}, err.Error(), "")
		return
	}

	http.Redirect(w, r, "/projects/"+project.ID+"?info=Project+redeployed", http.StatusFound)
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	if err := s.projects.DeleteProject(r.Context(), chi.URLParam(r, "projectID"), user.ID); err != nil {
		s.renderDashboard(w, r, user.Email, ui.ProjectFormData{
			Type:              "web",
			InternalPort:      3000,
			WatchtowerEnabled: true,
		}, err.Error(), "")
		return
	}

	http.Redirect(w, r, "/?info=Project+deleted", http.StatusFound)
}

func (s *Server) handleProjectLogsStream(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.projects == nil {
		http.NotFound(w, r)
		return
	}

	_, ok, err := s.currentUser(w, r)
	if err != nil {
		s.logger.Error("authenticate log stream", "error", err)
		http.Error(w, "failed to authenticate request", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	tail := 200
	if rawTail := strings.TrimSpace(r.URL.Query().Get("tail")); rawTail != "" {
		if parsedTail, err := strconv.Atoi(rawTail); err == nil && parsedTail > 0 {
			tail = parsedTail
		}
	}

	reader, err := s.projects.StreamProjectLogs(r.Context(), chi.URLParam(r, "projectID"), tail)
	if err != nil {
		s.renderProjectError(w, err)
		return
	}
	defer reader.Close()

	headers := w.Header()
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")
	headers.Set("X-Accel-Buffering", "no")

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
			return
		}
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Error("stream project logs", "project_id", chi.URLParam(r, "projectID"), "error", err)
	}
}

func (s *Server) handleDeployWebhook(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(chi.URLParam(r, "slug"))
	if slug == "" {
		http.NotFound(w, r)
		return
	}
	if !s.limiter.Allow(auth.ClientIP(r) + ":" + slug) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	project, _, err := s.projects.GetProjectBySlug(r.Context(), slug)
	if err != nil {
		s.renderProjectError(w, err)
		return
	}

	payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if !validWebhookSignature(project.WebhookSecret, r.Header.Get("X-Signature"), payload) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	if _, err := s.projects.RedeployProjectByWebhook(r.Context(), slug); err != nil {
		s.logger.Error("webhook redeploy", "slug", slug, "error", err)
		http.Error(w, "failed to redeploy project", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "accepted",
		"project": slug,
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

func (s *Server) requireAuthenticated(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	required, err := s.auth.BootstrapRequired(r.Context())
	if err != nil {
		s.logger.Error("check bootstrap status", "error", err)
		http.Error(w, "failed to check bootstrap status", http.StatusInternalServerError)
		return auth.User{}, false
	}
	if required {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return auth.User{}, false
	}

	user, ok, err := s.currentUser(w, r)
	if err != nil {
		s.logger.Error("authenticate request", "error", err)
		http.Error(w, "failed to authenticate request", http.StatusInternalServerError)
		return auth.User{}, false
	}
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return auth.User{}, false
	}
	return user, true
}

func (s *Server) renderProjectPage(w http.ResponseWriter, r *http.Request, currentUser string, project projects.Project, form ui.ProjectFormData, errorMessage, infoMessage string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	attachments := make([]ui.DBAttachmentItem, 0, len(project.DBAttachments))
	for _, attachment := range project.DBAttachments {
		attachments = append(attachments, ui.DBAttachmentItem{
			ID:             attachment.ID,
			Engine:         attachment.Engine,
			DBName:         attachment.DBName,
			DBUser:         attachment.DBUser,
			EnvVarName:     attachment.EnvVarName,
			Host:           attachment.Host,
			Port:           attachment.Port,
			ConnectionHint: attachment.ConnectionHint,
		})
	}

	data := ui.ProjectPageData{
		PageTitle:      "CaddyTower | " + project.Name,
		Headline:       "Edit " + project.Name,
		CSRFToken:      s.auth.EnsureCSRFCookie(w, r),
		CurrentUser:    currentUser,
		InfoMessage:    infoMessage,
		ErrorMessage:   errorMessage,
		Project:        form,
		ProjectMeta:    projectListItem(project),
		WebhookURL:     strings.TrimRight(s.cfg.PublicBaseURL, "/") + "/api/webhooks/deploy/" + project.Slug,
		WebhookSecret:  project.WebhookSecret,
		Attachments:    attachments,
		AttachmentForm: ui.DBAttachmentFormData{Engine: "pg", EnvVarName: "DATABASE_URL"},
	}

	if err := s.ui.Render(w, "project.gohtml", data); err != nil {
		s.logger.Error("render project page", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

func (s *Server) renderProjectWithFallback(w http.ResponseWriter, r *http.Request, currentUser, projectID string, form ui.ProjectFormData, errorMessage, infoMessage string) {
	project, _, err := s.projects.GetProject(r.Context(), projectID)
	if err != nil {
		s.renderProjectError(w, err)
		return
	}
	if form.Action == "" {
		form = ui.ProjectFormData{
			ID:                project.ID,
			Action:            "/projects/" + project.ID,
			SubmitLabel:       "Save and deploy",
			Type:              project.Type,
			Name:              project.Name,
			Slug:              project.Slug,
			ImageRef:          project.ImageRef,
			Subdomain:         project.Subdomain,
			InternalPort:      project.InternalPort,
			PortMappingsText:  projects.PortMappingsText(project.Ports),
			WatchtowerEnabled: project.WatchtowerEnabled,
			EnvText:           project.EnvText,
			SlugReadOnly:      true,
			TypeReadOnly:      true,
		}
	}
	s.renderProjectPage(w, r, currentUser, project, form, errorMessage, infoMessage)
}

func (s *Server) renderProjectError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	s.logger.Error("project error", "error", err)
	http.Error(w, "failed to load project", http.StatusInternalServerError)
}

func projectInputFromRequest(r *http.Request, projectID, fixedType string) (projects.WebProjectInput, ui.ProjectFormData, error) {
	projectType := strings.TrimSpace(strings.ToLower(fixedType))
	if projectType == "" {
		projectType = strings.TrimSpace(strings.ToLower(r.FormValue("project_type")))
	}
	if projectType == "" {
		projectType = "web"
	}

	internalPort := 0
	internalPortValue := strings.TrimSpace(r.FormValue("internal_port"))
	if internalPortValue != "" {
		parsedPort, err := strconv.Atoi(internalPortValue)
		if err != nil {
			form := projectFormDataFromRequest(r, projectID, projectType)
			form.TypeReadOnly = fixedType != ""
			return projects.WebProjectInput{}, form, fmt.Errorf("internal port must be a number")
		}
		internalPort = parsedPort
	}

	input := projects.WebProjectInput{
		Type:              projectType,
		ID:                projectID,
		Name:              strings.TrimSpace(r.FormValue("name")),
		Slug:              strings.TrimSpace(r.FormValue("slug")),
		ImageRef:          strings.TrimSpace(r.FormValue("image_ref")),
		Subdomain:         strings.TrimSpace(r.FormValue("subdomain")),
		InternalPort:      internalPort,
		PortMappingsText:  r.FormValue("port_mappings"),
		WatchtowerEnabled: projects.ParseBoolCheckbox(r.FormValue("watchtower_enabled")),
		EnvText:           r.FormValue("env_text"),
	}

	form := projectFormDataFromInput(input, projectID)
	form.TypeReadOnly = fixedType != ""
	return input, form, nil
}

func projectListItem(project projects.Project) ui.ProjectListItem {
	portMappings := strings.ReplaceAll(projects.PortMappingsText(project.Ports), "\n", ", ")
	return ui.ProjectListItem{
		ID:                project.ID,
		Name:              project.Name,
		Type:              project.Type,
		Slug:              project.Slug,
		ImageRef:          project.ImageRef,
		Subdomain:         project.Subdomain,
		FullDomain:        project.FullDomain,
		ContainerName:     project.ContainerName,
		InternalPort:      project.InternalPort,
		PortMappingsText:  portMappings,
		EndpointSummary:   projectEndpointSummary(project),
		WatchtowerEnabled: project.WatchtowerEnabled,
		Status:            project.Status,
	}
}

func projectFormDataFromRequest(r *http.Request, projectID, projectType string) ui.ProjectFormData {
	input := projects.WebProjectInput{
		Type:              projectType,
		ID:                projectID,
		Name:              strings.TrimSpace(r.FormValue("name")),
		Slug:              strings.TrimSpace(r.FormValue("slug")),
		ImageRef:          strings.TrimSpace(r.FormValue("image_ref")),
		Subdomain:         strings.TrimSpace(r.FormValue("subdomain")),
		PortMappingsText:  r.FormValue("port_mappings"),
		WatchtowerEnabled: projects.ParseBoolCheckbox(r.FormValue("watchtower_enabled")),
		EnvText:           r.FormValue("env_text"),
	}
	if internalPort, err := strconv.Atoi(strings.TrimSpace(r.FormValue("internal_port"))); err == nil {
		input.InternalPort = internalPort
	}
	return projectFormDataFromInput(input, projectID)
}

func projectFormDataFromInput(input projects.WebProjectInput, projectID string) ui.ProjectFormData {
	form := ui.ProjectFormData{
		ID:                projectID,
		Action:            "/projects/" + projectID,
		SubmitLabel:       "Save and deploy",
		Type:              input.Type,
		Name:              input.Name,
		Slug:              input.Slug,
		ImageRef:          input.ImageRef,
		Subdomain:         input.Subdomain,
		InternalPort:      input.InternalPort,
		PortMappingsText:  input.PortMappingsText,
		WatchtowerEnabled: input.WatchtowerEnabled,
		EnvText:           input.EnvText,
		SlugReadOnly:      projectID != "",
	}
	if form.Type == "" {
		form.Type = "web"
	}
	if projectID == "" {
		form.Action = "/projects"
		form.SubmitLabel = "Create and deploy"
	}
	return form
}

func projectEndpointSummary(project projects.Project) string {
	if project.Type == "web" {
		if project.FullDomain != "" {
			return project.FullDomain
		}
		return project.Subdomain
	}
	return strings.ReplaceAll(projects.PortMappingsText(project.Ports), "\n", ", ")
}

func validWebhookSignature(secret, provided string, payload []byte) bool {
	if secret == "" || provided == "" {
		return false
	}
	provided = strings.TrimSpace(provided)
	expectedMAC := hmac.New(sha256.New, []byte(secret))
	expectedMAC.Write(payload)
	expected := "sha256=" + fmt.Sprintf("%x", expectedMAC.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(provided))
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
