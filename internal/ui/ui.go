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

type GitHubInstallationItem struct {
	InstallationID int64
	AccountLogin   string
	AccountType    string
	ManageURL      string
	CreatedAt      string
}

type GitHubStatusData struct {
	Configured           bool
	Connected            bool
	InstallURL           string
	Installations        []GitHubInstallationItem
	AppID                string
	AppSlug              string
	WebhookSecret        string
	WebhookSecretPresent bool
	PrivateKeyPresent    bool
	StoredInApp          bool
}

type ProjectFormData struct {
	ID                        string
	Action                    string
	SubmitLabel               string
	Type                      string
	Name                      string
	Slug                      string
	ImageRef                  string
	Subdomain                 string
	InternalPort              int
	HealthCheckPath           string
	HealthCheckTimeoutSeconds int
	PortMappingsText          string
	WatchtowerEnabled         bool
	EnvText                   string
	SlugReadOnly              bool
	TypeReadOnly              bool
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

type ProjectDomainItem struct {
	ID            int64
	Hostname      string
	IsPrimary     bool
	DNSVerified   bool
	DNSVerifiedAt string
}

type ProjectDomainFormData struct {
	Hostname    string
	MakePrimary bool
}

type ProjectDeployItem struct {
	ID          int64
	ImageDigest string
	ImageRef    string
	Status      string
	Trigger     string
	Actor       string
	StartedAt   string
	FinishedAt  string
	Error       string
	CanRollback bool
}

type ProjectEnvItem struct {
	Key         string
	Value       string
	MaskedValue string
	Sensitive   bool
}

type ProjectRuntimeItem struct {
	Available      bool
	Status         string
	LastChecked    string
	CPUSummary     string
	MemorySummary  string
	MemoryUsedPct  int
	NetworkSummary string
	BlockIOSummary string
	ProcessSummary string
	OpenAppStatus  string
	Warnings       []string
	ErrorMessage   string
}

type HomePageData struct {
	GeneratedAt               time.Time
	PageTitle                 string
	Headline                  string
	CSRFToken                 string
	Version                   version.Info
	Config                    ConfigSummary
	CurrentUser               string
	InfoMessage               string
	ErrorMessage              string
	ErrorTitle                string
	ErrorHints                []string
	Settings                  SettingsFormData
	CreateForm                ProjectFormData
	Projects                  []ProjectListItem
	Backups                   []BackupItem
	BackupsEnabled            bool
	BackupsRetentionDays      int
	BackupsScheduleUTC        string
	BackupsIncludeEngineDumps bool
	Requirements              RequirementsStatusData
	VPSStatus                 VPSStatusData
	ShowOnboarding            bool
	OpenProjectDialog         bool
	DomainConfigured          bool
	EffectivePublicBaseURL    string
	PublicURLReady            bool
	PublicAdminHost           string
	SuggestedPublicBaseURL    string
	NeedsSetup                bool
	GitHubConfigured          bool
	GitHubConnected           bool
}

type BackupItem struct {
	Name      string
	CreatedAt string
	Size      string
}

type AuditLogItem struct {
	Timestamp string
	UserEmail string
	Action    string
	Target    string
	Payload   string
}

type VPSStatusData struct {
	Available       bool
	ErrorMessage    string
	MemorySummary   string
	MemoryUsedPct   int
	MemoryFreePct   int
	DiskSummary     string
	DiskUsedPct     int
	DiskFreePct     int
	DiskPath        string
	WarningCount    int
	Warnings        []string
	RAMThreshold    int
	DiskThreshold   int
	EmailConfigured bool
	EmailTo         string
	CheckedAt       string
}

type RequirementsStatusData struct {
	Available    bool
	ErrorMessage string
	HealthyCount int
	WarningCount int
	FailureCount int
	Checks       []RequirementCheckData
}

type RequirementCheckData struct {
	Name    string
	Status  string
	Summary string
	Detail  string
}

type SetupPageData struct {
	PageTitle     string
	Headline      string
	CSRFToken     string
	Email         string
	ManualKey     string
	OTPAuthURL    string
	QRCodeDataURL template.URL
	ErrorMessage  string
}

type LoginPageData struct {
	PageTitle    string
	Headline     string
	CSRFToken    string
	Email        string
	ErrorMessage string
}

type SettingsPageData struct {
	PageTitle                 string
	Headline                  string
	CSRFToken                 string
	Version                   version.Info
	Config                    ConfigSummary
	CurrentUser               string
	InfoMessage               string
	ErrorMessage              string
	ErrorTitle                string
	ErrorHints                []string
	Settings                  SettingsFormData
	EffectiveRootDomain       string
	EffectivePublicBaseURL    string
	PublicURLReady            bool
	PublicAdminHost           string
	SuggestedPublicBaseURL    string
	GitHub                    GitHubStatusData
	Projects                  []ProjectListItem
	Backups                   []BackupItem
	BackupsEnabled            bool
	BackupsRetentionDays      int
	BackupsScheduleUTC        string
	BackupsIncludeEngineDumps bool
	VPSStatus                 VPSStatusData
	AuditFilter               string
	AuditLogs                 []AuditLogItem
	ControllerUpdate          ControllerUpdateData
	RestartPrompt             RestartPromptData
}

type ControllerUpdateData struct {
	Checked             bool
	CurrentVersion      string
	CurrentImage        string
	LatestRelease       string
	LatestReleaseURL    string
	StatusMessage       string
	UpdateAvailable     bool
	CanTrigger          bool
	ButtonLabel         string
	TargetImage         string
	TrackingLatest      bool
	CurrentChannel      string
	CurrentChannelIsDev bool
}

type RestartPromptData struct {
	Visible     bool
	Title       string
	Message     string
	ActionLabel string
}

type GitHubRepositoryItem struct {
	InstallationID int64
	Name           string
	FullName       string
	DefaultBranch  string
	HTMLURL        string
	Selected       bool
}

type ImportDetectionData struct {
	Ready                bool
	RepoName             string
	RepoFullName         string
	RepoURL              string
	DefaultBranch        string
	InstallationID       int64
	DockerfileFound      bool
	DockerComposeFound   bool
	WorkflowDetected     bool
	WorkflowPaths        []string
	FrameworkHint        string
	SuggestedImageRef    string
	ImageReachable       bool
	ImageCheckMessage    string
	WillOpenWorkflowPR   bool
	SecretName           string
	WebhookSnippet       string
	UnsupportedReason    string
	WorkflowPRFilePath   string
	WorkflowPRBranchName string
}

type ImportPageData struct {
	PageTitle            string
	Headline             string
	CSRFToken            string
	CurrentUser          string
	InfoMessage          string
	ErrorMessage         string
	ErrorTitle           string
	ErrorHints           []string
	GitHub               GitHubStatusData
	SelectedInstallation int64
	Query                string
	Repositories         []GitHubRepositoryItem
	SelectedRepoFullName string
	Detection            ImportDetectionData
	Project              ProjectFormData
}

type ProjectPageData struct {
	PageTitle          string
	Headline           string
	CSRFToken          string
	CurrentUser        string
	InfoMessage        string
	ErrorMessage       string
	ErrorTitle         string
	ErrorHints         []string
	Project            ProjectFormData
	ProjectMeta        ProjectListItem
	WebhookURL         string
	WebhookSecret      string
	WorkflowSecretName string
	WorkflowSnippet    string
	PrimaryDomain      string
	PendingImage       bool
	PendingImageHint   string
	GeneratedDomain    string
	ExpectedDomainDNS  string
	HealthCheckTarget  string
	Runtime            ProjectRuntimeItem
	Domains            []ProjectDomainItem
	DomainForm         ProjectDomainFormData
	Deploys            []ProjectDeployItem
	EnvItems           []ProjectEnvItem
	DeployEventsURL    string
	Attachments        []DBAttachmentItem
	AttachmentForm     DBAttachmentFormData
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
