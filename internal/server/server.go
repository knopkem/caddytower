package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"caddytower/internal/auth"
	"caddytower/internal/backups"
	"caddytower/internal/config"
	githubapp "caddytower/internal/github"
	"caddytower/internal/monitor"
	"caddytower/internal/projects"
	"caddytower/internal/store"
	"caddytower/internal/ui"
	"caddytower/internal/version"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/pquerna/otp"
)

type Server struct {
	cfg          config.Config
	ui           *ui.UI
	logger       *slog.Logger
	version      version.Info
	ready        readinessChecker
	auth         *auth.Service
	projects     *projects.Service
	backups      *backups.Service
	monitor      *monitor.Service
	github       *githubapp.Service
	limiter      *webhookRateLimiter
	imageChecker *imageRefChecker
}

type readinessChecker interface {
	Ping(context.Context) error
}

func New(cfg config.Config, webUI *ui.UI, logger *slog.Logger, build version.Info, ready readinessChecker, authService *auth.Service, projectService *projects.Service, githubService *githubapp.Service, backupService *backups.Service, monitorServices ...*monitor.Service) *Server {
	var monitorService *monitor.Service
	if len(monitorServices) > 0 {
		monitorService = monitorServices[0]
	}
	return &Server{
		cfg:          cfg,
		ui:           webUI,
		logger:       logger,
		version:      build,
		ready:        ready,
		auth:         authService,
		projects:     projectService,
		github:       githubService,
		backups:      backupService,
		monitor:      monitorService,
		limiter:      newWebhookRateLimiter(10, time.Minute),
		imageChecker: newImageRefChecker(),
	}
}

