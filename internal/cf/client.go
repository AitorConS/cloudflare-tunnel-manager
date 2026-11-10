// Package cf is a small client for the parts of the Cloudflare v4 API the
// manager needs: accounts, zones, tunnels, DNS records and Access apps.
package cf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultBase = "https://api.cloudflare.com/client/v4"

type Client struct {
	token string
	base  string
	http  *http.Client
}

func New(token string) *Client {
	return &Client{token: token, base: defaultBase, http: &http.Client{Timeout: 30 * time.Second}}
}

// NewWithBase points the client at another API root (used by tests).
func NewWithBase(token, base string) *Client {
	c := New(token)
	c.base = strings.TrimRight(base, "/")
	return c
}

type ErrorItem struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type APIError struct {
	Status int
	Errors []ErrorItem
}

func (e *APIError) Error() string {
	if len(e.Errors) == 0 {
		return fmt.Sprintf("Cloudflare API: HTTP %d", e.Status)
	}
	parts := make([]string, len(e.Errors))
	for i, it := range e.Errors {
		parts[i] = fmt.Sprintf("%s (code %d)", it.Message, it.Code)
	}
	return "Cloudflare API: " + strings.Join(parts, "; ")
}

// IsNotFound reports whether err is a Cloudflare 404, i.e. the remote object
// is already gone.
func IsNotFound(err error) bool {
	e, ok := err.(*APIError)
	return ok && e.Status == http.StatusNotFound
}

// IsAuth reports whether err is an authentication/permission failure.
func IsAuth(err error) bool {
	e, ok := err.(*APIError)
	return ok && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
}

type envelope struct {
	Success    bool            `json:"success"`
	Errors     []ErrorItem     `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) (*envelope, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the Cloudflare API: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &APIError{Status: resp.StatusCode}
	}
	if resp.StatusCode >= 400 || !env.Success {
		return nil, &APIError{Status: resp.StatusCode, Errors: env.Errors}
	}
	if out != nil && len(env.Result) > 0 && string(env.Result) != "null" {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return nil, fmt.Errorf("unexpected response from Cloudflare: %w", err)
		}
	}
	return &env, nil
}

// list walks every page of a paginated collection.
func list[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("per_page", "50")
	all := []T{}
	for page := 1; page <= 100; page++ {
		query.Set("page", strconv.Itoa(page))
		var items []T
		env, err := c.do(ctx, http.MethodGet, path, query, nil, &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		// Not every collection reports total_pages, so also stop on a short page.
		if len(items) < 50 || (env.ResultInfo.TotalPages > 0 && env.ResultInfo.TotalPages <= page) {
			break
		}
	}
	return all, nil
}

// ---- accounts & zones ----

type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	return list[Account](ctx, c, "/accounts", nil)
}

type Zone struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Status  string  `json:"status"`
	Account Account `json:"account"`
}

// Zones lists the zones the token can see, optionally only those of one account.
func (c *Client) Zones(ctx context.Context, accountID string) ([]Zone, error) {
	q := url.Values{}
	if accountID != "" {
		q.Set("account.id", accountID)
	}
	return list[Zone](ctx, c, "/zones", q)
}

// CheckTunnels reports whether the token can reach the account's tunnels.
func (c *Client) CheckTunnels(ctx context.Context, accountID string) error {
	var raw json.RawMessage
	_, err := c.do(ctx, http.MethodGet, tunnelPath(accountID, ""), url.Values{"is_deleted": {"false"}, "per_page": {"1"}}, nil, &raw)
	return err
}

// ---- tunnels ----

type TunnelConn struct {
	ColoName string `json:"colo_name"`
	OriginIP string `json:"origin_ip"`
	OpenedAt string `json:"opened_at"`
}

type Tunnel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"` // inactive | healthy | degraded | down
	CreatedAt string `json:"created_at"`
}

func tunnelPath(accountID, tunnelID string) string {
	p := "/accounts/" + accountID + "/cfd_tunnel"
	if tunnelID != "" {
		p += "/" + tunnelID
	}
	return p
}

