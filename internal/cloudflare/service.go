package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const defaultBaseURL = "https://api.cloudflare.com/client/v4"

type Client struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type DNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

type apiResponse[T any] struct {
	Success bool       `json:"success"`
	Errors  []apiError `json:"errors"`
	Result  T          `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type recordRequest struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

func New(token string) (*Client, error) {
	return NewWithBaseURL(token, defaultBaseURL, nil)
}

func NewWithBaseURL(token, baseURL string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("token must not be empty")
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
		token:      token,
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
	}, nil
}

func (c *Client) ValidateToken(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/user/tokens/verify", nil)
	return err
}

func (c *Client) ListZones(ctx context.Context, name string) ([]Zone, error) {
	path := "/zones"
	if name != "" {
		path += "?name=" + url.QueryEscape(name)
	}

	body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var response apiResponse[[]Zone]
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode zones response: %w", err)
	}

	if !response.Success {
		return nil, fmt.Errorf("list zones failed: %s", formatErrors(response.Errors))
	}

	return response.Result, nil
}

func (c *Client) UpsertCNAME(ctx context.Context, zoneID, fqdn, target string, proxied bool) (DNSRecord, bool, error) {
	existing, err := c.listDNSRecords(ctx, zoneID, "CNAME", fqdn)
	if err != nil {
		return DNSRecord{}, false, err
	}

	if len(existing) > 0 {
		record := existing[0]
		if record.Content == target && record.Proxied == proxied {
			return record, false, nil
		}

		body, err := c.do(ctx, http.MethodPut, fmt.Sprintf("/zones/%s/dns_records/%s", zoneID, record.ID), recordRequest{
			Type:    "CNAME",
			Name:    fqdn,
			Content: target,
			Proxied: proxied,
		})
		if err != nil {
			return DNSRecord{}, false, err
		}

		var response apiResponse[DNSRecord]
		if err := json.Unmarshal(body, &response); err != nil {
			return DNSRecord{}, false, fmt.Errorf("decode update dns record response: %w", err)
		}
		if !response.Success {
			return DNSRecord{}, false, fmt.Errorf("update dns record failed: %s", formatErrors(response.Errors))
		}

		return response.Result, true, nil
	}

	body, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/zones/%s/dns_records", zoneID), recordRequest{
		Type:    "CNAME",
		Name:    fqdn,
		Content: target,
		Proxied: proxied,
	})
	if err != nil {
		return DNSRecord{}, false, err
	}

	var response apiResponse[DNSRecord]
	if err := json.Unmarshal(body, &response); err != nil {
		return DNSRecord{}, false, fmt.Errorf("decode create dns record response: %w", err)
	}
	if !response.Success {
		return DNSRecord{}, false, fmt.Errorf("create dns record failed: %s", formatErrors(response.Errors))
	}

	return response.Result, true, nil
}

func (c *Client) DeleteCNAME(ctx context.Context, zoneID, fqdn string) error {
	records, err := c.listDNSRecords(ctx, zoneID, "CNAME", fqdn)
	if err != nil {
		return err
	}

	for _, record := range records {
		if _, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/zones/%s/dns_records/%s", zoneID, record.ID), nil); err != nil {
			return err
		}
	}

	return nil
}

func (c *Client) listDNSRecords(ctx context.Context, zoneID, recordType, name string) ([]DNSRecord, error) {
	path := fmt.Sprintf("/zones/%s/dns_records?type=%s&name=%s", zoneID, url.QueryEscape(recordType), url.QueryEscape(name))

	body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var response apiResponse[[]DNSRecord]
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode dns records response: %w", err)
	}
	if !response.Success {
		return nil, fmt.Errorf("list dns records failed: %s", formatErrors(response.Errors))
	}

	return response.Result, nil
}

func (c *Client) do(ctx context.Context, method, path string, payload any) ([]byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal request payload: %w", err)
		}
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response %s %s: %w", method, path, err)
	}

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("cloudflare api %s %s returned %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}

	return body, nil
}

func formatErrors(errors []apiError) string {
	if len(errors) == 0 {
		return "unknown error"
	}

	parts := make([]string, 0, len(errors))
	for _, err := range errors {
		if err.Code == 0 {
			parts = append(parts, err.Message)
			continue
		}
		parts = append(parts, fmt.Sprintf("%d: %s", err.Code, err.Message))
	}

	return strings.Join(parts, "; ")
}
