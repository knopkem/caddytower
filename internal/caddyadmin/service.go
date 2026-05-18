package caddyadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const defaultServerName = "srv0"

type Client struct {
	baseURL    string
	httpClient *http.Client
}

type HTTPRoute struct {
	Host          string
	MatchType     string
	MatchValue    string
	StripPrefix   bool
	RewritePrefix string
	Priority      int
	Upstreams     []string
}

type Config struct {
	Apps Apps `json:"apps"`
}

type Apps struct {
	HTTP HTTPApp `json:"http"`
}

type HTTPApp struct {
	Servers map[string]Server `json:"servers"`
}

type Server struct {
	Listen []string    `json:"listen,omitempty"`
	Routes []RouteRule `json:"routes,omitempty"`
}

type RouteRule struct {
	Match    []Match   `json:"match,omitempty"`
	Handle   []Handler `json:"handle,omitempty"`
	Terminal bool      `json:"terminal,omitempty"`
}

type Match struct {
	Host       []string `json:"host,omitempty"`
	PathPrefix []string `json:"path_prefix,omitempty"`
	PathExact  []string `json:"path_exact,omitempty"`
}

type Handler struct {
	Handler         string              `json:"handler"`
	Encodings       map[string]struct{} `json:"encodings,omitempty"`
	Upstreams       []Upstream          `json:"upstreams,omitempty"`
	StripPathPrefix string              `json:"strip_path_prefix,omitempty"`
	URISubstring    []URISubstring      `json:"uri_substring,omitempty"`
}

type Upstream struct {
	Dial string `json:"dial"`
}

type URISubstring struct {
	Find    string `json:"find,omitempty"`
	Replace string `json:"replace,omitempty"`
}

func New(baseURL string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("base url must not be empty")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("base url must include scheme and host")
	}

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
	}, nil
}

func BuildConfig(routes []HTTPRoute) Config {
	sortedRoutes := append([]HTTPRoute(nil), routes...)
	sort.Slice(sortedRoutes, func(i, j int) bool { return routeLess(sortedRoutes[i], sortedRoutes[j]) })

	serverRoutes := make([]RouteRule, 0, len(sortedRoutes))
	for _, route := range sortedRoutes {
		upstreams := append([]string(nil), route.Upstreams...)
		sort.Strings(upstreams)

		handlerUpstreams := make([]Upstream, 0, len(upstreams))
		for _, upstream := range upstreams {
			handlerUpstreams = append(handlerUpstreams, Upstream{Dial: upstream})
		}

		match := Match{
			Host: []string{route.Host},
		}
		switch route.MatchType {
		case "path_prefix":
			match.PathPrefix = []string{route.MatchValue}
		case "path_exact":
			match.PathExact = []string{route.MatchValue}
		}

		handlers := []Handler{
			{
				Handler: "encode",
				Encodings: map[string]struct{}{
					"gzip": {},
					"zstd": {},
				},
			},
		}
		if route.StripPrefix || strings.TrimSpace(route.RewritePrefix) != "" {
			rewrite := Handler{Handler: "rewrite"}
			if route.StripPrefix {
				rewrite.StripPathPrefix = route.MatchValue
			}
			if strings.TrimSpace(route.RewritePrefix) != "" {
				rewrite.URISubstring = []URISubstring{{
					Find:    route.MatchValue,
					Replace: route.RewritePrefix,
				}}
			}
			handlers = append(handlers, rewrite)
		}
		handlers = append(handlers, Handler{
			Handler:   "reverse_proxy",
			Upstreams: handlerUpstreams,
		})

		serverRoutes = append(serverRoutes, RouteRule{
			Match:    []Match{match},
			Handle:   handlers,
			Terminal: true,
		})
	}

	return Config{
		Apps: Apps{
			HTTP: HTTPApp{
				Servers: map[string]Server{
					defaultServerName: {
						Listen: []string{":80", ":443"},
						Routes: serverRoutes,
					},
				},
			},
		},
	}
}

func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/config/", nil)
	if err != nil {
		return fmt.Errorf("build ping request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send ping request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ping caddy admin: unexpected status %s", resp.Status)
	}

	return nil
}