func (s *Server) Router() http.Handler {
	router := chi.NewRouter()
	router.Use(chimiddleware.RequestID)
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
	if s.github != nil {
		router.Post("/api/webhooks/github", s.handleGitHubWebhook)
	}

	if s.auth != nil {
		router.Get("/setup", s.handleSetupForm)
		router.Post("/setup", s.handleSetupSubmit)
		router.Get("/login", s.handleLoginForm)
		router.Post("/login", s.handleLoginSubmit)
		router.Post("/logout", s.handleLogout)
		router.Get("/api/image-check", s.handleImageCheck)
		if s.github != nil {
			router.Get("/github/install", s.handleGitHubInstall)
			router.Post("/github/installations/{installationID}/disconnect", s.handleGitHubDisconnect)
		}
		if s.projects != nil {
			router.Get("/settings", s.handleSettingsPage)
			router.Post("/settings", s.handleSettingsSubmit)
			router.Post("/adopt", s.handleAdoptProjects)
			if s.github != nil {
				router.Get("/projects/import", s.handleImportPage)
				router.Post("/projects/import", s.handleImportCreate)
			}
			router.Post("/projects", s.handleProjectCreate)
			router.Get("/projects/{projectID}", s.handleProjectPage)
			router.Get("/projects/{projectID}/logs/stream", s.handleProjectLogsStream)
			router.Get("/projects/{projectID}/events/stream", s.handleProjectDeployEventsStream)
			router.Post("/projects/{projectID}", s.handleProjectUpdate)
			router.Post("/projects/{projectID}/domains", s.handleProjectDomainCreate)
			router.Post("/projects/{projectID}/domains/{domainID}/verify", s.handleProjectDomainVerify)
			router.Post("/projects/{projectID}/domains/{domainID}/delete", s.handleProjectDomainDelete)
			router.Post("/projects/{projectID}/databases", s.handleProjectDBAttach)
			router.Post("/projects/{projectID}/databases/{attachmentID}/rotate", s.handleProjectDBRotate)
			router.Post("/projects/{projectID}/databases/{attachmentID}/delete", s.handleProjectDBDelete)
			router.Post("/projects/{projectID}/deploy", s.handleProjectRedeploy)
			router.Post("/projects/{projectID}/deploys/{deployID}/rollback", s.handleProjectRollback)
			router.Post("/projects/{projectID}/delete", s.handleProjectDelete)
		}
		if s.backups != nil {
			router.Post("/backups/run", s.handleBackupRun)
			router.Get("/backups/{name}", s.handleBackupDownload)
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
			Type:                      "web",
			InternalPort:              3000,
			HealthCheckTimeoutSeconds: 5,
			WatchtowerEnabled:         true,
		}, "", r.URL.Query().Get("info"))
		return
	}

	s.renderDashboard(w, r, "", ui.ProjectFormData{}, "", "")
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, currentUser string, createForm ui.ProjectFormData, errorMessage, infoMessage string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	commonData, err := s.loadDashboardData(r.Context())
	if err != nil {
		s.logger.Error("load dashboard", "error", err)
		http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
		return
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
	errorTitle, errorHints := describeUIError(errorMessage)

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
		CurrentUser:               currentUser,
		ErrorMessage:              errorMessage,
		ErrorTitle:                errorTitle,
		ErrorHints:                errorHints,
		InfoMessage:               infoMessage,
		Settings:                  commonData.settings,
		CreateForm:                createForm,
		Projects:                  commonData.projects,
		Backups:                   commonData.backups,
		BackupsEnabled:            s.cfg.BackupsEnabled,
		BackupsRetentionDays:      s.cfg.BackupsRetentionDays,
		BackupsScheduleUTC:        s.cfg.BackupsScheduleUTC,
		BackupsIncludeEngineDumps: s.cfg.BackupsIncludeEngineDumps,
		VPSStatus:                 s.vpsStatusData(),
		ShowOnboarding:            r.URL.Query().Get("welcome") == "1",
		OpenProjectDialog:         r.URL.Query().Get("open") == "project",
		DomainConfigured:          commonData.settings.RootDomain != "" && commonData.settings.OriginHostname != "",
		NeedsSetup:                commonData.settings.RootDomain == "" || commonData.settings.OriginHostname == "" || len(commonData.projects) == 0,
		GitHubConfigured:          s.github != nil && s.github.Configured(),
		GitHubConnected:           len(s.gitHubStatusData(r.Context()).Installations) > 0,
	}

	if err := s.ui.Render(w, "home.gohtml", data); err != nil {
		s.logger.Error("render home", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

type dashboardData struct {
	settings ui.SettingsFormData
	projects []ui.ProjectListItem
	backups  []ui.BackupItem
}

func (s *Server) loadDashboardData(ctx context.Context) (dashboardData, error) {
	data := dashboardData{
		settings: ui.SettingsFormData{},
		projects: []ui.ProjectListItem{},
		backups:  []ui.BackupItem{},
	}
	if s.projects != nil {
		dashboard, err := s.projects.Dashboard(ctx)
		if err != nil {
			return dashboardData{}, err
		}
		data.settings = ui.SettingsFormData{
			RootDomain:             dashboard.Settings.RootDomain,
			OriginHostname:         dashboard.Settings.OriginHostname,
			CloudflareZoneID:       dashboard.Settings.CloudflareZoneID,
			CloudflareTokenPresent: dashboard.Settings.CloudflareTokenPresent,
			CloudflareProxied:      dashboard.Settings.CloudflareProxied,
		}
		for _, project := range dashboard.Projects {
			data.projects = append(data.projects, projectListItem(project))
		}
	}
	if s.backups != nil {
		snapshots, err := s.backups.ListSnapshots()
		if err != nil {
			return dashboardData{}, err
		}
		for _, snapshot := range snapshots {
			data.backups = append(data.backups, ui.BackupItem{
				Name:      snapshot.Name,
				CreatedAt: snapshot.CreatedAt.Format("2006-01-02 15:04:05 MST"),
				Size:      humanSize(snapshot.SizeBytes),
			})
		}
	}
	return data, nil
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
	http.Redirect(w, r, "/?welcome=1", http.StatusFound)
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, data ui.SetupPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data.CSRFToken = s.auth.EnsureCSRFCookie(w, r)
	if data.QRCodeDataURL == "" && strings.TrimSpace(data.OTPAuthURL) != "" {
		qrCodeDataURL, err := buildTOTPQRCodeDataURL(data.OTPAuthURL)
		if err != nil {
			s.logger.Error("build setup qr code", "error", err)
		} else {
			data.QRCodeDataURL = template.URL(qrCodeDataURL)
		}
	}
	if err := s.ui.Render(w, "setup.gohtml", data); err != nil {
		s.logger.Error("render setup", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

func buildTOTPQRCodeDataURL(otpAuthURL string) (string, error) {
	key, err := otp.NewKeyFromURL(strings.TrimSpace(otpAuthURL))
	if err != nil {
		return "", fmt.Errorf("parse otpauth url: %w", err)
	}

	image, err := key.Image(240, 240)
	if err != nil {
		return "", fmt.Errorf("render qr image: %w", err)
	}

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image); err != nil {
		return "", fmt.Errorf("encode qr image: %w", err)
	}

	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes()), nil
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

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	s.renderSettingsPage(w, r, user.Email, "", r.URL.Query().Get("info"))
}

func (s *Server) handleGitHubInstall(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuthenticated(w, r); !ok {
		return
	}
	if s.github == nil || !s.github.Configured() {
		http.Redirect(w, r, "/settings?info=GitHub+App+is+not+configured", http.StatusFound)
		return
	}
	http.Redirect(w, r, s.github.InstallURL(), http.StatusFound)
}

func (s *Server) handleGitHubDisconnect(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	installationID, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "installationID")), 10, 64)
	if err != nil || installationID <= 0 {
		http.Error(w, "invalid installation id", http.StatusBadRequest)
		return
	}
	if err := s.github.DisconnectInstallation(r.Context(), installationID); err != nil {
		s.renderSettingsPage(w, r, user.Email, err.Error(), "")
		return
	}
	http.Redirect(w, r, "/settings?info="+neturl.QueryEscape("Disconnected GitHub installation "+strconv.FormatInt(installationID, 10)), http.StatusFound)
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
		s.renderSettingsPage(w, r, user.Email, err.Error(), "")
		return
	}

	http.Redirect(w, r, "/settings?info=Settings+saved", http.StatusFound)
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
		s.renderSettingsPage(w, r, user.Email, err.Error(), "")
		return
	}

	message := "No adoptable projects found"
	if len(adopted) == 1 {
		message = "Adopted 1 existing project"
	} else if len(adopted) > 1 {
		message = fmt.Sprintf("Adopted %d existing projects", len(adopted))
	}
	http.Redirect(w, r, "/settings?info="+neturl.QueryEscape(message), http.StatusFound)
}

