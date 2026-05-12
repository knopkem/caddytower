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

func normalizeJSON(raw []byte) ([]byte, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}

	return json.Marshal(decoded)
}
