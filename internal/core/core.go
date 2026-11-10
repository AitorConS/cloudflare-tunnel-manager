// Package core holds the business logic: it validates what the user asks for,
// stores it, and reconciles Cloudflare (tunnel ingress, DNS, Access) with it.
package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"tunnelmanager/internal/cf"
	"tunnelmanager/internal/runner"
	"tunnelmanager/internal/secret"
	"tunnelmanager/internal/store"
)

const tunnelDNSSuffix = ".cfargotunnel.com"

var ErrNotConfigured = errors.New("Cloudflare is not configured: add an API token in Settings")

// ValidationError is a user-fixable problem with the request.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{Msg: fmt.Sprintf(format, a...)} }

// DNSConflictError means records for the hostname already exist and point
// somewhere else; the caller must confirm before they are replaced.
type DNSConflictError struct {
	Hostname string
	Records  []cf.DNSRecord
}

func (e *DNSConflictError) Error() string {
	return "DNS records already exist for " + e.Hostname
}

type Service struct {
	Store  *store.Store
	Box    *secret.Box
	Runner *runner.Manager

	// NewClient builds the Cloudflare client; tests replace it.
	NewClient func(token string) *cf.Client

	mu sync.Mutex // serializes every mutation that touches Cloudflare

	cacheMu     sync.Mutex
	zones       []cf.Zone
	zonesAt     time.Time
	remote      map[int64]*RemoteStatus
	remoteAt    map[int64]time.Time
	accessReady *bool
}

func New(st *store.Store, box *secret.Box, rn *runner.Manager) *Service {
	return &Service{
		Store: st, Box: box, Runner: rn, NewClient: cf.New,
		remote: map[int64]*RemoteStatus{}, remoteAt: map[int64]time.Time{},
	}
}

// ---- Cloudflare connection ----

const (
	keyToken       = "cf_token_enc"
	keyAccountID   = "cf_account_id"
	keyAccountName = "cf_account_name"
)

func (s *Service) client() (*cf.Client, string, error) {
	enc, err := s.Store.Setting(keyToken)
	if err != nil {
		return nil, "", err
	}
	acc, err := s.Store.Setting(keyAccountID)
	if err != nil {
		return nil, "", err
	}
	if enc == "" || acc == "" {
		return nil, "", ErrNotConfigured
	}
	token, err := s.Box.Open(enc)
	if err != nil {
		return nil, "", fmt.Errorf("could not decrypt the API token: %w", err)
	}
	return s.NewClient(token), acc, nil
}

type CloudflareInfo struct {
	Configured  bool   `json:"configured"`
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
}

func (s *Service) CloudflareInfo() (*CloudflareInfo, error) {
	enc, err := s.Store.Setting(keyToken)
	if err != nil {
		return nil, err
	}
	id, _ := s.Store.Setting(keyAccountID)
	name, _ := s.Store.Setting(keyAccountName)
	return &CloudflareInfo{Configured: enc != "" && id != "", AccountID: id, AccountName: name}, nil
}

