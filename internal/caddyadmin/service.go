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
	Host      string
	Upstreams []string
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
	Host []string `json:"host,omitempty"`
}

type Handler struct {
	Handler   string              `json:"handler"`
	Encodings map[string]struct{} `json:"encodings,omitempty"`
	Upstreams []Upstream          `json:"upstreams,omitempty"`
}

type Upstream struct {
	Dial string `json:"dial"`
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
	sort.Slice(sortedRoutes, func(i, j int) bool {
		return sortedRoutes[i].Host < sortedRoutes[j].Host
	})

	serverRoutes := make([]RouteRule, 0, len(sortedRoutes))
	for _, route := range sortedRoutes {
		upstreams := append([]string(nil), route.Upstreams...)
		sort.Strings(upstreams)

		handlerUpstreams := make([]Upstream, 0, len(upstreams))
		for _, upstream := range upstreams {
			handlerUpstreams = append(handlerUpstreams, Upstream{Dial: upstream})
		}

		serverRoutes = append(serverRoutes, RouteRule{
			Match: []Match{{
				Host: []string{route.Host},
			}},
			Handle: []Handler{
				{
					Handler: "encode",
					Encodings: map[string]struct{}{
						"gzip": {},
						"zstd": {},
					},
				},
				{
					Handler:   "reverse_proxy",
					Upstreams: handlerUpstreams,
				},
			},
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

func (c *Client) ReconcileManagedRoutes(ctx context.Context, routes []HTTPRoute, managedHosts []string) (bool, error) {
	current, err := c.GetConfig(ctx)
	if err != nil {
		return false, err
	}

	merged, err := MergeManagedRoutes(current, routes, managedHosts)
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

func MergeManagedRoutes(current json.RawMessage, routes []HTTPRoute, managedHosts []string) (json.RawMessage, error) {
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
			if !routeMatchesManagedHosts(item, managedHosts) {
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
		for _, match := range route.Match {
			hosts = append(hosts, match.Host...)
		}
		if len(hosts) == 0 {
			continue
		}

		upstreams := make([]string, 0)
		for _, handler := range route.Handle {
			if handler.Handler != "reverse_proxy" {
				continue
			}
			for _, upstream := range handler.Upstreams {
				if strings.TrimSpace(upstream.Dial) != "" {
					upstreams = append(upstreams, upstream.Dial)
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
				Host:      host,
				Upstreams: append([]string(nil), upstreams...),
			})
		}
	}

	sort.Slice(routes, func(i, j int) bool {
		return routes[i].Host < routes[j].Host
	})
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

func routeMatchesManagedHosts(route any, managedHosts []string) bool {
	routeMap, ok := route.(map[string]any)
	if !ok {
		return false
	}

	matchers, ok := routeMap["match"].([]any)
	if !ok {
		return false
	}

	managedSet := map[string]struct{}{}
	for _, host := range managedHosts {
		managedSet[host] = struct{}{}
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
		for _, host := range hosts {
			hostValue, ok := host.(string)
			if ok {
				if _, exists := managedSet[hostValue]; exists {
					return true
				}
			}
		}
	}

	return false
}