func (s *Server) handleBackupRun(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	snapshot, err := s.backups.RunNow(r.Context(), "manual")
	if err != nil {
		s.renderSettingsPage(w, r, user.Email, err.Error(), "")
		return
	}

	http.Redirect(w, r, "/settings?info="+neturl.QueryEscape("Backup created: "+snapshot.Name), http.StatusFound)
}

func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuthenticatedForDownload(w, r); !ok {
		return
	}

	file, snapshot, err := s.backups.OpenSnapshot(chi.URLParam(r, "name"))
	if err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "invalid backup name") {
			http.NotFound(w, r)
			return
		}
		s.logger.Error("open backup", "error", err)
		http.Error(w, "failed to open backup", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", snapshot.Name))
	http.ServeContent(w, r, snapshot.Name, snapshot.CreatedAt, file)
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
		ID:                        project.ID,
		Action:                    "/projects/" + project.ID,
		SubmitLabel:               "Save and deploy",
		Type:                      project.Type,
		Name:                      project.Name,
		Slug:                      project.Slug,
		ImageRef:                  project.ImageRef,
		Subdomain:                 project.Subdomain,
		InternalPort:              project.InternalPort,
		HealthCheckPath:           project.HealthCheckPath,
		HealthCheckTimeoutSeconds: project.HealthCheckTimeoutSeconds,
		PortMappingsText:          projects.PortMappingsText(project.Ports),
		WatchtowerEnabled:         project.WatchtowerEnabled,
		EnvText:                   project.EnvText,
		SlugReadOnly:              true,
		TypeReadOnly:              true,
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

func (s *Server) handleProjectRollback(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	deployID, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "deployID")), 10, 64)
	if err != nil || deployID <= 0 {
		http.Error(w, "invalid deploy id", http.StatusBadRequest)
		return
	}
	project, err := s.projects.RollbackProject(r.Context(), chi.URLParam(r, "projectID"), deployID, user.ID)
	if err != nil {
		s.renderProjectWithFallback(w, r, user.Email, chi.URLParam(r, "projectID"), ui.ProjectFormData{}, err.Error(), "")
		return
	}
	http.Redirect(w, r, "/projects/"+project.ID+"?info=Rollback+started", http.StatusFound)
}

func (s *Server) handleProjectDomainCreate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	project, err := s.projects.AddProjectDomain(
		r.Context(),
		chi.URLParam(r, "projectID"),
		r.FormValue("hostname"),
		projects.ParseBoolCheckbox(r.FormValue("is_primary")),
		user.ID,
	)
	if err != nil {
		s.renderProjectWithFallback(w, r, user.Email, chi.URLParam(r, "projectID"), ui.ProjectFormData{}, err.Error(), "")
		return
	}
	http.Redirect(w, r, "/projects/"+project.ID+"?info="+neturl.QueryEscape("Custom domain added"), http.StatusFound)
}

func (s *Server) handleProjectDomainVerify(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	domainID, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "domainID")), 10, 64)
	if err != nil || domainID <= 0 {
		http.Error(w, "invalid domain id", http.StatusBadRequest)
		return
	}
	project, err := s.projects.VerifyProjectDomain(r.Context(), chi.URLParam(r, "projectID"), domainID, user.ID)
	if err != nil {
		s.renderProjectWithFallback(w, r, user.Email, chi.URLParam(r, "projectID"), ui.ProjectFormData{}, err.Error(), "")
		return
	}
	http.Redirect(w, r, "/projects/"+project.ID+"?info="+neturl.QueryEscape("Domain verified"), http.StatusFound)
}