// ConfigureCloudflare validates the token and stores it. When the token can
// see several accounts and accountID is empty, nothing is saved and the list
// is returned so the user can choose.
func (s *Service) ConfigureCloudflare(ctx context.Context, token, accountID string) (chosen *cf.Account, choices []cf.Account, err error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, nil, invalid("the API token is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.NewClient(token)
	accounts, err := discoverAccounts(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	for i := range accounts {
		if accounts[i].ID == accountID || (accountID == "" && len(accounts) == 1) {
			chosen = &accounts[i]
		}
	}
	if chosen == nil {
		if accountID != "" {
			return nil, nil, invalid("the token does not have access to the selected account")
		}
		return nil, accounts, nil
	}
	// A different account would orphan every tunnel we manage.
	if cur, _ := s.Store.Setting(keyAccountID); cur != "" && cur != chosen.ID {
		if tunnels, err := s.Store.Tunnels(); err != nil {
			return nil, nil, err
		} else if len(tunnels) > 0 {
			return nil, nil, invalid("this token belongs to a different account; delete the existing tunnels first")
		}
	}
	// Fail early if the token cannot manage tunnels at all.
	if _, err := c.Zones(ctx, chosen.ID); err != nil {
		return nil, nil, invalid("the token cannot list zones (permission Zone → Zone → Read): %v", err)
	}
	enc, err := s.Box.Seal(token)
	if err != nil {
		return nil, nil, err
	}
	if err := s.Store.SetSettings(map[string]string{
		keyToken: enc, keyAccountID: chosen.ID, keyAccountName: chosen.Name,
	}); err != nil {
		return nil, nil, err
	}
	s.cacheMu.Lock()
	s.zones, s.zonesAt, s.accessReady = nil, time.Time{}, nil
	s.cacheMu.Unlock()
	return chosen, nil, nil
}

func (s *Service) Zones(ctx context.Context) ([]cf.Zone, error) {
	s.cacheMu.Lock()
	if s.zones != nil && time.Since(s.zonesAt) < time.Minute {
		z := s.zones
		s.cacheMu.Unlock()
		return z, nil
	}
	s.cacheMu.Unlock()
	c, acc, err := s.client()
	if err != nil {
		return nil, err
	}
	zones, err := c.Zones(ctx, acc)
	if err != nil {
		return nil, err
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].Name < zones[j].Name })
	s.cacheMu.Lock()
	s.zones, s.zonesAt = zones, time.Now()
	s.cacheMu.Unlock()
	return zones, nil
}

// zoneFor picks the most specific zone that contains hostname.
func zoneFor(zones []cf.Zone, hostname string) *cf.Zone {
	name := strings.TrimPrefix(hostname, "*.")
	var best *cf.Zone
	for i := range zones {
		z := &zones[i]
		if name == z.Name || strings.HasSuffix(name, "."+z.Name) {
			if best == nil || len(z.Name) > len(best.Name) {
				best = z
			}
		}
	}
	return best
}

// ---- tunnels ----

func (s *Service) CreateTunnel(ctx context.Context, name, mode string) (*store.Tunnel, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return nil, invalid("the tunnel name is required (max. 100 characters)")
	}
	if mode != store.ModeManaged && mode != store.ModeExternal {
		return nil, invalid("invalid tunnel mode")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, acc, err := s.client()
	if err != nil {
		return nil, err
	}
	remote, err := c.CreateTunnel(ctx, acc, name)
	if err != nil {
		return nil, err
	}
	// From here on a failure would leave an orphan tunnel in Cloudflare, so undo it.
	fail := func(err error) (*store.Tunnel, error) {
		_ = c.DeleteTunnel(context.WithoutCancel(ctx), acc, remote.ID)
		return nil, err
	}
	token, err := c.TunnelToken(ctx, acc, remote.ID)
	if err != nil {
		return fail(err)
	}
	enc, err := s.Box.Seal(token)
	if err != nil {
		return fail(err)
	}
	t := &store.Tunnel{CFID: remote.ID, Name: remote.Name, Mode: mode, TokenEnc: enc, Enabled: true}
	if err := s.Store.InsertTunnel(t); err != nil {
		return fail(err)
	}
	if err := s.applyIngress(ctx, c, acc, t, 0); err != nil {
		t.LastSyncError = err.Error()
	}
	if mode == store.ModeManaged {
		s.Runner.Start(t.ID, token)
	}
	return t, nil
}

// TunnelToken returns the connector token, for running cloudflared elsewhere.
func (s *Service) TunnelToken(id int64) (string, error) {
	t, err := s.Store.Tunnel(id)
	if err != nil {
		return "", err
	}
	return s.Box.Open(t.TokenEnc)
}

