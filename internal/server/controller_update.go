package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"caddytower/internal/ui"
)

const controllerReleaseCheckTimeout = 3 * time.Second

type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func (s *Server) controllerUpdateStatus(ctx context.Context) ui.ControllerUpdateData {
	data := ui.ControllerUpdateData{
		CurrentVersion: strings.TrimSpace(s.version.Version),
	}
	if s.projects == nil {
		data.StatusMessage = "Controller updates are unavailable in this mode."
		return data
	}

	controller, err := s.projects.ControllerContainer(ctx)
	if err != nil {
		data.StatusMessage = "Controller update status is unavailable right now."
		return data
	}
	data.Checked = true
	data.CurrentImage = strings.TrimSpace(controller.Image)

	owner, repo, currentTag, ok := parseGHCRImageRef(data.CurrentImage)
	if !ok {
		data.StatusMessage = "Controller updates need a GHCR image like ghcr.io/<owner>/caddytower:<tag>."
		return data
	}

	releaseCtx, cancel := context.WithTimeout(ctx, controllerReleaseCheckTimeout)
	defer cancel()

	latest, err := fetchLatestRelease(releaseCtx, owner, repo)
	if err != nil {
		data.StatusMessage = "Could not check the latest GitHub release right now."
		if isRefreshableChannelTag(currentTag) {
			data.CanTrigger = true
			data.TrackingLatest = true
			data.CurrentChannel = currentTag
			data.TargetImage = imageRefForTag(owner, repo, currentTag)
			data.ButtonLabel = "Pull current channel"
			data.StatusMessage = "GitHub release lookup failed, but you can still pull the current channel on demand."
		}
		return data
	}

	data.LatestRelease = latest.TagName
	data.LatestReleaseURL = latest.HTMLURL

	currentRelease := normalizeReleaseVersion(data.CurrentVersion)
	if currentRelease == "" {
		currentRelease = normalizeReleaseVersion(currentTag)
	}

	switch {
	case currentRelease != "" && normalizeReleaseVersion(latest.TagName) != "":
		switch compareReleaseVersions(currentRelease, latest.TagName) {
		case -1:
			data.UpdateAvailable = true
			data.CanTrigger = true
			data.TargetImage = imageRefForTag(owner, repo, latest.TagName)
			data.ButtonLabel = "Update and restart"
			data.StatusMessage = "A newer release is available."
		case 0:
			data.StatusMessage = "You are already on the latest release."
		default:
			data.StatusMessage = "This controller is ahead of the latest tagged release."
		}
	case isRefreshableChannelTag(currentTag):
		data.CanTrigger = true
		data.TrackingLatest = true
		data.CurrentChannel = currentTag
		data.TargetImage = imageRefForTag(owner, repo, currentTag)
		data.ButtonLabel = "Pull current channel"
		data.StatusMessage = "This install tracks a moving channel, so release-to-release update checks are not exact."
	default:
		data.CurrentChannelIsDev = true
		data.StatusMessage = "This controller is not pinned to a release tag, so only release information is shown."
	}

	return data
}

func fetchLatestRelease(ctx context.Context, owner, repo string) (githubRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+owner+"/"+repo+"/releases/latest", nil)
	if err != nil {
		return githubRelease{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return githubRelease{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("github releases returned %d", response.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return githubRelease{}, err
	}
	if strings.TrimSpace(release.TagName) == "" {
		return githubRelease{}, fmt.Errorf("github release tag is empty")
	}
	return release, nil
}

func parseGHCRImageRef(value string) (string, string, string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", "", false
	}
	path := value
	if index := strings.Index(path, "@"); index >= 0 {
		path = path[:index]
	}
	tag := ""
	if index := strings.LastIndex(path, ":"); index >= 0 {
		tag = path[index+1:]
		path = path[:index]
	}
	parts := strings.Split(path, "/")
	if len(parts) != 3 || !strings.EqualFold(parts[0], "ghcr.io") {
		return "", "", "", false
	}
	return strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2]), strings.TrimSpace(tag), true
}

func imageRefForTag(owner, repo, tag string) string {
	return "ghcr.io/" + owner + "/" + repo + ":" + strings.TrimSpace(tag)
}

func isRefreshableChannelTag(tag string) bool {
	tag = strings.TrimSpace(strings.ToLower(tag))
	switch tag {
	case "latest", "main":
		return true
	default:
		return false
	}
}

func normalizeReleaseVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	if lower == "dev" || lower == "main" || lower == "latest" || lower == "unknown" {
		return ""
	}
	if index := strings.IndexAny(value, "+-"); index >= 0 {
		value = value[:index]
	}
	value = strings.TrimPrefix(value, "v")
	if value == "" {
		return ""
	}
	parts := strings.Split(value, ".")
	for _, part := range parts {
		if part == "" {
			return ""
		}
		if _, err := strconv.Atoi(part); err != nil {
			return ""
		}
	}
	return "v" + strings.Join(parts, ".")
}

func compareReleaseVersions(current, latest string) int {
	current = normalizeReleaseVersion(current)
	latest = normalizeReleaseVersion(latest)
	if current == "" || latest == "" {
		return 0
	}
	currentParts := strings.Split(strings.TrimPrefix(current, "v"), ".")
	latestParts := strings.Split(strings.TrimPrefix(latest, "v"), ".")
	maxParts := len(currentParts)
	if len(latestParts) > maxParts {
		maxParts = len(latestParts)
	}
	for i := 0; i < maxParts; i++ {
		currentValue := releasePartAt(currentParts, i)
		latestValue := releasePartAt(latestParts, i)
		switch {
		case currentValue < latestValue:
			return -1
		case currentValue > latestValue:
			return 1
		}
	}
	return 0
}

func releasePartAt(parts []string, index int) int {
	if index >= len(parts) {
		return 0
	}
	value, err := strconv.Atoi(parts[index])
	if err != nil {
		return 0
	}
	return value
}
