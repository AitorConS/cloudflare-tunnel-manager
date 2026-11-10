package core

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"tunnelmanager/internal/cf"
	"tunnelmanager/internal/store"
)

// HostInput is what the UI sends to create or edit a proxy host.
type HostInput struct {
	TunnelID     int64               `json:"tunnelId"`
	Hostname     string              `json:"hostname"`
	Path         string              `json:"path"`
	Service      string              `json:"service"`
	Enabled      bool                `json:"enabled"`
	Options      store.OriginOptions `json:"options"`
	Access       store.AccessConfig  `json:"access"`
	OverwriteDNS bool                `json:"overwriteDns"`
}

var (
	hostnameRe = regexp.MustCompile(`^(\*\.)?([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	domainRe   = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	emailRe    = regexp.MustCompile(`^[^\s@,]+@[^\s@,]+\.[^\s@,]+$`)
	statusRe   = regexp.MustCompile(`^http_status:[1-5]\d\d$`)
)

var urlSchemes = map[string]bool{"http": true, "https": true, "tcp": true, "ssh": true, "rdp": true, "smb": true}

var sessionDurations = map[string]bool{
	"30m": true, "6h": true, "12h": true, "24h": true, "168h": true, "730h": true,
}

// NormalizeService validates a cloudflared service string and returns its
// canonical form.
func NormalizeService(svc string) (string, error) {
	svc = strings.TrimSpace(svc)
	if svc == "" {
		return "", invalid("the destination (service) is required")
	}
	if statusRe.MatchString(svc) || svc == "hello_world" {
		return svc, nil
	}
	for _, p := range []string{"unix:", "unix+tls:"} {
		if rest, ok := strings.CutPrefix(svc, p); ok {
			if !strings.HasPrefix(rest, "/") || strings.ContainsAny(rest, " \t\n") {
				return "", invalid("the unix socket must be an absolute path, e.g. unix:/run/app.sock")
			}
			return svc, nil
		}
	}
	u, err := url.Parse(svc)
	if err != nil || !urlSchemes[u.Scheme] {
		return "", invalid("invalid destination: use something like http://192.168.1.10:8080 (schemes: http, https, tcp, ssh, rdp, smb, unix)")
	}
	if u.Hostname() == "" {
		return "", invalid("the destination is missing a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", invalid("the destination only accepts scheme, host and port (no path, user or query parameters)")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", invalid("invalid destination port")
		}
	} else if u.Scheme != "http" && u.Scheme != "https" {
		return "", invalid("specify the destination port, e.g. %s://%s:22", u.Scheme, u.Hostname())
	}
	return u.Scheme + "://" + u.Host, nil
}

func cleanList(in []string, re *regexp.Regexp, what string) ([]string, error) {
	out, seen := []string{}, map[string]bool{}
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || seen[v] {
			continue
		}
		if !re.MatchString(v) {
			return nil, invalid("invalid %s: %s", what, v)
		}
		seen[v] = true
		out = append(out, v)
	}
	return out, nil
}

// normalize validates in and writes the user-editable fields onto h.
func (in *HostInput) normalize(h *store.Host) error {
	hostname := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.Hostname), "."))
	if len(hostname) > 253 || !hostnameRe.MatchString(hostname) {
		return invalid("invalid hostname: use something like app.example.com or *.example.com")
	}
	path := strings.TrimSpace(in.Path)
	if path != "" {
		if len(path) > 255 {
			return invalid("the path is too long")
		}
		if _, err := regexp.Compile(path); err != nil {
			return invalid("the path must be a valid regular expression: %v", err)
		}
	}
	service, err := NormalizeService(in.Service)
	if err != nil {
		return err
	}
	o := in.Options
	o.HTTPHostHeader = strings.TrimSpace(o.HTTPHostHeader)
	o.OriginServerName = strings.TrimSpace(o.OriginServerName)
	if strings.ContainsAny(o.HTTPHostHeader+o.OriginServerName, " \t\r\n") {
		return invalid("the Host header and the TLS server name cannot contain spaces")
	}
	if o.ConnectTimeout < 0 || o.ConnectTimeout > 300 {
		return invalid("the connect timeout must be between 0 and 300 seconds")
	}
	a := in.Access
	if a.Emails, err = cleanList(a.Emails, emailRe, "email"); err != nil {
		return err
	}
	if a.EmailDomains, err = cleanList(a.EmailDomains, domainRe, "email domain"); err != nil {
		return err
	}
	if a.SessionDuration == "" {
		a.SessionDuration = "24h"
	}
	if !sessionDurations[a.SessionDuration] {
		return invalid("invalid session duration")
	}
	if a.Enabled {
		if path != "" {
			return invalid("Access protects the whole hostname: enable it on the host that has no path")
		}
		if len(a.Emails)+len(a.EmailDomains) == 0 {
			return invalid("Access needs at least one allowed email or email domain")
		}
	}
	h.TunnelID, h.Hostname, h.Path, h.Service, h.Enabled = in.TunnelID, hostname, path, service, in.Enabled
	h.Options, h.Access = o, a
	return nil
}

// checkSiblings enforces the rules between hosts sharing a hostname: unique
// path, and a single tunnel (one DNS record can only point to one tunnel).
func (s *Service) checkSiblings(h *store.Host) error {
	siblings, err := s.Store.HostsByHostname(h.Hostname)
	if err != nil {
		return err
	}
	for _, sib := range siblings {
		if sib.ID == h.ID {
			continue
		}
		if sib.Path == h.Path {
			return invalid("a host for %s with the same path already exists", h.Hostname)
		}
		if sib.TunnelID != h.TunnelID {
			return invalid("%s is already served by another tunnel; all of its paths must use the same tunnel", h.Hostname)
		}
	}
	return nil
}

func (s *Service) resolveZone(ctx context.Context, h *store.Host) error {
	zones, err := s.Zones(ctx)
	if err != nil {
		return err
	}
	z := zoneFor(zones, h.Hostname)
	if z == nil {
		return invalid("no zone in your Cloudflare account contains %s", h.Hostname)
	}
	h.ZoneID, h.ZoneName = z.ID, z.Name
	return nil
}

// publish runs the remote steps that follow a stored host change. Access goes
// first so a protected host is never reachable without its Access app.
// Failures are saved on the host (sync_error) instead of being returned: the
// host exists locally and can be re-synced.
func (s *Service) publish(ctx context.Context, c *cf.Client, acc string, h *store.Host, tunnels ...*store.Tunnel) {
	var errs []error
	if err := s.ensureAccess(ctx, c, acc, h); err != nil {
		errs = append(errs, err)
	}
	// Persist the Access ids before building the ingress, which reads them.
	_ = s.Store.UpdateHost(h)
	for _, t := range tunnels {
		if err := s.applyIngress(ctx, c, acc, t, 0); err != nil {
			errs = append(errs, err)
		}
	}
	h.SyncError = ""
	if err := errors.Join(errs...); err != nil {
		h.SyncError = err.Error()
	}
	_ = s.Store.UpdateHost(h)
}

func (s *Service) CreateHost(ctx context.Context, in HostInput) (*store.Host, error) {
	h := &store.Host{}
	if err := in.normalize(h); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.Store.Tunnel(h.TunnelID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, invalid("choose a tunnel")
	} else if err != nil {
		return nil, err
	}
	c, acc, err := s.client()
	if err != nil {
		return nil, err
	}
	if err := s.resolveZone(ctx, h); err != nil {
		return nil, err
	}
	if err := s.checkSiblings(h); err != nil {
		return nil, err
	}
	if h.DNSRecordID, err = s.ensureDNS(ctx, c, h, t, in.OverwriteDNS); err != nil {
		return nil, err
	}
	if err := s.Store.InsertHost(h); err != nil {
		return nil, err
	}
	s.publish(ctx, c, acc, h, t)
	return h, nil
}

func (s *Service) UpdateHost(ctx context.Context, id int64, in HostInput) (*store.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.Store.Host(id)
	if err != nil {
		return nil, err
	}
	h := *old
	if err := in.normalize(&h); err != nil {
		return nil, err
	}
	t, err := s.Store.Tunnel(h.TunnelID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, invalid("choose a tunnel")
	} else if err != nil {
		return nil, err
	}
	c, acc, err := s.client()
	if err != nil {
		return nil, err
	}
	renamed := h.Hostname != old.Hostname
	if renamed {
		if err := s.resolveZone(ctx, &h); err != nil {
			return nil, err
		}
	}
	if err := s.checkSiblings(&h); err != nil {
		return nil, err
	}
	if renamed || h.TunnelID != old.TunnelID || h.DNSRecordID == "" {
		if renamed {
			h.DNSRecordID = ""
		}
		if h.DNSRecordID, err = s.ensureDNS(ctx, c, &h, t, in.OverwriteDNS); err != nil {
			return nil, err
		}
	}
	if err := s.Store.UpdateHost(&h); err != nil {
		return nil, err
	}
	tunnels := []*store.Tunnel{t}
	if h.TunnelID != old.TunnelID {
		if prev, err := s.Store.Tunnel(old.TunnelID); err == nil {
			tunnels = append(tunnels, prev)
		}
	}
	s.publish(ctx, c, acc, &h, tunnels...)
	if renamed {
		if err := s.releaseDNS(ctx, c, old.ZoneID, old.Hostname, h.ID, 0); err != nil && h.SyncError == "" {
			h.SyncError = "could not delete the old DNS record for " + old.Hostname + ": " + err.Error()
			_ = s.Store.UpdateHost(&h)
		}
	}
	return &h, nil
}

// SetHostEnabled takes a host on or off line without touching DNS or Access.
func (s *Service) SetHostEnabled(ctx context.Context, id int64, enabled bool) (*store.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.Store.Host(id)
	if err != nil {
		return nil, err
	}
	t, err := s.Store.Tunnel(h.TunnelID)
	if err != nil {
		return nil, err
	}
	c, acc, err := s.client()
	if err != nil {
		return nil, err
	}
	h.Enabled = enabled
	if err := s.Store.UpdateHost(h); err != nil {
		return nil, err
	}
	s.publish(ctx, c, acc, h, t)
	return h, nil
}

// DeleteHost takes the host offline, removes its Access app and DNS record and
// then forgets it. With force, remote errors do not stop the local deletion.
func (s *Service) DeleteHost(ctx context.Context, id int64, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.Store.Host(id)
	if err != nil {
		return err
	}
	c, acc, err := s.client()
	if err != nil && !force {
		return err
	}
	if c != nil {
		t, err := s.Store.Tunnel(h.TunnelID)
		if err == nil {
			err = s.applyIngress(ctx, c, acc, t, h.ID)
		}
		if err == nil {
			err = s.cleanupHostRemote(ctx, c, acc, h, 0)
		}
		if err != nil && !force {
			return err
		}
	}
	return s.Store.DeleteHost(id)
}

// SyncResult summarizes a full reconciliation of a tunnel.
type SyncResult struct {
	Hosts  int      `json:"hosts"`
	Errors []string `json:"errors"`
}

// SyncTunnel re-applies everything the tunnel's hosts need in Cloudflare:
// DNS records (never overwriting foreign ones), Access apps and ingress.
func (s *Service) SyncTunnel(ctx context.Context, id int64) (*SyncResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.Store.Tunnel(id)
	if err != nil {
		return nil, err
	}
	c, acc, err := s.client()
	if err != nil {
		return nil, err
	}
	hosts, err := s.Store.HostsByTunnel(id)
	if err != nil {
		return nil, err
	}
	res := &SyncResult{Hosts: len(hosts), Errors: []string{}}
	for _, h := range hosts {
		var errs []error
		if recID, err := s.ensureDNS(ctx, c, h, t, false); err != nil {
			errs = append(errs, err)
		} else {
			h.DNSRecordID = recID
		}
		if err := s.ensureAccess(ctx, c, acc, h); err != nil {
			errs = append(errs, err)
		}
		h.SyncError = ""
		if err := errors.Join(errs...); err != nil {
			h.SyncError = err.Error()
			res.Errors = append(res.Errors, h.Hostname+": "+h.SyncError)
		}
		if err := s.Store.UpdateHost(h); err != nil {
			return nil, err
		}
	}
	if err := s.applyIngress(ctx, c, acc, t, 0); err != nil {
		res.Errors = append(res.Errors, "tunnel rules: "+err.Error())
	}
	return res, nil
}