func (c *Client) GetConfig(ctx context.Context) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/config/", nil)
	if err != nil {
		return nil, fmt.Errorf("build get config request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send get config request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read get config response: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("get caddy config: unexpected status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return json.RawMessage(body), nil
}

func (c *Client) LoadConfig(ctx context.Context, cfg Config) error {
	payload, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal caddy config: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/load", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build load request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send load request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read load response: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("load caddy config: unexpected status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return nil
}

func (c *Client) Reconcile(ctx context.Context, desired Config) (bool, error) {
	current, err := c.GetConfig(ctx)
	if err != nil {
		return false, err
	}

	currentNormalized, err := normalizeJSON(current)
	if err != nil {
		return false, fmt.Errorf("normalize current config: %w", err)
	}

	desiredBytes, err := json.Marshal(desired)
	if err != nil {
		return false, fmt.Errorf("marshal desired config: %w", err)
	}

	desiredNormalized, err := normalizeJSON(desiredBytes)
	if err != nil {
		return false, fmt.Errorf("normalize desired config: %w", err)
	}

	if bytes.Equal(currentNormalized, desiredNormalized) {
		return false, nil
	}

	if err := c.LoadConfig(ctx, desired); err != nil {
		return false, err
	}

	return true, nil
}

func (c *Client) ReconcileManagedRoutes(ctx context.Context, routes []HTTPRoute, managedKeys []string) (bool, error) {
	current, err := c.GetConfig(ctx)
	if err != nil {
		return false, err
	}

	merged, err := MergeManagedRoutes(current, routes, managedKeys)
	if err != nil {
		return false, err
	}

	currentNormalized, err := normalizeJSON(current)
	if err != nil {
		return false, fmt.Errorf("normalize current config: %w", err)
	}
	desiredNormalized, err := normalizeJSON(merged)
	if err != nil {
		return false, fmt.Errorf("normalize merged config: %w", err)
	}
	if bytes.Equal(currentNormalized, desiredNormalized) {
		return false, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/load", bytes.NewReader(merged))
	if err != nil {
		return false, fmt.Errorf("build raw load request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("send raw load request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("read raw load response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("load caddy config: unexpected status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return true, nil
}

func MergeManagedRoutes(current json.RawMessage, routes []HTTPRoute, managedKeys []string) (json.RawMessage, error) {
	var root map[string]any
	if len(bytes.TrimSpace(current)) == 0 {
		root = map[string]any{}
	} else if err := json.Unmarshal(current, &root); err != nil {
		return nil, fmt.Errorf("unmarshal current config: %w", err)
	}

	apps := ensureMap(root, "apps")
	httpApp := ensureMap(apps, "http")
	servers := ensureMap(httpApp, "servers")
	server := ensureMap(servers, defaultServerName)

	if _, ok := server["listen"]; !ok {
		server["listen"] = []any{":80", ":443"}
	}

	preserved := make([]any, 0)
	if existing, ok := server["routes"].([]any); ok {
		for _, item := range existing {
			if !routeMatchesManagedKeys(item, managedKeys) {
				preserved = append(preserved, item)
			}
		}
	}

	desired := BuildConfig(routes)
	desiredRoutes := make([]any, 0, len(desired.Apps.HTTP.Servers[defaultServerName].Routes))
	for _, route := range desired.Apps.HTTP.Servers[defaultServerName].Routes {
		var routeMap map[string]any
		body, err := json.Marshal(route)
		if err != nil {
			return nil, fmt.Errorf("marshal desired route: %w", err)
		}
		if err := json.Unmarshal(body, &routeMap); err != nil {
			return nil, fmt.Errorf("unmarshal desired route: %w", err)
		}
		desiredRoutes = append(desiredRoutes, routeMap)
	}

	server["routes"] = append(preserved, desiredRoutes...)

	return json.Marshal(root)
}

func ExtractHTTPRoutes(current json.RawMessage) ([]HTTPRoute, error) {
	if len(bytes.TrimSpace(current)) == 0 {
		return nil, nil
	}

	var decoded Config
	if err := json.Unmarshal(current, &decoded); err != nil {
		return nil, fmt.Errorf("unmarshal caddy config: %w", err)
	}

	server, ok := decoded.Apps.HTTP.Servers[defaultServerName]
	if !ok {
		return nil, nil
	}

	routes := make([]HTTPRoute, 0, len(server.Routes))
	for _, route := range server.Routes {
		hosts := make([]string, 0)
		matchType := "host"
		matchValue := ""
		for _, match := range route.Match {
			hosts = append(hosts, match.Host...)
			if len(match.PathExact) > 0 && strings.TrimSpace(match.PathExact[0]) != "" {
				matchType = "path_exact"
				matchValue = strings.TrimSpace(match.PathExact[0])
			} else if len(match.PathPrefix) > 0 && strings.TrimSpace(match.PathPrefix[0]) != "" {
				matchType = "path_prefix"
				matchValue = strings.TrimSpace(match.PathPrefix[0])
			}
		}
		if len(hosts) == 0 {
			continue
		}

		upstreams := make([]string, 0)
		stripPrefix := false
		rewritePrefix := ""
		for _, handler := range route.Handle {
			switch handler.Handler {
			case "reverse_proxy":
				for _, upstream := range handler.Upstreams {
					if strings.TrimSpace(upstream.Dial) != "" {
						upstreams = append(upstreams, upstream.Dial)
					}
				}
			case "rewrite":
				if strings.TrimSpace(handler.StripPathPrefix) != "" {
					stripPrefix = true
				}
				if len(handler.URISubstring) > 0 && strings.TrimSpace(handler.URISubstring[0].Replace) != "" {
					rewritePrefix = strings.TrimSpace(handler.URISubstring[0].Replace)
				}
			}
		}
		if len(upstreams) == 0 {
			continue
		}

		sort.Strings(hosts)
		sort.Strings(upstreams)
		for _, host := range hosts {
			routes = append(routes, HTTPRoute{
				Host:          host,
				MatchType:     matchType,
				MatchValue:    matchValue,
				StripPrefix:   stripPrefix,
				RewritePrefix: rewritePrefix,
				Upstreams:     append([]string(nil), upstreams...),
			})
		}
	}

	sort.Slice(routes, func(i, j int) bool { return routeLess(routes[i], routes[j]) })
	return routes, nil
}

func normalizeJSON(raw []byte) ([]byte, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}

	return json.Marshal(decoded)
}

func ensureMap(root map[string]any, key string) map[string]any {
	if value, ok := root[key].(map[string]any); ok {
		return value
	}
	next := map[string]any{}
	root[key] = next
	return next
}

func routeMatchesManagedKeys(route any, managedKeys []string) bool {
	routeMap, ok := route.(map[string]any)
	if !ok {
		return false
	}

	matchers, ok := routeMap["match"].([]any)
	if !ok {
		return false
	}

	managedSet := map[string]struct{}{}
	for _, key := range managedKeys {
		managedSet[key] = struct{}{}
	}

	for _, matcher := range matchers {
		matcherMap, ok := matcher.(map[string]any)
		if !ok {
			continue
		}
		hosts, ok := matcherMap["host"].([]any)
		if !ok {
			continue
		}
		matchType := "host"
		matchValue := ""
		if values, ok := matcherMap["path_exact"].([]any); ok && len(values) > 0 {
			if value, ok := values[0].(string); ok && strings.TrimSpace(value) != "" {
				matchType = "path_exact"
				matchValue = strings.TrimSpace(value)
			}
		} else if values, ok := matcherMap["path_prefix"].([]any); ok && len(values) > 0 {
			if value, ok := values[0].(string); ok && strings.TrimSpace(value) != "" {
				matchType = "path_prefix"
				matchValue = strings.TrimSpace(value)
			}
		}
		for _, host := range hosts {
			hostValue, ok := host.(string)
			if ok {
				if _, exists := managedSet[RouteKey(HTTPRoute{
					Host:       hostValue,
					MatchType:  matchType,
					MatchValue: matchValue,
				})]; exists {
					return true
				}
			}
		}
	}

	return false
}

func RouteKey(route HTTPRoute) string {
	return strings.Join([]string{
		strings.TrimSpace(strings.ToLower(route.Host)),
		normalizedMatchType(route.MatchType),
		strings.TrimSpace(route.MatchValue),
	}, "|")
}

func MatcherSummary(route HTTPRoute) string {
	switch normalizedMatchType(route.MatchType) {
	case "path_prefix":
		return "prefix " + strings.TrimSpace(route.MatchValue)
	case "path_exact":
		return "exact " + strings.TrimSpace(route.MatchValue)
	default:
		return "catch-all host"
	}
}

func normalizedMatchType(value string) string {
	switch strings.TrimSpace(value) {
	case "path_prefix", "path_exact":
		return strings.TrimSpace(value)
	default:
		return "host"
	}
}

func routeLess(a, b HTTPRoute) bool {
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	if routeSpecificity(a) != routeSpecificity(b) {
		return routeSpecificity(a) > routeSpecificity(b)
	}
	if len(strings.TrimSpace(a.MatchValue)) != len(strings.TrimSpace(b.MatchValue)) {
		return len(strings.TrimSpace(a.MatchValue)) > len(strings.TrimSpace(b.MatchValue))
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return MatcherSummary(a) < MatcherSummary(b)
}

func routeSpecificity(route HTTPRoute) int {
	switch normalizedMatchType(route.MatchType) {
	case "path_exact":
		return 2
	case "path_prefix":
		return 1
	default:
		return 0
	}
}
