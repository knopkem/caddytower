package github

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"caddytower/internal/store"

	"github.com/google/uuid"
)

type Config struct {
	AppID          int64
	AppSlug        string
	PrivateKeyPath string
	WebhookSecret  string
	APIBaseURL     string
	WebBaseURL     string
}

type Service struct {
	cfg    Config
	store  *store.Store
	client *http.Client
	now    func() time.Time

	mu         sync.Mutex
	tokenCache map[int64]cachedInstallationToken
	keyOnce    sync.Once
	key        *rsa.PrivateKey
	keyErr     error
}

type cachedInstallationToken struct {
	token     string
	expiresAt time.Time
}

type Status struct {
	Configured    bool
	InstallURL    string
	Installations []Installation
}

type Installation struct {
	ID             string
	InstallationID int64
	AccountLogin   string
	AccountType    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ManageURL      string
}

type Repository struct {
	ID            int64
	Name          string
	FullName      string
	DefaultBranch string
	Private       bool
	HTMLURL       string
}

type Branch struct {
	Name string
}

type PullRequestInput struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
}

type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

type ContentEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
}

type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github api %s %s returned %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
}

func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

func New(cfg Config, stateStore *store.Store, client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		cfg:        cfg,
		store:      stateStore,
		client:     client,
		now:        time.Now,
		tokenCache: map[int64]cachedInstallationToken{},
	}
}

func (s *Service) Configured() bool {
	return s != nil &&
		s.cfg.AppID > 0 &&
		s.cfg.AppSlug != "" &&
		s.cfg.PrivateKeyPath != "" &&
		s.cfg.WebhookSecret != ""
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	status := Status{Configured: s.Configured()}
	if !status.Configured {
		return status, nil
	}

	status.InstallURL = s.InstallURL()
	records, err := s.store.ListGitHubInstallations(ctx)
	if err != nil {
		return Status{}, err
	}
	status.Installations = make([]Installation, 0, len(records))
	for _, record := range records {
		status.Installations = append(status.Installations, Installation{
			ID:             record.ID,
			InstallationID: record.InstallationID,
			AccountLogin:   record.AccountLogin,
			AccountType:    record.AccountType,
			CreatedAt:      record.CreatedAt,
			UpdatedAt:      record.UpdatedAt,
			ManageURL:      s.ManageInstallationURL(record.InstallationID),
		})
	}
	return status, nil
}

func (s *Service) InstallURL() string {
	if !s.Configured() {
		return ""
	}
	return strings.TrimRight(s.cfg.WebBaseURL, "/") + "/apps/" + s.cfg.AppSlug + "/installations/new"
}

func (s *Service) ManageInstallationURL(installationID int64) string {
	if installationID == 0 {
		return ""
	}
	return strings.TrimRight(s.cfg.WebBaseURL, "/") + "/settings/installations/" + fmt.Sprintf("%d", installationID)
}

func (s *Service) VerifyWebhookSignature(provided string, payload []byte) bool {
	if !s.Configured() || strings.TrimSpace(provided) == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.cfg.WebhookSecret))
	mac.Write(payload)
	expected := "sha256=" + fmt.Sprintf("%x", mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(provided)))
}

func (s *Service) HandleWebhook(ctx context.Context, event, signature string, payload []byte) (string, error) {
	if !s.VerifyWebhookSignature(signature, payload) {
		return "", fmt.Errorf("invalid github webhook signature")
	}

	switch strings.TrimSpace(event) {
	case "installation", "installation_repositories":
		var envelope installationWebhookPayload
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return "", fmt.Errorf("decode github webhook: %w", err)
		}
		installationID := envelope.Installation.ID
		if installationID == 0 {
			return "ignored github webhook without installation id", nil
		}
		accountLogin := envelope.Installation.Account.Login
		accountType := envelope.Installation.Account.Type
		if accountLogin == "" {
			accountLogin = envelope.Account.Login
			accountType = envelope.Account.Type
		}
		switch envelope.Action {
		case "deleted", "suspend":
			s.mu.Lock()
			delete(s.tokenCache, installationID)
			s.mu.Unlock()
			if err := s.store.DeleteGitHubInstallationByInstallationID(ctx, installationID); err != nil {
				return "", err
			}
			return fmt.Sprintf("removed installation %d", installationID), nil
		default:
			if accountLogin == "" {
				return "ignored github webhook without account login", nil
			}
			if err := s.store.UpsertGitHubInstallation(ctx, store.GitHubInstallationRecord{
				ID:             uuid.NewString(),
				InstallationID: installationID,
				AccountLogin:   accountLogin,
				AccountType:    accountType,
			}); err != nil {
				return "", err
			}
			return fmt.Sprintf("stored installation %d", installationID), nil
		}
	default:
		return "ignored unsupported github event", nil
	}
}

