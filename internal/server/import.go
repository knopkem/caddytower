package server

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	neturl "net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	githubapp "caddytower/internal/github"
	"caddytower/internal/projects"
	"caddytower/internal/ui"
)

const (
	importWorkflowSecretName = "CADDYTOWER_DEPLOY_WEBHOOK_SECRET"
	importWorkflowPath       = ".github/workflows/caddytower-deploy.yml"
)

type importSelection struct {
	InstallationID int64
	Query          string
	RepoFullName   string
}

type importDetection struct {
	Repository        githubapp.Repository
	InstallationID    int64
	DockerfileFound   bool
	DockerCompose     bool
	WorkflowPaths     []string
	FrameworkHint     string
	SuggestedImageRef string
	SuggestedPort     int
	ImageReachable    bool
	ImageCheckMessage string
}

func (s *Server) handleImportPage(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	selection := importSelection{
		Query:        strings.TrimSpace(r.URL.Query().Get("q")),
		RepoFullName: strings.TrimSpace(r.URL.Query().Get("repo")),
	}
	if value := strings.TrimSpace(r.URL.Query().Get("installation")); value != "" {
		if installationID, err := strconv.ParseInt(value, 10, 64); err == nil {
			selection.InstallationID = installationID
		}
	}
	s.renderImportPage(w, r, user.Email, selection, ui.ProjectFormData{}, importDetection{}, "", r.URL.Query().Get("info"))
}

func (s *Server) handleImportCreate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAuthenticated(w, r)
	if !ok {
		return
	}
	if !s.auth.ValidateCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	installationID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("installation_id")), 10, 64)
	if err != nil || installationID <= 0 {
		s.renderImportPage(w, r, user.Email, importSelection{RepoFullName: strings.TrimSpace(r.FormValue("repo_full_name"))}, ui.ProjectFormData{}, importDetection{}, "choose a GitHub installation first", "")
		return
	}

	selection := importSelection{
		InstallationID: installationID,
		RepoFullName:   strings.TrimSpace(r.FormValue("repo_full_name")),
	}
	detection, err := s.detectImport(r.Context(), selection.InstallationID, selection.RepoFullName)
	if err != nil {
		s.renderImportPage(w, r, user.Email, selection, ui.ProjectFormData{}, importDetection{}, err.Error(), "")
		return
	}
	if detection.DockerCompose {
		s.renderImportPage(w, r, user.Email, selection, ui.ProjectFormData{}, detection, "docker-compose projects need a manual image flow today", "")
		return
	}

	input, form, err := projectInputFromRequest(r, "", "web")
	if err != nil {
		s.renderImportPage(w, r, user.Email, selection, form, detection, err.Error(), "")
		return
	}
	input.Type = "web"
	input.GitHubRepoFullName = detection.Repository.FullName
	input.GitHubInstallationID = selection.InstallationID
	input.GitHubDefaultBranch = detection.Repository.DefaultBranch
	input.SkipDeploy = !detection.workflowDetected() || !detection.ImageReachable

	project, err := s.projects.CreateWebProject(r.Context(), input, user.ID)
	if err != nil {
		s.renderImportPage(w, r, user.Email, selection, form, detection, err.Error(), "")
		return
	}

	info := "Imported " + detection.Repository.FullName + "."
	if detection.workflowDetected() {
		if detection.ImageReachable {
			info = "Imported " + detection.Repository.FullName + " and deployed the current GHCR image."
		} else {
			info = "Imported " + detection.Repository.FullName + " in pending-image mode. Add the GitHub Actions webhook step shown on the project page, then push once."
		}
		http.Redirect(w, r, "/projects/"+project.ID+"?info="+neturl.QueryEscape(info), http.StatusFound)
		return
	}

	pr, err := s.createImportWorkflowPR(r.Context(), project)
	if err != nil {
		info = "Imported " + detection.Repository.FullName + " in pending-image mode. Automatic workflow PR failed, so add the GitHub Actions webhook step from the project page manually."
		http.Redirect(w, r, "/projects/"+project.ID+"?info="+neturl.QueryEscape(info), http.StatusFound)
		return
	}
	info = "Imported " + detection.Repository.FullName + ". Open " + pr.HTMLURL + " to merge the generated workflow, then add repo secret " + importWorkflowSecretName + " from the project page."
	http.Redirect(w, r, "/projects/"+project.ID+"?info="+neturl.QueryEscape(info), http.StatusFound)
}