func (s *Server) handleProjectDomainDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	domainID, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "domainID")), 10, 64)
	if err != nil || domainID <= 0 {
		http.Error(w, "invalid domain id", http.StatusBadRequest)
		return
	}
	project, err := s.projects.DeleteProjectDomain(r.Context(), chi.URLParam(r, "projectID"), domainID, user.ID)
	if err != nil {
		s.renderProjectWithFallback(w, r, user.Email, chi.URLParam(r, "projectID"), ui.ProjectFormData{}, err.Error(), "")
		return
	}
	http.Redirect(w, r, "/projects/"+project.ID+"?info="+neturl.QueryEscape("Custom domain removed"), http.StatusFound)
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
			Type:                      "web",
			InternalPort:              3000,
			HealthCheckTimeoutSeconds: 5,
			WatchtowerEnabled:         true,
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

func (s *Server) handleProjectDeployEventsStream(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.projects == nil {
		http.NotFound(w, r)
		return
	}

	_, ok, err := s.currentUser(w, r)
	if err != nil {
		s.logger.Error("authenticate deploy stream", "error", err)
		http.Error(w, "failed to authenticate request", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if _, _, err := s.projects.GetProject(r.Context(), chi.URLParam(r, "projectID")); err != nil {
		s.renderProjectError(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	headers := w.Header()
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")
	headers.Set("X-Accel-Buffering", "no")

	events, cancel := s.projects.SubscribeDeployEvents(chi.URLParam(r, "projectID"))
	defer cancel()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-events:
			payload, err := json.Marshal(map[string]string{
				"stage":     event.Stage,
				"message":   event.Message,
				"status":    event.Status,
				"timestamp": event.Timestamp.Format(time.RFC3339),
			})
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
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

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if s.github == nil {
		http.NotFound(w, r)
		return
	}

	payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	result, err := s.github.HandleWebhook(r.Context(), r.Header.Get("X-GitHub-Event"), r.Header.Get("X-Hub-Signature-256"), payload)
	if err != nil {
		if strings.Contains(err.Error(), "invalid github webhook signature") {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		s.logger.Error("github webhook", "error", err)
		http.Error(w, "failed to process github webhook", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "accepted",
		"result": result,
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

func (s *Server) requireAuthenticatedForDownload(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
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
		http.Error(w, "authentication required", http.StatusUnauthorized)
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
	domains := make([]ui.ProjectDomainItem, 0, len(project.CustomDomains))
	for _, domain := range project.CustomDomains {
		domains = append(domains, ui.ProjectDomainItem{
			ID:            domain.ID,
			Hostname:      domain.Hostname,
			IsPrimary:     domain.IsPrimary,
			DNSVerified:   !domain.DNSVerifiedAt.IsZero(),
			DNSVerifiedAt: formatRelativeTime(domain.DNSVerifiedAt),
		})
	}
	deploys := make([]ui.ProjectDeployItem, 0, len(project.Deploys))
	for _, deploy := range project.Deploys {
		deploys = append(deploys, ui.ProjectDeployItem{
			ID:          deploy.ID,
			ImageDigest: deploy.ImageDigest,
			ImageRef:    deploy.ImageRef,
			Status:      deploy.Status,
			Trigger:     deploy.Trigger,
			Actor:       deploy.Actor,
			StartedAt:   formatRelativeTime(deploy.StartedAt),
			FinishedAt:  formatRelativeTime(deploy.FinishedAt),
			Error:       deploy.Error,
			CanRollback: strings.TrimSpace(deploy.ImageDigest) != "",
		})
	}
	runtimeData := ui.ProjectRuntimeItem{
		Available:     false,
		Status:        project.Status,
		OpenAppStatus: project.Status,
	}
	if s.projects != nil {
		snapshot, err := s.projects.RuntimeSnapshot(r.Context(), project)
		if err != nil {
			s.logger.Warn("load project runtime snapshot", "project_id", project.ID, "error", err)
			runtimeData.ErrorMessage = "Runtime snapshot is unavailable right now."
		} else {
			runtimeData = projectRuntimeData(snapshot)
		}
	}
	errorTitle, errorHints := describeUIError(errorMessage)

	data := ui.ProjectPageData{
		PageTitle:         "CaddyTower | " + project.Name,
		Headline:          "Edit " + project.Name,
		CSRFToken:         s.auth.EnsureCSRFCookie(w, r),
		CurrentUser:       currentUser,
		InfoMessage:       infoMessage,
		ErrorMessage:      errorMessage,
		ErrorTitle:        errorTitle,
		ErrorHints:        errorHints,
		Project:           form,
		ProjectMeta:       projectListItem(project),
		WebhookURL:        strings.TrimRight(s.cfg.PublicBaseURL, "/") + "/api/webhooks/deploy/" + project.Slug,
		WebhookSecret:     project.WebhookSecret,
		PrimaryDomain:     project.PrimaryDomain,
		PendingImage:      project.Status == "pending image",
		PendingImageHint:  "Waiting for the first image push. Push to your default branch and CaddyTower will deploy when the webhook arrives.",
		GeneratedDomain:   project.FullDomain,
		ExpectedDomainDNS: s.projectsExpectedDomainTarget(r.Context(), project),
		HealthCheckTarget: projectHealthTarget(project),
		Runtime:           runtimeData,
		Domains:           domains,
		Deploys:           deploys,
		EnvItems:          maskEnvItems(project.Env),
		DeployEventsURL:   "/projects/" + project.ID + "/events/stream",
		Attachments:       attachments,
		AttachmentForm:    ui.DBAttachmentFormData{Engine: "pg", EnvVarName: "DATABASE_URL"},
	}
	if strings.TrimSpace(project.GitHubRepoFullName) != "" {
		data.WorkflowSecretName = importWorkflowSecretName
		data.WorkflowSnippet = buildGitHubWebhookSnippet(strings.TrimRight(s.cfg.PublicBaseURL, "/")+"/api/webhooks/deploy/"+project.Slug, importWorkflowSecretName)
	}

	if err := s.ui.Render(w, "project.gohtml", data); err != nil {
		s.logger.Error("render project page", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

func (s *Server) renderSettingsPage(w http.ResponseWriter, r *http.Request, currentUser, errorMessage, infoMessage string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	commonData, err := s.loadDashboardData(r.Context())
	if err != nil {
		s.logger.Error("load settings page", "error", err)
		http.Error(w, "failed to load settings page", http.StatusInternalServerError)
		return
	}
	errorTitle, errorHints := describeUIError(errorMessage)

	data := ui.SettingsPageData{
		PageTitle: "CaddyTower | Settings",
		Headline:  "Settings and operations",
		CSRFToken: s.auth.EnsureCSRFCookie(w, r),
		Version:   s.version,
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
		CurrentUser:               currentUser,
		InfoMessage:               infoMessage,
		ErrorMessage:              errorMessage,
		ErrorTitle:                errorTitle,
		ErrorHints:                errorHints,
		Settings:                  commonData.settings,
		GitHub:                    s.gitHubStatusData(r.Context()),
		Projects:                  commonData.projects,
		Backups:                   commonData.backups,
		BackupsEnabled:            s.cfg.BackupsEnabled,
		BackupsRetentionDays:      s.cfg.BackupsRetentionDays,
		BackupsScheduleUTC:        s.cfg.BackupsScheduleUTC,
		BackupsIncludeEngineDumps: s.cfg.BackupsIncludeEngineDumps,
		VPSStatus:                 s.vpsStatusData(),
		AuditFilter:               strings.TrimSpace(r.URL.Query().Get("audit")),
	}
	if s.projects != nil {
		entries, err := s.projects.AuditLogs(r.Context(), data.AuditFilter, 50)
		if err != nil {
			s.logger.Warn("load audit logs", "error", err)
		} else {
			for _, entry := range entries {
				data.AuditLogs = append(data.AuditLogs, ui.AuditLogItem{
					Timestamp: formatRelativeTime(entry.Timestamp),
					UserEmail: entry.UserEmail,
					Action:    entry.Action,
					Target:    entry.Target,
					Payload:   entry.Payload,
				})
			}
		}
	}

	if err := s.ui.Render(w, "settings.gohtml", data); err != nil {
		s.logger.Error("render settings page", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

func (s *Server) gitHubStatusData(ctx context.Context) ui.GitHubStatusData {
	if s.github == nil {
		return ui.GitHubStatusData{}
	}
	status, err := s.github.Status(ctx)
	if err != nil {
		s.logger.Warn("load github status", "error", err)
		return ui.GitHubStatusData{Configured: s.github.Configured()}
	}
	data := ui.GitHubStatusData{
		Configured: status.Configured,
		Connected:  len(status.Installations) > 0,
		InstallURL: status.InstallURL,
	}
	for _, installation := range status.Installations {
		data.Installations = append(data.Installations, ui.GitHubInstallationItem{
			InstallationID: installation.InstallationID,
			AccountLogin:   installation.AccountLogin,
			AccountType:    installation.AccountType,
			ManageURL:      installation.ManageURL,
			CreatedAt:      installation.CreatedAt.Format("2006-01-02 15:04:05 MST"),
		})
	}
	return data
}

func (s *Server) renderProjectWithFallback(w http.ResponseWriter, r *http.Request, currentUser, projectID string, form ui.ProjectFormData, errorMessage, infoMessage string) {
	project, _, err := s.projects.GetProject(r.Context(), projectID)
	if err != nil {
		s.renderProjectError(w, err)
		return
	}
	if form.Action == "" {
		form = ui.ProjectFormData{
			ID:                        project.ID,
			Action:                    "/projects/" + project.ID,
			SubmitLabel:               "Save and deploy",
			Type:                      project.Type,
			Name:                      project.Name,
			Slug:                      project.Slug,
			ImageRef:                  project.ImageRef,
			Subdomain:                 project.Subdomain,
			InternalPort:              project.InternalPort,
			HealthCheckPath:           project.HealthCheckPath,
			HealthCheckTimeoutSeconds: project.HealthCheckTimeoutSeconds,
			PortMappingsText:          projects.PortMappingsText(project.Ports),
			WatchtowerEnabled:         project.WatchtowerEnabled,
			EnvText:                   project.EnvText,
			SlugReadOnly:              true,
			TypeReadOnly:              true,
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
	healthTimeoutSeconds := 0
	healthTimeoutValue := strings.TrimSpace(r.FormValue("health_timeout_seconds"))
	if healthTimeoutValue != "" {
		parsedTimeout, err := strconv.Atoi(healthTimeoutValue)
		if err != nil {
			form := projectFormDataFromRequest(r, projectID, projectType)
			form.TypeReadOnly = fixedType != ""
			return projects.WebProjectInput{}, form, fmt.Errorf("health check timeout must be a number")
		}
		healthTimeoutSeconds = parsedTimeout
	}

	input := projects.WebProjectInput{
		Type:                      projectType,
		ID:                        projectID,
		Name:                      strings.TrimSpace(r.FormValue("name")),
		Slug:                      strings.TrimSpace(r.FormValue("slug")),
		ImageRef:                  strings.TrimSpace(r.FormValue("image_ref")),
		Subdomain:                 strings.TrimSpace(r.FormValue("subdomain")),
		InternalPort:              internalPort,
		HealthCheckPath:           strings.TrimSpace(r.FormValue("health_check_path")),
		HealthCheckTimeoutSeconds: healthTimeoutSeconds,
		PortMappingsText:          r.FormValue("port_mappings"),
		WatchtowerEnabled:         projects.ParseBoolCheckbox(r.FormValue("watchtower_enabled")),
		EnvText:                   r.FormValue("env_text"),
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
		HealthCheckPath:   strings.TrimSpace(r.FormValue("health_check_path")),
		PortMappingsText:  r.FormValue("port_mappings"),
		WatchtowerEnabled: projects.ParseBoolCheckbox(r.FormValue("watchtower_enabled")),
		EnvText:           r.FormValue("env_text"),
	}
	if internalPort, err := strconv.Atoi(strings.TrimSpace(r.FormValue("internal_port"))); err == nil {
		input.InternalPort = internalPort
	}
	if healthTimeout, err := strconv.Atoi(strings.TrimSpace(r.FormValue("health_timeout_seconds"))); err == nil {
		input.HealthCheckTimeoutSeconds = healthTimeout
	}
	return projectFormDataFromInput(input, projectID)
}

func projectFormDataFromInput(input projects.WebProjectInput, projectID string) ui.ProjectFormData {
	form := ui.ProjectFormData{
		ID:                        projectID,
		Action:                    "/projects/" + projectID,
		SubmitLabel:               "Save and deploy",
		Type:                      input.Type,
		Name:                      input.Name,
		Slug:                      input.Slug,
		ImageRef:                  input.ImageRef,
		Subdomain:                 input.Subdomain,
		InternalPort:              input.InternalPort,
		HealthCheckPath:           input.HealthCheckPath,
		HealthCheckTimeoutSeconds: input.HealthCheckTimeoutSeconds,
		PortMappingsText:          input.PortMappingsText,
		WatchtowerEnabled:         input.WatchtowerEnabled,
		EnvText:                   input.EnvText,
		SlugReadOnly:              projectID != "",
	}
	if form.Type == "" {
		form.Type = "web"
	}
	if projectID == "" {
		form.Action = "/projects"
		form.SubmitLabel = "Create and deploy"
	}
	if form.Type == "web" && form.HealthCheckTimeoutSeconds == 0 {
		form.HealthCheckTimeoutSeconds = 5
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

func formatRelativeTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format("2006-01-02 15:04:05 MST")
}

func maskEnvItems(values map[string]string) []ui.ProjectEnvItem {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	items := make([]ui.ProjectEnvItem, 0, len(keys))
	for _, key := range keys {
		value := values[key]
		sensitive := looksSensitiveEnvKey(key)
		masked := value
		if sensitive {
			masked = maskSecretValue(value)
		}
		items = append(items, ui.ProjectEnvItem{
			Key:         key,
			Value:       value,
			MaskedValue: masked,
			Sensitive:   sensitive,
		})
	}
	return items
}

func looksSensitiveEnvKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	for _, token := range []string{"secret", "token", "key", "password", "passwd"} {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}

func maskSecretValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 4 {
		return strings.Repeat("*", len(value))
	}
	return value[:2] + strings.Repeat("*", len(value)-4) + value[len(value)-2:]
}

func (s *Server) projectsExpectedDomainTarget(ctx context.Context, project projects.Project) string {
	if s.projects == nil {
		return project.FullDomain
	}
	dashboard, err := s.projects.Dashboard(ctx)
	if err != nil {
		return project.FullDomain
	}
	if strings.TrimSpace(dashboard.Settings.OriginHostname) != "" {
		return dashboard.Settings.OriginHostname
	}
	return project.FullDomain
}

func projectHealthTarget(project projects.Project) string {
	if strings.TrimSpace(project.HealthCheckPath) == "" || project.InternalPort == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d%s", project.ContainerName, project.InternalPort, project.HealthCheckPath)
}

func projectRuntimeData(snapshot projects.ProjectRuntimeSnapshot) ui.ProjectRuntimeItem {
	item := ui.ProjectRuntimeItem{
		Available:     true,
		Status:        snapshot.Status,
		LastChecked:   formatRelativeTime(snapshot.ReadAt),
		CPUSummary:    fmt.Sprintf("%.1f%%", snapshot.CPUPercent),
		MemoryUsedPct: snapshot.MemoryPercent,
		Warnings:      append([]string(nil), snapshot.Warnings...),
		OpenAppStatus: snapshot.Status,
	}
	if snapshot.ReadAt.IsZero() {
		item.LastChecked = "no live sample yet"
	}
	if snapshot.MemoryLimitBytes > 0 {
		item.MemorySummary = fmt.Sprintf("%s / %s (%d%%)", humanSizeUint(snapshot.MemoryUsageBytes), humanSizeUint(snapshot.MemoryLimitBytes), snapshot.MemoryPercent)
	} else {
		item.MemorySummary = humanSizeUint(snapshot.MemoryUsageBytes)
	}
	item.NetworkSummary = fmt.Sprintf("%s in / %s out", humanSizeUint(snapshot.NetworkRxBytes), humanSizeUint(snapshot.NetworkTxBytes))
	item.BlockIOSummary = fmt.Sprintf("%s read / %s write", humanSizeUint(snapshot.BlockReadBytes), humanSizeUint(snapshot.BlockWriteBytes))
	item.ProcessSummary = fmt.Sprintf("%d processes", snapshot.PIDs)
	if snapshot.Status != "running" && snapshot.ReadAt.IsZero() {
		item.CPUSummary = "n/a"
		item.MemorySummary = "n/a"
		item.NetworkSummary = "n/a"
		item.BlockIOSummary = "n/a"
		item.ProcessSummary = "n/a"
		item.OpenAppStatus = "container not running"
		item.LastChecked = "container is not running"
	}
	return item
}

func describeUIError(message string) (string, []string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return "", nil
	}

	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "docker-compose projects need a manual image flow") || strings.Contains(lower, "docker-compose"):
		return "This repository needs the manual image flow", []string{
			"Build and publish an image first, then create the project with the manual image path instead of the GitHub import flow.",
			"If the repo already publishes to GHCR, point CaddyTower at that image reference directly.",
		}
	case strings.Contains(lower, "health check failed") || strings.Contains(lower, "project saved but deployment failed") || strings.Contains(lower, "project updated but deployment failed") || strings.Contains(lower, "project redeploy failed") || strings.Contains(lower, "rolled back"):
		return "Deployment did not finish cleanly", []string{
			"Open the live logs and deploy events on the project page to see whether the app crashed, never started, or failed its health endpoint.",
			"Check that the image exposes the expected internal port and that the health path returns HTTP 200 after boot.",
		}
	case strings.Contains(lower, "root domain") || strings.Contains(lower, "origin hostname"):
		return "Routing setup is incomplete", []string{
			"Finish the root domain and origin hostname in Settings before deploying or verifying public web routes.",
			"If Cloudflare fronts the domain, make sure the origin hostname points at this VPS or shared Caddy entrypoint.",
		}
	case strings.Contains(lower, "cloudflare"):
		return "Cloudflare settings need attention", []string{
			"Verify the API token has DNS edit access for the right zone and that the zone ID matches the public domain you want to manage.",
			"If you only need local testing, save the project first and finish DNS later from Settings.",
		}
	case strings.Contains(lower, "docker integration is unavailable") || strings.Contains(lower, "docker logs are unavailable") || strings.Contains(lower, "pull image") || strings.Contains(lower, "recreate container") || strings.Contains(lower, "inspect container"):
		return "Docker could not complete the request", []string{
			"Make sure the controller still has access to the Docker socket and that the Docker daemon is healthy on the VPS.",
			"If the image pull failed, confirm the image reference exists and is publicly reachable or that the registry credentials are configured elsewhere.",
		}
	case strings.Contains(lower, "caddy"):
		return "Caddy routing could not be updated", []string{
			"Check that shared Caddy is reachable from the controller on the internal admin URL and that the root domain/origin are configured.",
			"Until Caddy updates cleanly, the container may be running without a public route.",
		}
	case strings.Contains(lower, "subdomain") || strings.Contains(lower, "slug") || strings.Contains(lower, "internal port") || strings.Contains(lower, "health check path") || strings.Contains(lower, "health check timeout") || strings.Contains(lower, "port mapping") || strings.Contains(lower, "image reference is required") || strings.Contains(lower, "project name is required"):
		return "Some project settings need fixing", []string{
			"Review the project form values: slug, subdomain, image, port, and health check fields must all match the app you are deploying.",
			"For network services, published ports must stay unique and use the host:container format on separate lines.",
		}
	case strings.Contains(lower, "dns for ") || strings.Contains(lower, "custom domains") || strings.Contains(lower, "domain hostname is required"):
		return "The custom domain is not ready yet", []string{
			"Point the hostname at the expected origin shown on the project page, then run Verify again once DNS has propagated.",
			"Keep the generated domain as the fallback until the custom hostname passes verification.",
		}
	case strings.Contains(lower, "reusable image pin"):
		return "This deploy cannot be rolled back automatically", []string{
			"Only deploys with a stored local image pin can be used for rollback.",
			"Redeploy from a known-good image reference if you need to recover immediately.",
		}
	case strings.Contains(lower, "database engine"):
		return "The shared database service is unavailable", []string{
			"Start or repair the shared database engine before attaching, rotating, or detaching credentials.",
			"If the database changed but redeploy failed, the new connection details may already be stored on the project page.",
		}
	default:
		return "Action failed", []string{
			"Retry once after checking the affected page for logs, status, or missing setup fields.",
			"If this keeps happening, the raw error below is the exact backend detail to investigate next.",
		}
	}
}

func (s *Server) vpsStatusData() ui.VPSStatusData {
	if s.monitor == nil {
		return ui.VPSStatusData{
			Available:     false,
			ErrorMessage:  "VPS status monitor is not available.",
			RAMThreshold:  s.cfg.VPSRAMFreeWarnPercent,
			DiskThreshold: s.cfg.VPSDiskFreeWarnPercent,
		}
	}
	status, err := s.monitor.Snapshot()
	if err != nil {
		s.logger.Warn("collect vps status", "error", err)
		return ui.VPSStatusData{
			Available:     false,
			ErrorMessage:  err.Error(),
			RAMThreshold:  s.cfg.VPSRAMFreeWarnPercent,
			DiskThreshold: s.cfg.VPSDiskFreeWarnPercent,
		}
	}
	return ui.VPSStatusData{
		Available:       true,
		MemorySummary:   fmt.Sprintf("%s / %s", humanSizeUint(status.Memory.UsedBytes), humanSizeUint(status.Memory.TotalBytes)),
		MemoryUsedPct:   status.Memory.UsedPercent,
		MemoryFreePct:   status.Memory.FreePercent,
		DiskSummary:     fmt.Sprintf("%s / %s", humanSizeUint(status.Disk.UsedBytes), humanSizeUint(status.Disk.TotalBytes)),
		DiskUsedPct:     status.Disk.UsedPercent,
		DiskFreePct:     status.Disk.FreePercent,
		DiskPath:        status.DiskPath,
		WarningCount:    len(status.Warnings),
		Warnings:        status.Warnings,
		RAMThreshold:    status.RAMThreshold,
		DiskThreshold:   status.DiskThreshold,
		EmailConfigured: status.EmailConfigured,
		EmailTo:         status.EmailTo,
		CheckedAt:       status.CollectedAt.Format("2006-01-02 15:04:05 MST"),
	}
}

func humanSize(size int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case size >= GB:
		return fmt.Sprintf("%.1f GB", float64(size)/GB)
	case size >= MB:
		return fmt.Sprintf("%.1f MB", float64(size)/MB)
	case size >= KB:
		return fmt.Sprintf("%.1f KB", float64(size)/KB)
	default:
		return fmt.Sprintf("%d B", size)
	}
}

func humanSizeUint(size uint64) string {
	if size > uint64(^uint(0)>>1) {
		return fmt.Sprintf("%.1f GB", float64(size)/(1024*1024*1024))
	}
	return humanSize(int64(size))
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