// SetTunnelRun changes where/whether the connector runs and applies it.
func (s *Service) SetTunnelRun(id int64, mode string, enabled bool) (*store.Tunnel, error) {
	if mode != store.ModeManaged && mode != store.ModeExternal {
		return nil, invalid("invalid tunnel mode")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.Store.Tunnel(id)
	if err != nil {
		return nil, err
	}
	if err := s.Store.UpdateTunnelRun(id, mode, enabled); err != nil {
		return nil, err
	}
	t.Mode, t.Enabled = mode, enabled
	s.Runner.Stop(id)
	if err := s.startIfManaged(t); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Service) RestartTunnel(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.Store.Tunnel(id)
	if err != nil {
		return err
	}
	s.Runner.Stop(id)
	return s.startIfManaged(t)
}

func (s *Service) startIfManaged(t *store.Tunnel) error {
	if t.Mode != store.ModeManaged || !t.Enabled {
		return nil
	}
	token, err := s.Box.Open(t.TokenEnc)
	if err != nil {
		return fmt.Errorf("could not decrypt the tunnel token: %w", err)
	}
	s.Runner.Start(t.ID, token)
	return nil
}

// StartAll launches every enabled managed tunnel; called once at boot.
func (s *Service) StartAll() error {
	tunnels, err := s.Store.Tunnels()
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range tunnels {
		if err := s.startIfManaged(t); err != nil {
			errs = append(errs, fmt.Errorf("tunnel %s: %w", t.Name, err))
		}
	}
	return errors.Join(errs...)
}

// DeleteTunnel removes the tunnel, its hosts and everything they created in
// Cloudflare. With force, remote cleanup errors are ignored.
func (s *Service) DeleteTunnel(ctx context.Context, id int64, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.Store.Tunnel(id)
	if err != nil {
		return err
	}
	hosts, err := s.Store.HostsByTunnel(id)
	if err != nil {
		return err
	}
	c, acc, err := s.client()
	if err != nil && !force {
		return err
	}
	s.Runner.Stop(id)
	if c != nil {
		for _, h := range hosts {
			if err := s.cleanupHostRemote(ctx, c, acc, h, id); err != nil && !force {
				_ = s.startIfManaged(t)
				return fmt.Errorf("%s: %w", h.Hostname, err)
			}
		}
		if err := c.DeleteTunnel(ctx, acc, t.CFID); err != nil && !cf.IsNotFound(err) && !force {
			_ = s.startIfManaged(t)
			return err
		}
	}
	for _, h := range hosts {
		if err := s.Store.DeleteHost(h.ID); err != nil {
			return err
		}
	}
	return s.Store.DeleteTunnel(id)
}

// RemoteStatus is what Cloudflare reports about a tunnel.
type RemoteStatus struct {
	Status     string         `json:"status"`
	Connectors []cf.Connector `json:"connectors"`
	Error      string         `json:"error,omitempty"`
}

// RemoteStatus is cached briefly: the UI polls it for every tunnel.
func (s *Service) RemoteStatus(ctx context.Context, t *store.Tunnel) *RemoteStatus {
	s.cacheMu.Lock()
	if rs, ok := s.remote[t.ID]; ok && time.Since(s.remoteAt[t.ID]) < 8*time.Second {
		s.cacheMu.Unlock()
		return rs
	}
	s.cacheMu.Unlock()
	rs := &RemoteStatus{Status: "unknown", Connectors: []cf.Connector{}}
	c, acc, err := s.client()
	if err == nil {
		var rt *cf.Tunnel
		if rt, err = c.Tunnel(ctx, acc, t.CFID); err == nil {
			rs.Status = rt.Status
			rs.Connectors, err = c.TunnelConnectors(ctx, acc, t.CFID)
		}
	}
	if err != nil {
		rs.Error = err.Error()
	}
	s.cacheMu.Lock()
	s.remote[t.ID], s.remoteAt[t.ID] = rs, time.Now()
	s.cacheMu.Unlock()
	return rs
}

// ---- ingress ----

// buildIngress turns the enabled hosts of a tunnel into cloudflared rules.
// cloudflared takes the first match, so exact hostnames go before wildcards
// and rules with a path before the bare hostname.
func buildIngress(hosts []*store.Host) []cf.IngressRule {
	live := make([]*store.Host, 0, len(hosts))
	for _, h := range hosts {
		// Never publish a host that asks for Access before its Access app exists.
		if h.Enabled && !(h.Access.Enabled && h.AccessAppID == "") {
			live = append(live, h)
		}
	}
	sort.SliceStable(live, func(i, j int) bool {
		a, b := live[i], live[j]
		aw, bw := strings.HasPrefix(a.Hostname, "*."), strings.HasPrefix(b.Hostname, "*.")
		if aw != bw {
			return !aw
		}
		if a.Hostname != b.Hostname {
			// Longer wildcard suffixes are more specific.
			if aw && len(a.Hostname) != len(b.Hostname) {
				return len(a.Hostname) > len(b.Hostname)
			}
			return a.Hostname < b.Hostname
		}
		if (a.Path == "") != (b.Path == "") {
			return a.Path != ""
		}
		if len(a.Path) != len(b.Path) {
			return len(a.Path) > len(b.Path)
		}
		return a.ID < b.ID
	})
	rules := make([]cf.IngressRule, 0, len(live)+1)
	for _, h := range live {
		r := cf.IngressRule{Hostname: h.Hostname, Path: h.Path, Service: h.Service}
		o, origin := h.Options, map[string]any{}
		if o.NoTLSVerify {
			origin["noTLSVerify"] = true
		}
		if o.HTTPHostHeader != "" {
			origin["httpHostHeader"] = o.HTTPHostHeader
		}
		if o.OriginServerName != "" {
			origin["originServerName"] = o.OriginServerName
		}
		if o.HTTP2Origin {
			origin["http2Origin"] = true
		}
		if o.DisableChunkedEncoding {
			origin["disableChunkedEncoding"] = true
		}
		if o.ConnectTimeout > 0 {
			origin["connectTimeout"] = o.ConnectTimeout
		}
		if len(origin) > 0 {
			r.OriginRequest = origin
		}
		rules = append(rules, r)
	}
	return append(rules, cf.IngressRule{Service: "http_status:404"})
}

// applyIngress pushes the tunnel's rules to Cloudflare, leaving out excludeHost
// (a host about to be deleted). The outcome is recorded on the tunnel.
func (s *Service) applyIngress(ctx context.Context, c *cf.Client, acc string, t *store.Tunnel, excludeHost int64) error {
	hosts, err := s.Store.HostsByTunnel(t.ID)
	if err != nil {
		return err
	}
	kept := hosts[:0]
	for _, h := range hosts {
		if h.ID != excludeHost {
			kept = append(kept, h)
		}
	}
	err = c.PutTunnelConfig(ctx, acc, t.CFID, buildIngress(kept))
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_ = s.Store.SetTunnelSync(t.ID, msg)
	return err
}

// ---- DNS ----

func (s *Service) ourTargets() (map[string]bool, error) {
	tunnels, err := s.Store.Tunnels()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, t := range tunnels {
		out[t.CFID+tunnelDNSSuffix] = true
	}
	return out, nil
}

func isAddressRecord(t string) bool { return t == "A" || t == "AAAA" || t == "CNAME" }

// ensureDNS makes hostname a proxied CNAME to the tunnel. Records that point
// elsewhere are only replaced when overwrite is set.
func (s *Service) ensureDNS(ctx context.Context, c *cf.Client, h *store.Host, t *store.Tunnel, overwrite bool) (string, error) {
	target := t.CFID + tunnelDNSSuffix
	ours, err := s.ourTargets()
	if err != nil {
		return "", err
	}
	recs, err := c.DNSRecordsByName(ctx, h.ZoneID, h.Hostname)
	if err != nil {
		return "", err
	}
	var mine *cf.DNSRecord
	var conflicts []cf.DNSRecord
	for i := range recs {
		r := &recs[i]
		if !isAddressRecord(r.Type) || !strings.EqualFold(r.Name, h.Hostname) {
			continue
		}
		if r.Type == "CNAME" && mine == nil && (ours[strings.ToLower(r.Content)] || r.ID == h.DNSRecordID) {
			mine = r
		} else {
			conflicts = append(conflicts, *r)
		}
	}
	if len(conflicts) > 0 {
		if !overwrite {
			return "", &DNSConflictError{Hostname: h.Hostname, Records: conflicts}
		}
		for _, r := range conflicts {
			if err := c.DeleteDNSRecord(ctx, h.ZoneID, r.ID); err != nil && !cf.IsNotFound(err) {
				return "", err
			}
		}
	}
	want := cf.DNSRecord{Type: "CNAME", Name: h.Hostname, Content: target, Proxied: true, TTL: 1,
		Comment: dnsComment(t.Name)}
	if mine != nil {
		if strings.EqualFold(mine.Content, target) && mine.Proxied {
			return mine.ID, nil
		}
		rec, err := c.UpdateDNSRecord(ctx, h.ZoneID, mine.ID, want)
		if err != nil {
			return "", err
		}
		return rec.ID, nil
	}
	rec, err := c.CreateDNSRecord(ctx, h.ZoneID, want)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// releaseDNS deletes the tunnel CNAME of hostname unless another host still
// needs it (excludeHost and the hosts of excludeTunnel do not count: they are
// being removed). Records we did not create are left alone.
func (s *Service) releaseDNS(ctx context.Context, c *cf.Client, zoneID, hostname string, excludeHost, excludeTunnel int64) error {
	siblings, err := s.Store.HostsByHostname(hostname)
	if err != nil {
		return err
	}
	for _, sib := range siblings {
		if sib.ID != excludeHost && sib.TunnelID != excludeTunnel {
			return nil
		}
	}
	ours, err := s.ourTargets()
	if err != nil {
		return err
	}
	recs, err := c.DNSRecordsByName(ctx, zoneID, hostname)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.Type == "CNAME" && strings.EqualFold(r.Name, hostname) && ours[strings.ToLower(r.Content)] {
			if err := c.DeleteDNSRecord(ctx, zoneID, r.ID); err != nil && !cf.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

// ---- Access ----

// ensureAccess creates, updates or removes the Access application of a host
// and stores the resulting ids on h (not persisted here).
func (s *Service) ensureAccess(ctx context.Context, c *cf.Client, acc string, h *store.Host) error {
	if !h.Access.Enabled {
		if h.AccessAppID == "" {
			return nil
		}
		if err := c.DeleteAccessApp(ctx, acc, h.AccessAppID); err != nil && !cf.IsNotFound(err) {
			return err
		}
		h.AccessAppID, h.AccessPolicyID = "", ""
		return nil
	}
	appID, policyID, err := c.EnsureAccessApp(ctx, acc, h.AccessAppID, h.AccessPolicyID, cf.AccessSpec{
		Name:            h.Hostname,
		Domain:          h.Hostname,
		SessionDuration: h.Access.SessionDuration,
		Emails:          h.Access.Emails,
		EmailDomains:    h.Access.EmailDomains,
	})
	// Keep whatever ids were created even on a partial failure, so a retry
	// updates the same app instead of creating a second one.
	if appID != "" {
		h.AccessAppID = appID
	}
	if policyID != "" {
		h.AccessPolicyID = policyID
	}
	return err
}

// AccessAvailable reports whether the account has Zero Trust set up, which
// Access applications require. A positive answer is cached.
func (s *Service) AccessAvailable(ctx context.Context) (bool, string) {
	s.cacheMu.Lock()
	if s.accessReady != nil && *s.accessReady {
		s.cacheMu.Unlock()
		return true, ""
	}
	s.cacheMu.Unlock()
	c, acc, err := s.client()
	if err != nil {
		return false, err.Error()
	}
	if err := c.AccessOrganization(ctx, acc); err != nil {
		return false, err.Error()
	}
	ok := true
	s.cacheMu.Lock()
	s.accessReady = &ok
	s.cacheMu.Unlock()
	return true, ""
}

// cleanupHostRemote removes what a host created in Cloudflare besides its
// ingress rule: the Access app and the DNS record.
func (s *Service) cleanupHostRemote(ctx context.Context, c *cf.Client, acc string, h *store.Host, excludeTunnel int64) error {
	if h.AccessAppID != "" {
		if err := c.DeleteAccessApp(ctx, acc, h.AccessAppID); err != nil && !cf.IsNotFound(err) {
			return err
		}
	}
	return s.releaseDNS(ctx, c, h.ZoneID, h.Hostname, h.ID, excludeTunnel)
}

// dnsComment labels the records we create; Cloudflare caps comments at 100
// characters on free plans.
func dnsComment(tunnelName string) string {
	r := []rune("Tunnel Manager · " + tunnelName)
	if len(r) > 100 {
		r = r[:100]
	}
	return string(r)
}

// discoverAccounts finds the accounts a token works on. They are read from the
// zones it can see, so the token needs no account-level read permission; the
// accounts endpoint is only a fallback.
func discoverAccounts(ctx context.Context, c *cf.Client) ([]cf.Account, error) {
	zones, zerr := c.Zones(ctx, "")
	seen := map[string]bool{}
	var accounts []cf.Account
	for _, z := range zones {
		if z.Account.ID != "" && !seen[z.Account.ID] {
			seen[z.Account.ID] = true
			accounts = append(accounts, z.Account)
		}
	}
	if len(accounts) > 0 {
		return accounts, nil
	}
	accounts, aerr := c.Accounts(ctx)
	if len(accounts) > 0 {
		return accounts, nil
	}
	switch {
	case zerr != nil && cf.IsAuth(zerr) && (aerr == nil || cf.IsAuth(aerr)):
		return nil, invalid("Cloudflare rejected the token or it lacks the Zone → Zone → Read permission: %v", zerr)
	case zerr != nil:
		return nil, zerr
	default:
		return nil, invalid("the token is valid but cannot see any zone: include your domains under \"Zone Resources\"")
	}
}

// PermCheck is the live result of probing one capability of the stored token.
type PermCheck struct {
	Key      string `json:"key"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
	Optional bool   `json:"optional"`
}

// CheckPermissions probes, read-only, each API area the manager uses. A pass
// proves the token reaches that area; write access is only proven on first use.
func (s *Service) CheckPermissions(ctx context.Context) ([]PermCheck, error) {
	c, acc, err := s.client()
	if err != nil {
		return nil, err
	}
	result := func(key string, optional bool, detail string, err error) PermCheck {
		if err != nil {
			return PermCheck{Key: key, Optional: optional, Detail: err.Error()}
		}
		return PermCheck{Key: key, OK: true, Optional: optional, Detail: detail}
	}
	zones, zerr := c.Zones(ctx, acc)
	names := make([]string, len(zones))
	for i, z := range zones {
		names[i] = z.Name
	}
	if zerr == nil && len(zones) == 0 {
		zerr = errors.New("the token cannot see any zone in this account")
	}
	out := []PermCheck{result("zone", false, strings.Join(names, ", "), zerr)}
	if len(zones) > 0 {
		_, derr := c.DNSRecordsByName(ctx, zones[0].ID, zones[0].Name)
		out = append(out, result("dns", false, "", derr))
	} else {
		out = append(out, PermCheck{Key: "dns", Detail: "cannot be checked without zones"})
	}
	out = append(out, result("tunnel", false, "", c.CheckTunnels(ctx, acc)))
	out = append(out, result("access", true, "", c.AccessOrganization(ctx, acc)))
	return out, nil
}