func (s *Server) renderImportPage(w http.ResponseWriter, r *http.Request, currentUser string, selection importSelection, form ui.ProjectFormData, detection importDetection, errorMessage, infoMessage string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	errorTitle, errorHints := describeUIError(errorMessage)

	data := ui.ImportPageData{
		PageTitle:            "CaddyTower | Import from GitHub",
		Headline:             "Import a GitHub repository",
		CSRFToken:            s.auth.EnsureCSRFCookie(w, r),
		CurrentUser:          currentUser,
		ErrorMessage:         errorMessage,
		ErrorTitle:           errorTitle,
		ErrorHints:           errorHints,
		InfoMessage:          infoMessage,
		GitHub:               s.gitHubStatusData(r.Context()),
		SelectedInstallation: selection.InstallationID,
		Query:                selection.Query,
		SelectedRepoFullName: selection.RepoFullName,
	}

	if !data.GitHub.Configured || !data.GitHub.Connected {
		if err := s.ui.Render(w, "import.gohtml", data); err != nil {
			s.logger.Error("render import page", "error", err)
			http.Error(w, "failed to render page", http.StatusInternalServerError)
		}
		return
	}

	if data.SelectedInstallation == 0 && len(data.GitHub.Installations) > 0 {
		data.SelectedInstallation = data.GitHub.Installations[0].InstallationID
	}

	repos, repoErr := s.loadImportRepositories(r.Context(), data.SelectedInstallation, data.Query, selection.RepoFullName)
	if repoErr != nil && data.ErrorMessage == "" {
		data.ErrorMessage = repoErr.Error()
		data.ErrorTitle, data.ErrorHints = describeUIError(data.ErrorMessage)
	}
	data.Repositories = repos

	if detection.Repository.FullName == "" && selection.RepoFullName != "" && data.SelectedInstallation > 0 {
		var detectErr error
		detection, detectErr = s.detectImport(r.Context(), data.SelectedInstallation, selection.RepoFullName)
		if detectErr != nil && data.ErrorMessage == "" {
			data.ErrorMessage = detectErr.Error()
			data.ErrorTitle, data.ErrorHints = describeUIError(data.ErrorMessage)
		}
	}
	if detection.Repository.FullName != "" {
		data.Detection = ui.ImportDetectionData{
			Ready:                true,
			RepoName:             detection.Repository.Name,
			RepoFullName:         detection.Repository.FullName,
			RepoURL:              detection.Repository.HTMLURL,
			DefaultBranch:        detection.Repository.DefaultBranch,
			InstallationID:       detection.InstallationID,
			DockerfileFound:      detection.DockerfileFound,
			DockerComposeFound:   detection.DockerCompose,
			WorkflowDetected:     detection.workflowDetected(),
			WorkflowPaths:        append([]string(nil), detection.WorkflowPaths...),
			FrameworkHint:        detection.FrameworkHint,
			SuggestedImageRef:    detection.SuggestedImageRef,
			ImageReachable:       detection.ImageReachable,
			ImageCheckMessage:    detection.ImageCheckMessage,
			WillOpenWorkflowPR:   !detection.workflowDetected(),
			SecretName:           importWorkflowSecretName,
			WebhookSnippet:       buildGitHubWebhookSnippet(strings.TrimRight(s.runtimePublicBaseURL(r.Context()), "/")+"/api/webhooks/deploy/"+normalizeImportSlug(detection.Repository.Name), importWorkflowSecretName),
			WorkflowPRFilePath:   importWorkflowPath,
			WorkflowPRBranchName: importWorkflowBranchName(detection.Repository.Name),
		}
		if detection.DockerCompose {
			data.Detection.UnsupportedReason = "docker-compose.yml was found in this repo. CaddyTower imports image-based projects today, so convert this repo to a single-image GHCR flow first."
		}
		if form.Action == "" {
			form = projectFormDataFromInput(projects.WebProjectInput{
				Type:              "web",
				Name:              detection.Repository.Name,
				Slug:              normalizeImportSlug(detection.Repository.Name),
				ImageRef:          detection.SuggestedImageRef,
				Subdomain:         normalizeImportSlug(detection.Repository.Name),
				InternalPort:      detection.defaultPort(),
				WatchtowerEnabled: true,
			}, "")
			if !detection.workflowDetected() || !detection.ImageReachable {
				form.SubmitLabel = "Create project"
			} else {
				form.SubmitLabel = "Create and deploy"
			}
		}
	}
	data.Project = form

	if err := s.ui.Render(w, "import.gohtml", data); err != nil {
		s.logger.Error("render import page", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

func (s *Server) loadImportRepositories(ctx context.Context, installationID int64, query, selectedFullName string) ([]ui.GitHubRepositoryItem, error) {
	if installationID <= 0 {
		return nil, nil
	}
	githubService := s.runtimeGitHubService(ctx)
	if githubService == nil || !githubService.Configured() {
		return nil, fmt.Errorf("GitHub App is not configured")
	}
	repositories, err := githubService.ListRepositories(ctx, installationID)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	filter := strings.ToLower(strings.TrimSpace(query))
	items := make([]ui.GitHubRepositoryItem, 0, len(repositories))
	for _, repository := range repositories {
		if filter != "" && !strings.Contains(strings.ToLower(repository.FullName), filter) && !strings.Contains(strings.ToLower(repository.Name), filter) {
			continue
		}
		items = append(items, ui.GitHubRepositoryItem{
			InstallationID: installationID,
			Name:           repository.Name,
			FullName:       repository.FullName,
			DefaultBranch:  repository.DefaultBranch,
			HTMLURL:        repository.HTMLURL,
			Selected:       repository.FullName == selectedFullName,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].FullName < items[j].FullName
	})
	return items, nil
}

func (s *Server) detectImport(ctx context.Context, installationID int64, repoFullName string) (importDetection, error) {
	githubService := s.runtimeGitHubService(ctx)
	if githubService == nil || !githubService.Configured() {
		return importDetection{}, fmt.Errorf("GitHub App is not configured")
	}
	owner, repo, err := splitRepoFullName(repoFullName)
	if err != nil {
		return importDetection{}, err
	}
	repository, err := githubService.GetRepository(ctx, installationID, owner, repo)
	if err != nil {
		return importDetection{}, fmt.Errorf("load repository %s: %w", repoFullName, err)
	}
	detection := importDetection{
		Repository:        repository,
		InstallationID:    installationID,
		SuggestedImageRef: suggestedImageRef(repository.FullName),
	}

	if dockerfile, ok, err := s.optionalRepoFile(ctx, githubService, installationID, owner, repo, "Dockerfile", repository.DefaultBranch); err != nil {
		return importDetection{}, err
	} else if ok {
		detection.DockerfileFound = true
		if port := firstExposedPort(dockerfile); port > 0 {
			detection.SuggestedPort = port
		}
	}

	for _, filePath := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, ok, err := s.optionalRepoFile(ctx, githubService, installationID, owner, repo, filePath, repository.DefaultBranch); err != nil {
			return importDetection{}, err
		} else if ok {
			detection.DockerCompose = true
			break
		}
	}

	detection.FrameworkHint = s.detectFrameworkHint(ctx, githubService, installationID, owner, repo, repository.DefaultBranch)
	detection.WorkflowPaths = s.detectWorkflowPaths(ctx, githubService, installationID, owner, repo, repository.DefaultBranch)

	if !detection.workflowDetected() && !detection.DockerfileFound {
		return importDetection{}, fmt.Errorf("could not find a root Dockerfile or an existing GHCR workflow for %s", repository.FullName)
	}

	imageStatus := s.imageChecker.Check(ctx, detection.SuggestedImageRef)
	detection.ImageReachable = imageStatus.OK
	detection.ImageCheckMessage = imageStatus.Message
	return detection, nil
}

func (s *Server) optionalRepoFile(ctx context.Context, githubService *githubapp.Service, installationID int64, owner, repo, filePath, ref string) ([]byte, bool, error) {
	content, err := githubService.GetFileContent(ctx, installationID, owner, repo, filePath, ref)
	if err != nil {
		if githubapp.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("load %s: %w", filePath, err)
	}
	return content, true, nil
}

func (s *Server) detectFrameworkHint(ctx context.Context, githubService *githubapp.Service, installationID int64, owner, repo, ref string) string {
	files := []struct {
		Path string
		Name string
	}{
		{Path: "package.json", Name: "Node.js app"},
		{Path: "go.mod", Name: "Go app"},
		{Path: "pyproject.toml", Name: "Python app"},
		{Path: "requirements.txt", Name: "Python app"},
		{Path: "Cargo.toml", Name: "Rust app"},
	}
	for _, file := range files {
		if _, ok, err := s.optionalRepoFile(ctx, githubService, installationID, owner, repo, file.Path, ref); err == nil && ok {
			return file.Name
		}
	}
	return ""
}

func (s *Server) detectWorkflowPaths(ctx context.Context, githubService *githubapp.Service, installationID int64, owner, repo, ref string) []string {
	entries, err := githubService.ListContents(ctx, installationID, owner, repo, ".github/workflows", ref)
	if err != nil {
		if githubapp.IsNotFound(err) {
			return nil
		}
		s.logger.Warn("list github workflows", "repository", owner+"/"+repo, "error", err)
		return nil
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != "file" {
			continue
		}
		lowerPath := strings.ToLower(entry.Path)
		if !strings.HasSuffix(lowerPath, ".yml") && !strings.HasSuffix(lowerPath, ".yaml") {
			continue
		}
		content, err := githubService.GetFileContent(ctx, installationID, owner, repo, entry.Path, ref)
		if err != nil {
			s.logger.Warn("read github workflow", "path", entry.Path, "error", err)
			continue
		}
		if looksLikeImageWorkflow(string(content)) {
			paths = append(paths, entry.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

func (s *Server) createImportWorkflowPR(ctx context.Context, project projects.Project) (githubapp.PullRequest, error) {
	githubService := s.runtimeGitHubService(ctx)
	if githubService == nil || !githubService.Configured() {
		return githubapp.PullRequest{}, fmt.Errorf("GitHub App is not configured")
	}
	owner, repo, err := splitRepoFullName(project.GitHubRepoFullName)
	if err != nil {
		return githubapp.PullRequest{}, err
	}
	baseSHA, err := githubService.GetBranchHeadSHA(ctx, project.GitHubInstallationID, owner, repo, project.GitHubDefaultBranch)
	if err != nil {
		return githubapp.PullRequest{}, fmt.Errorf("load default branch head: %w", err)
	}
	branchName := importWorkflowBranchName(project.Slug)
	if err := githubService.CreateBranch(ctx, project.GitHubInstallationID, owner, repo, branchName, baseSHA); err != nil && !githubapp.IsConflict(err) {
		return githubapp.PullRequest{}, fmt.Errorf("create workflow branch: %w", err)
	}
	webhookURL := strings.TrimRight(s.runtimePublicBaseURL(ctx), "/") + "/api/webhooks/deploy/" + project.Slug
	if err := githubService.PutFile(ctx, project.GitHubInstallationID, owner, repo, importWorkflowPath, branchName, "Add CaddyTower deploy workflow", []byte(buildGitHubDeployWorkflow(webhookURL, project.GitHubDefaultBranch, project.ImageRef))); err != nil {
		return githubapp.PullRequest{}, fmt.Errorf("write workflow file: %w", err)
	}
	body := "This workflow builds a GHCR image and notifies CaddyTower after pushes to `" + project.GitHubDefaultBranch + "`.\n\n" +
		"After merging, add repository secret `" + importWorkflowSecretName + "` with the webhook secret shown in the CaddyTower project page."
	return githubService.CreatePullRequest(ctx, project.GitHubInstallationID, owner, repo, githubapp.PullRequestInput{
		Title: "Add CaddyTower deploy workflow",
		Head:  branchName,
		Base:  project.GitHubDefaultBranch,
		Body:  body,
	})
}

func (d importDetection) workflowDetected() bool {
	return len(d.WorkflowPaths) > 0
}

func (d importDetection) defaultPort() int {
	if d.SuggestedPort > 0 {
		return d.SuggestedPort
	}
	return 3000
}

func splitRepoFullName(value string) (string, string, error) {
	owner, repo, ok := strings.Cut(strings.TrimSpace(value), "/")
	if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" {
		return "", "", fmt.Errorf("invalid repository %q", value)
	}
	return strings.TrimSpace(owner), strings.TrimSpace(repo), nil
}

func suggestedImageRef(repoFullName string) string {
	return strings.ToLower("ghcr.io/" + strings.TrimSpace(repoFullName) + ":latest")
}

func normalizeImportSlug(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("_", "-", ".", "-", "/", "-", " ", "-")
	normalized = replacer.Replace(normalized)
	var builder strings.Builder
	lastDash := false
	for _, r := range normalized {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastDash = false
		case !lastDash:
			builder.WriteRune('-')
			lastDash = true
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "project"
	}
	return result
}

func firstExposedPort(dockerfile []byte) int {
	scanner := bufio.NewScanner(strings.NewReader(string(dockerfile)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(strings.ToUpper(line), "EXPOSE ") {
			continue
		}
		parts := strings.Fields(line)
		for _, part := range parts[1:] {
			candidate := strings.TrimSpace(strings.SplitN(part, "/", 2)[0])
			port, err := strconv.Atoi(candidate)
			if err == nil && port > 0 && port <= 65535 {
				return port
			}
		}
	}
	return 0
}

func looksLikeImageWorkflow(content string) bool {
	lower := strings.ToLower(content)
	return strings.Contains(lower, "docker/build-push-action") ||
		strings.Contains(lower, "ghcr.io") ||
		strings.Contains(lower, "docker/login-action")
}

func importWorkflowBranchName(slug string) string {
	return fmt.Sprintf("caddytower/%s-%d", normalizeImportSlug(slug), time.Now().UTC().Unix())
}

func buildGitHubWebhookSnippet(webhookURL, secretName string) string {
	return strings.TrimSpace(fmt.Sprintf(`
- name: Notify CaddyTower
  env:
    CADDYTOWER_DEPLOY_WEBHOOK_SECRET: ${{ secrets.%s }}
    CADDYTOWER_DEPLOY_WEBHOOK_URL: %s
  run: |
    payload='{"ref":"'"${GITHUB_REF}"'","sha":"'"${GITHUB_SHA}"'","repository":"'"${GITHUB_REPOSITORY}"'"}'
    printf '%%s' "$payload" > payload.json
    signature="sha256=$(openssl dgst -sha256 -hmac "$CADDYTOWER_DEPLOY_WEBHOOK_SECRET" payload.json | awk '{print $2}')"
    curl --fail --show-error --silent \
      --header "Content-Type: application/json" \
      --header "X-Signature: $signature" \
      --data @payload.json \
      "$CADDYTOWER_DEPLOY_WEBHOOK_URL"
`, secretName, webhookURL))
}

func buildGitHubDeployWorkflow(webhookURL, defaultBranch, imageRef string) string {
	return strings.TrimSpace(fmt.Sprintf(`
name: Build and deploy with CaddyTower

on:
  push:
    branches:
      - %s
  workflow_dispatch:

permissions:
  contents: read
  packages: write

jobs:
  build-and-deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Resolve image reference
        id: image
        run: |
          echo "ref=%s" >> "$GITHUB_OUTPUT"
      - uses: docker/build-push-action@v6
        with:
          context: .
          file: ./Dockerfile
          push: true
          tags: ${{ steps.image.outputs.ref }}
%s
`, defaultBranch, imageRef, indentBlock(buildGitHubWebhookSnippet(webhookURL, importWorkflowSecretName), "      ")))
}

func indentBlock(value, prefix string) string {
	lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
	for idx, line := range lines {
		lines[idx] = prefix + line
	}
	return strings.Join(lines, "\n")
}