// CreateTunnel creates a remotely-managed tunnel (its ingress lives in Cloudflare).
func (c *Client) CreateTunnel(ctx context.Context, accountID, name string) (*Tunnel, error) {
	t := &Tunnel{}
	_, err := c.do(ctx, http.MethodPost, tunnelPath(accountID, ""), nil,
		map[string]any{"name": name, "config_src": "cloudflare"}, t)
	return t, err
}

func (c *Client) Tunnel(ctx context.Context, accountID, tunnelID string) (*Tunnel, error) {
	t := &Tunnel{}
	_, err := c.do(ctx, http.MethodGet, tunnelPath(accountID, tunnelID), nil, nil, t)
	return t, err
}

func (c *Client) TunnelToken(ctx context.Context, accountID, tunnelID string) (string, error) {
	var token string
	_, err := c.do(ctx, http.MethodGet, tunnelPath(accountID, tunnelID)+"/token", nil, nil, &token)
	return token, err
}

type IngressRule struct {
	Hostname      string         `json:"hostname,omitempty"`
	Path          string         `json:"path,omitempty"`
	Service       string         `json:"service"`
	OriginRequest map[string]any `json:"originRequest,omitempty"`
}

// PutTunnelConfig replaces the whole ingress configuration of the tunnel.
func (c *Client) PutTunnelConfig(ctx context.Context, accountID, tunnelID string, ingress []IngressRule) error {
	body := map[string]any{"config": map[string]any{"ingress": ingress}}
	_, err := c.do(ctx, http.MethodPut, tunnelPath(accountID, tunnelID)+"/configurations", nil, body, nil)
	return err
}

// Connector is one cloudflared instance attached to the tunnel.
type Connector struct {
	ID      string       `json:"id"`
	Version string       `json:"version"`
	Arch    string       `json:"arch"`
	RunAt   string       `json:"run_at"`
	Conns   []TunnelConn `json:"conns"`
}

func (c *Client) TunnelConnectors(ctx context.Context, accountID, tunnelID string) ([]Connector, error) {
	out := []Connector{}
	_, err := c.do(ctx, http.MethodGet, tunnelPath(accountID, tunnelID)+"/connections", nil, nil, &out)
	return out, err
}

// DeleteTunnel drops stale connections first: Cloudflare refuses to delete a
// tunnel that still has them registered.
func (c *Client) DeleteTunnel(ctx context.Context, accountID, tunnelID string) error {
	_, _ = c.do(ctx, http.MethodDelete, tunnelPath(accountID, tunnelID)+"/connections", nil, nil, nil)
	// cascade (what cloudflared itself sends) also drops connections that
	// external connectors re-register in the meantime.
	_, err := c.do(ctx, http.MethodDelete, tunnelPath(accountID, tunnelID), url.Values{"cascade": {"true"}}, nil, nil)
	return err
}

// ---- DNS ----

type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
	Comment string `json:"comment,omitempty"`
}

func (c *Client) DNSRecordsByName(ctx context.Context, zoneID, name string) ([]DNSRecord, error) {
	return list[DNSRecord](ctx, c, "/zones/"+zoneID+"/dns_records", url.Values{"name": {name}})
}

func (c *Client) CreateDNSRecord(ctx context.Context, zoneID string, rec DNSRecord) (*DNSRecord, error) {
	out := &DNSRecord{}
	_, err := c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", nil, rec, out)
	return out, err
}

func (c *Client) UpdateDNSRecord(ctx context.Context, zoneID, id string, rec DNSRecord) (*DNSRecord, error) {
	out := &DNSRecord{}
	_, err := c.do(ctx, http.MethodPut, "/zones/"+zoneID+"/dns_records/"+id, nil, rec, out)
	return out, err
}

func (c *Client) DeleteDNSRecord(ctx context.Context, zoneID, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+id, nil, nil, nil)
	return err
}
