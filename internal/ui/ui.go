package ui

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"time"

	"caddytower/internal/version"
)

//go:embed templates/*.gohtml static/*
var embeddedFiles embed.FS

type UI struct {
	templates *template.Template
	assets    fs.FS
}

type ConfigSummary struct {
	HTTPAddr      string
	PublicBaseURL string
	DataDir       string
	StateDBPath   string
	CaddyAdminURL string
	RootDomain    string
	DockerHost    string
	MasterKeySet  bool
}

type HomePageData struct {
	GeneratedAt time.Time
	PageTitle   string
	Headline    string
	CSRFToken   string
	Version     version.Info
	Config      ConfigSummary
	CurrentUser string
}

type SetupPageData struct {
	PageTitle    string
	Headline     string
	CSRFToken    string
	Email        string
	ManualKey    string
	OTPAuthURL   string
	ErrorMessage string
}

type LoginPageData struct {
	PageTitle    string
	Headline     string
	CSRFToken    string
	Email        string
	ErrorMessage string
}

func New() (*UI, error) {
	templates, err := template.ParseFS(embeddedFiles, "templates/*.gohtml")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	assets, err := fs.Sub(embeddedFiles, "static")
	if err != nil {
		return nil, fmt.Errorf("sub static fs: %w", err)
	}

	return &UI{
		templates: templates,
		assets:    assets,
	}, nil
}

func (u *UI) Assets() fs.FS {
	return u.assets
}

func (u *UI) Render(w io.Writer, name string, data any) error {
	return u.templates.ExecuteTemplate(w, name, data)
}