func (s *Service) DisconnectInstallation(ctx context.Context, installationID int64) error {
	s.mu.Lock()
	delete(s.tokenCache, installationID)
	s.mu.Unlock()
	return s.store.DeleteGitHubInstallationByInstallationID(ctx, installationID)
}

func (s *Service) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	if !s.Configured() {
		return "", fmt.Errorf("github app is not configured")
	}

	now := s.now()
	s.mu.Lock()
	if cached, ok := s.tokenCache[installationID]; ok && cached.token != "" && now.Before(cached.expiresAt.Add(-time.Minute)) {
		s.mu.Unlock()
		return cached.token, nil
	}
	s.mu.Unlock()

	jwtToken, err := s.appJWT()
	if err != nil {
		return "", err
	}

	var response struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := s.doJSON(ctx, http.MethodPost, path.Join("/app/installations", fmt.Sprintf("%d", installationID), "access_tokens"), jwtToken, nil, &response); err != nil {
		return "", err
	}

	s.mu.Lock()
	s.tokenCache[installationID] = cachedInstallationToken{
		token:     response.Token,
		expiresAt: response.ExpiresAt,
	}
	s.mu.Unlock()
	return response.Token, nil
}

func (s *Service) ListRepositories(ctx context.Context, installationID int64) ([]Repository, error) {
	token, err := s.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var response struct {
		Repositories []struct {
			ID            int64  `json:"id"`
			Name          string `json:"name"`
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
			Private       bool   `json:"private"`
			HTMLURL       string `json:"html_url"`
		} `json:"repositories"`
	}
	if err := s.doJSON(ctx, http.MethodGet, "/installation/repositories", token, nil, &response); err != nil {
		return nil, err
	}
	repos := make([]Repository, 0, len(response.Repositories))
	for _, repo := range response.Repositories {
		repos = append(repos, Repository{
			ID:            repo.ID,
			Name:          repo.Name,
			FullName:      repo.FullName,
			DefaultBranch: repo.DefaultBranch,
			Private:       repo.Private,
			HTMLURL:       repo.HTMLURL,
		})
	}
	return repos, nil
}

func (s *Service) GetRepository(ctx context.Context, installationID int64, owner, repo string) (Repository, error) {
	token, err := s.InstallationToken(ctx, installationID)
	if err != nil {
		return Repository{}, err
	}
	var response struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
		HTMLURL       string `json:"html_url"`
	}
	if err := s.doJSON(ctx, http.MethodGet, "/repos/"+owner+"/"+repo, token, nil, &response); err != nil {
		return Repository{}, err
	}
	return Repository{
		ID:            response.ID,
		Name:          response.Name,
		FullName:      response.FullName,
		DefaultBranch: response.DefaultBranch,
		Private:       response.Private,
		HTMLURL:       response.HTMLURL,
	}, nil
}

func (s *Service) ListBranches(ctx context.Context, installationID int64, owner, repo string) ([]Branch, error) {
	token, err := s.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var response []struct {
		Name string `json:"name"`
	}
	if err := s.doJSON(ctx, http.MethodGet, "/repos/"+owner+"/"+repo+"/branches", token, nil, &response); err != nil {
		return nil, err
	}
	branches := make([]Branch, 0, len(response))
	for _, branch := range response {
		branches = append(branches, Branch{Name: branch.Name})
	}
	return branches, nil
}

