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

type SettingsFormData struct {
	RootDomain             string
	OriginHostname         string
	CloudflareZoneID       string
	CloudflareTokenPresent bool
	CloudflareProxied      bool
}

type ProjectFormData struct {
	ID                string
	Action            string
	SubmitLabel       string
	Type              string
	Name              string
	Slug              string
	ImageRef          string
	Subdomain         string
	InternalPort      int
	PortMappingsText  string
	WatchtowerEnabled bool
	EnvText           string
	SlugReadOnly      bool
	TypeReadOnly      bool
}

type ProjectListItem struct {
	ID                string
	Name              string
	Type              string
	Slug              string
	ImageRef          string
	Subdomain         string
	FullDomain        string
	ContainerName     string
	InternalPort      int
	PortMappingsText  string
	EndpointSummary   string
	WatchtowerEnabled bool
	Status            string
}

type DBAttachmentItem struct {
	ID             int64
	Engine         string
	DBName         string
	DBUser         string
	EnvVarName     string
	Host           string
	Port           int
	ConnectionHint string
}

type DBAttachmentFormData struct {
	Engine     string
	EnvVarName string
}

type HomePageData struct {
	GeneratedAt  time.Time
	PageTitle    string
	Headline     string
	CSRFToken    string
	Version      version.Info
	Config       ConfigSummary
	CurrentUser  string
	InfoMessage  string
	ErrorMessage string
	Settings     SettingsFormData
	CreateForm   ProjectFormData
	Projects     []ProjectListItem
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

type ProjectPageData struct {
	PageTitle      string
	Headline       string
	CSRFToken      string
	CurrentUser    string
	InfoMessage    string
	ErrorMessage   string
	Project        ProjectFormData
	ProjectMeta    ProjectListItem
	WebhookURL     string
	WebhookSecret  string
	Attachments    []DBAttachmentItem
	AttachmentForm DBAttachmentFormData
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
