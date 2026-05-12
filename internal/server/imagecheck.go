package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/distribution/reference"
)

type imageCheckResult struct {
	OK        bool   `json:"ok"`
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	Canonical string `json:"canonical,omitempty"`
	Registry  string `json:"registry,omitempty"`
}

type imageRefChecker struct {
	client             *http.Client
	baseURLForRegistry func(string) string
}

type bearerChallenge struct {
	Realm   string
	Service string
	Scope   string
}

func newImageRefChecker() *imageRefChecker {
	return &imageRefChecker{
		client: &http.Client{Timeout: 8 * time.Second},
		baseURLForRegistry: func(registry string) string {
			if registry == "docker.io" {
				return "https://registry-1.docker.io"
			}
			return "https://" + registry
		},
	}
}

func (s *Server) handleImageCheck(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.imageChecker == nil {
		http.NotFound(w, r)
		return
	}
	if _, ok, err := s.currentUser(w, r); err != nil {
		s.logger.Error("authenticate image check", "error", err)
		http.Error(w, "failed to authenticate request", http.StatusInternalServerError)
		return
	} else if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	result := s.imageChecker.Check(r.Context(), r.URL.Query().Get("image_ref"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if result.Kind == "invalid" {
		w.WriteHeader(http.StatusBadRequest)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(result)
}

func (c *imageRefChecker) Check(ctx context.Context, imageRef string) imageCheckResult {
	ref := strings.TrimSpace(imageRef)
	if ref == "" {
		return imageCheckResult{Kind: "invalid", Message: "Enter an image reference first."}
	}

	named, manifestRef, canonical, err := parseImageReference(ref)
	if err != nil {
		return imageCheckResult{Kind: "invalid", Message: "Use a valid image reference such as ghcr.io/owner/app:latest."}
	}

	registry := reference.Domain(named)
	repository := reference.Path(named)
	manifestURL := strings.TrimRight(c.baseURLForRegistry(registry), "/") + "/v2/" + repository + "/manifests/" + url.PathEscape(manifestRef)

	resp, body, err := c.requestManifest(ctx, http.MethodHead, manifestURL, "")
	if err != nil {
		return imageCheckResult{
			Kind:      "unreachable",
			Message:   "Unable to verify the registry right now. You can still save and deploy if the VPS can already pull this image.",
			Canonical: canonical,
			Registry:  registry,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := parseBearerChallenge(resp.Header.Get("Www-Authenticate"))
		if challenge != nil {
			token, tokenErr := c.fetchBearerToken(ctx, challenge)
			if tokenErr == nil && token != "" {
				_ = resp.Body.Close()
				resp, body, err = c.requestManifest(ctx, http.MethodHead, manifestURL, token)
				if err == nil {
					defer resp.Body.Close()
				}
			}
		}
	}

	if err != nil {
		return imageCheckResult{
			Kind:      "unreachable",
			Message:   "Unable to verify the registry right now. You can still save and deploy if the VPS can already pull this image.",
			Canonical: canonical,
			Registry:  registry,
		}
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return imageCheckResult{
			OK:        true,
			Kind:      "ok",
			Message:   "Image is reachable and appears pullable.",
			Canonical: canonical,
			Registry:  registry,
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		return imageCheckResult{
			Kind:      "denied",
			Message:   "Registry denied anonymous pull access. Make the image public or publish it before deploying.",
			Canonical: canonical,
			Registry:  registry,
		}
	case http.StatusNotFound:
		return imageCheckResult{
			Kind:      "not-found",
			Message:   "Image tag or digest was not found in the registry.",
			Canonical: canonical,
			Registry:  registry,
		}
	default:
		message := "Registry check returned an unexpected response."
		if body != "" {
			message = fmt.Sprintf("%s (%s)", message, body)
		}
		return imageCheckResult{
			Kind:      "unknown",
			Message:   message,
			Canonical: canonical,
			Registry:  registry,
		}
	}
}

func parseImageReference(raw string) (reference.Named, string, string, error) {
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(raw))
	if err != nil {
		return nil, "", "", err
	}
	if canonical, ok := named.(reference.Canonical); ok {
		return named, canonical.Digest().String(), reference.FamiliarString(named), nil
	}
	tagged := reference.TagNameOnly(named)
	taggedNamed, ok := tagged.(reference.NamedTagged)
	if !ok {
		return nil, "", "", fmt.Errorf("image reference has no tag")
	}
	return tagged, taggedNamed.Tag(), reference.FamiliarString(tagged), nil
}

func (c *imageRefChecker) requestManifest(ctx context.Context, method, manifestURL, token string) (*http.Response, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, manifestURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	req.Header.Set("User-Agent", "caddytower-image-check/1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode == http.StatusMethodNotAllowed && method == http.MethodHead {
		_ = resp.Body.Close()
		return c.requestManifest(ctx, http.MethodGet, manifestURL, token)
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body = io.NopCloser(strings.NewReader(string(body)))
	return resp, strings.TrimSpace(string(body)), nil
}

func parseBearerChallenge(header string) *bearerChallenge {
	header = strings.TrimSpace(header)
	if header == "" || !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return nil
	}
	raw := strings.TrimSpace(header[len("Bearer "):])
	challenge := &bearerChallenge{}
	for _, part := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"`)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "realm":
			challenge.Realm = value
		case "service":
			challenge.Service = value
		case "scope":
			challenge.Scope = value
		}
	}
	if challenge.Realm == "" {
		return nil
	}
	return challenge
}

func (c *imageRefChecker) fetchBearerToken(ctx context.Context, challenge *bearerChallenge) (string, error) {
	values := url.Values{}
	if challenge.Service != "" {
		values.Set("service", challenge.Service)
	}
	if challenge.Scope != "" {
		values.Set("scope", challenge.Scope)
	}

	tokenURL := challenge.Realm
	if encoded := values.Encode(); encoded != "" {
		separator := "?"
		if strings.Contains(tokenURL, "?") {
			separator = "&"
		}
		tokenURL += separator + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "caddytower-image-check/1")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected token status %d", resp.StatusCode)
	}

	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&payload); err != nil {
		return "", err
	}
	if payload.Token != "" {
		return payload.Token, nil
	}
	return payload.AccessToken, nil
}