func (s *Service) GetFileContent(ctx context.Context, installationID int64, owner, repo, filePath, ref string) ([]byte, error) {
	token, err := s.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}

	requestPath := "/repos/" + owner + "/" + repo + "/contents/" + strings.TrimLeft(filePath, "/")
	if strings.TrimSpace(ref) != "" {
		requestPath += "?ref=" + url.QueryEscape(strings.TrimSpace(ref))
	}

	var response struct {
		Type     string `json:"type"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := s.doJSON(ctx, http.MethodGet, requestPath, token, nil, &response); err != nil {
		return nil, err
	}
	if response.Type != "file" {
		return nil, fmt.Errorf("github contents response for %s is not a file", filePath)
	}
	if response.Encoding != "base64" {
		return nil, fmt.Errorf("unsupported github content encoding %q", response.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(response.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decode github file content: %w", err)
	}
	return decoded, nil
}

func (s *Service) ListContents(ctx context.Context, installationID int64, owner, repo, dirPath, ref string) ([]ContentEntry, error) {
	token, err := s.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}

	requestPath := "/repos/" + owner + "/" + repo + "/contents/" + strings.TrimLeft(dirPath, "/")
	if strings.TrimSpace(ref) != "" {
		requestPath += "?ref=" + url.QueryEscape(strings.TrimSpace(ref))
	}

	var entries []ContentEntry
	if err := s.doJSON(ctx, http.MethodGet, requestPath, token, nil, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *Service) GetBranchHeadSHA(ctx context.Context, installationID int64, owner, repo, branch string) (string, error) {
	token, err := s.InstallationToken(ctx, installationID)
	if err != nil {
		return "", err
	}
	var response struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := s.doJSON(ctx, http.MethodGet, "/repos/"+owner+"/"+repo+"/git/ref/heads/"+url.PathEscape(branch), token, nil, &response); err != nil {
		return "", err
	}
	if strings.TrimSpace(response.Object.SHA) == "" {
		return "", fmt.Errorf("github branch %s has no head sha", branch)
	}
	return response.Object.SHA, nil
}

func (s *Service) CreateBranch(ctx context.Context, installationID int64, owner, repo, branch, sha string) error {
	token, err := s.InstallationToken(ctx, installationID)
	if err != nil {
		return err
	}
	return s.doJSON(ctx, http.MethodPost, "/repos/"+owner+"/"+repo+"/git/refs", token, map[string]string{
		"ref": "refs/heads/" + strings.TrimSpace(branch),
		"sha": strings.TrimSpace(sha),
	}, nil)
}

func (s *Service) PutFile(ctx context.Context, installationID int64, owner, repo, filePath, branch, message string, content []byte) error {
	token, err := s.InstallationToken(ctx, installationID)
	if err != nil {
		return err
	}
	return s.doJSON(ctx, http.MethodPut, "/repos/"+owner+"/"+repo+"/contents/"+strings.TrimLeft(filePath, "/"), token, map[string]string{
		"message": strings.TrimSpace(message),
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  strings.TrimSpace(branch),
	}, nil)
}

func (s *Service) CreatePullRequest(ctx context.Context, installationID int64, owner, repo string, input PullRequestInput) (PullRequest, error) {
	token, err := s.InstallationToken(ctx, installationID)
	if err != nil {
		return PullRequest{}, err
	}
	var response PullRequest
	if err := s.doJSON(ctx, http.MethodPost, "/repos/"+owner+"/"+repo+"/pulls", token, input, &response); err != nil {
		return PullRequest{}, err
	}
	return response, nil
}

func (s *Service) doJSON(ctx context.Context, method, requestPath, bearerToken string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal github request: %w", err)
		}
		payload = strings.NewReader(string(encoded))
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.cfg.APIBaseURL, "/")+requestPath, payload)
	if err != nil {
		return fmt.Errorf("build github request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "caddytower-github-app")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearerToken != "" {
		if strings.Count(bearerToken, ".") == 2 {
			req.Header.Set("Authorization", "Bearer "+bearerToken)
		} else {
			req.Header.Set("Authorization", "token "+bearerToken)
		}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("github request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{StatusCode: resp.StatusCode, Method: method, Path: requestPath, Message: strings.TrimSpace(string(message))}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode github response: %w", err)
	}
	return nil
}

func (s *Service) appJWT() (string, error) {
	key, err := s.privateKey()
	if err != nil {
		return "", err
	}

	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	now := s.now()
	claims := map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": s.cfg.AppID,
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal github jwt header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal github jwt claims: %w", err)
	}

	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	sum := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign github jwt: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (s *Service) privateKey() (*rsa.PrivateKey, error) {
	s.keyOnce.Do(func() {
		pemBytes, err := os.ReadFile(s.cfg.PrivateKeyPath)
		if err != nil {
			s.keyErr = fmt.Errorf("read github app private key: %w", err)
			return
		}
		block, _ := pem.Decode(pemBytes)
		if block == nil {
			s.keyErr = fmt.Errorf("decode github app private key pem: no pem block found")
			return
		}
		switch block.Type {
		case "RSA PRIVATE KEY":
			s.key, s.keyErr = x509.ParsePKCS1PrivateKey(block.Bytes)
		case "PRIVATE KEY":
			parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				s.keyErr = fmt.Errorf("parse github app private key: %w", err)
				return
			}
			rsaKey, ok := parsed.(*rsa.PrivateKey)
			if !ok {
				s.keyErr = fmt.Errorf("github app private key is not RSA")
				return
			}
			s.key = rsaKey
		default:
			s.keyErr = fmt.Errorf("unsupported github app private key type %q", block.Type)
		}
	})
	if s.keyErr != nil {
		return nil, s.keyErr
	}
	return s.key, nil
}

type installationWebhookPayload struct {
	Action  string `json:"action"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	} `json:"installation"`
}
