// Package httpapi exposes the JSON API and serves the embedded web UI.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"tunnelmanager/internal/cf"
	"tunnelmanager/internal/core"
	"tunnelmanager/internal/runner"
	"tunnelmanager/internal/store"
	"tunnelmanager/internal/updater"
)

const (
	cookieName = "ctm_session"
	sessionTTL = 30 * 24 * time.Hour
	csrfHeader = "X-CTM"
)

type Server struct {
	Svc     *core.Service
	Static  fs.FS // built web UI; nil serves the API only
	Version string
	Updater *updater.Updater

	limiter loginLimiter
}

type ctxKey struct{}

func userOf(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxKey{}).(*store.User)
	return u
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	pub := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.guard(h, false)) }
	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.guard(h, true)) }

	pub("GET /api/status", s.status)
	pub("POST /api/setup", s.setup)
	pub("POST /api/login", s.login)
	pub("POST /api/logout", s.logout)
	api("PUT /api/password", s.changePassword)
	api("PUT /api/cloudflare", s.configureCloudflare)
	api("GET /api/cloudflare/check", s.checkCloudflare)
	api("GET /api/update", s.checkUpdate)
	api("POST /api/update", s.applyUpdate)
	api("GET /api/zones", s.zones)
	api("GET /api/access-status", s.accessStatus)

	api("GET /api/tunnels", s.listTunnels)
	api("POST /api/tunnels", s.createTunnel)
	api("PATCH /api/tunnels/{id}", s.patchTunnel)
	api("DELETE /api/tunnels/{id}", s.deleteTunnel)
	api("POST /api/tunnels/{id}/restart", s.restartTunnel)
	api("POST /api/tunnels/{id}/sync", s.syncTunnel)
	api("GET /api/tunnels/{id}/token", s.tunnelToken)
	api("GET /api/tunnels/{id}/metrics", s.tunnelMetrics)
	api("GET /api/tunnels/{id}/logs", s.tunnelLogs)

	api("GET /api/hosts", s.listHosts)
	api("POST /api/hosts", s.createHost)
	api("PUT /api/hosts/{id}", s.updateHost)
	api("PATCH /api/hosts/{id}", s.patchHost)
	api("DELETE /api/hosts/{id}", s.deleteHost)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	})
	if s.Static != nil {
		mux.Handle("/", s.spa())
	}
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// spa serves the built assets and falls back to index.html for client routes.
func (s *Server) spa() http.Handler {
	files := http.FileServerFS(s.Static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(s.Static, name); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			name = "index.html"
		}
		if name == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

// guard applies CSRF protection to every mutation and, when authed is set,
// requires a valid session. Browsers cannot add a custom header to a
// cross-site request without a CORS preflight, which this server never grants.
func (s *Server) guard(h http.HandlerFunc, authed bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) == "" {
			writeError(w, http.StatusForbidden, "csrf", "missing header "+csrfHeader)
			return
		}
		if c, err := r.Cookie(cookieName); err == nil {
			if u, err := s.Svc.Store.SessionUser(hashToken(c.Value)); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, u))
			}
		}
		if authed && userOf(r) == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "sign in to continue")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		if strings.HasSuffix(r.URL.Path, "/logs") {
			ctx, cancel = context.WithCancel(r.Context()) // log stream: no deadline
		}
		defer cancel()
		h(w, r.WithContext(ctx))
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

// fail maps a service error to an HTTP response.
func fail(w http.ResponseWriter, err error) {
	var ve *core.ValidationError
	var dc *core.DNSConflictError
	var ae *cf.APIError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, "invalid", ve.Msg)
	case errors.As(err, &dc):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": dc.Error(), "code": "dns_conflict", "hostname": dc.Hostname, "records": dc.Records,
		})
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, core.ErrNotConfigured):
		writeError(w, http.StatusConflict, "not_configured", err.Error())
	case store.IsUniqueViolation(err):
		writeError(w, http.StatusBadRequest, "invalid", "an item with these details already exists")
	case errors.As(err, &ae):
		writeError(w, http.StatusBadGateway, "cloudflare", err.Error())
	default:
		log.Printf("error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "invalid JSON")
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return 0, false
	}
	return id, true
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// ---- auth ----

type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*failRecord
}

type failRecord struct {
	count int
	until time.Time
}

// allow reports whether ip may try to log in now.
func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := l.fails[ip]
	return rec == nil || time.Now().After(rec.until)
}

// fail records a failed attempt; from the 5th on, the ip is locked out with an
// exponentially growing delay (capped at 15 minutes).
func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = map[string]*failRecord{}
	}
	if len(l.fails) > 10000 {
		clear(l.fails)
	}
	rec := l.fails[ip]
	if rec == nil {
		rec = &failRecord{}
		l.fails[ip] = rec
	}
	rec.count++
	if rec.count >= 5 {
		lock := 30 * time.Second << min(rec.count-5, 5)
		rec.until = time.Now().Add(min(lock, 15*time.Minute))
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func validCredentials(username, password string) error {
	if len(username) < 3 || len(username) > 64 || strings.ContainsAny(username, " \t\r\n") {
		return errors.New("the username must be 3 to 64 characters long, with no spaces")
	}
	if len(password) < 10 || len(password) > 72 {
		return errors.New("the password must be 10 to 72 characters long")
	}
	return nil
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := hex.EncodeToString(raw)
	if err := s.Svc.Store.CreateSession(hashToken(token), userID, sessionTTL); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure:  r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		Expires: time.Now().Add(sessionTTL),
	})
	return nil
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	n, err := s.Svc.Store.CountUsers()
	if err != nil {
		fail(w, err)
		return
	}
	out := map[string]any{"setupRequired": n == 0, "user": nil, "version": s.Version}
	if u := userOf(r); u != nil {
		out["user"] = u
		info, err := s.Svc.CloudflareInfo()
		if err != nil {
			fail(w, err)
			return
		}
		out["cloudflare"] = info
		ver, err := s.Svc.Runner.BinaryVersion()
		out["cloudflared"] = map[string]any{"available": err == nil, "version": ver}
	}
	writeJSON(w, http.StatusOK, out)
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !readJSON(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if err := validCredentials(in.Username, in.Password); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, err)
		return
	}
	created, err := s.Svc.Store.CreateFirstUser(in.Username, string(hash))
	if err != nil {
		fail(w, err)
		return
	}
	if !created {
		writeError(w, http.StatusConflict, "already_setup", "this installation is already set up")
		return
	}
	u, err := s.Svc.Store.UserByName(in.Username)
	if err == nil {
		err = s.startSession(w, r, u.ID)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// dummyHash keeps login timing the same whether or not the user exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("ctm-dummy-password"), bcrypt.DefaultCost)

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts; wait a moment and try again")
		return
	}
	var in credentials
	if !readJSON(w, r, &in) {
		return
	}
	u, err := s.Svc.Store.UserByName(strings.TrimSpace(in.Username))
	hash := dummyHash
	if err == nil {
		hash = []byte(u.PasswordHash)
	} else if !errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(in.Password)) != nil || u == nil {
		s.limiter.fail(ip)
		writeError(w, http.StatusUnauthorized, "bad_credentials", "incorrect username or password")
		return
	}
	s.limiter.reset(ip)
	if err := s.startSession(w, r, u.ID); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		_ = s.Svc.Store.DeleteSession(hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	u := userOf(r)
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Current)) != nil {
		writeError(w, http.StatusBadRequest, "invalid", "the current password is incorrect")
		return
	}
	if err := validCredentials(u.Username, in.New); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.New), bcrypt.DefaultCost)
	if err == nil {
		err = s.Svc.Store.SetPassword(u.ID, string(hash))
	}
	if err != nil {
		fail(w, err)
		return
	}
	// Log out every other device.
	keep := ""
	if c, err := r.Cookie(cookieName); err == nil {
		keep = hashToken(c.Value)
	}
	_ = s.Svc.Store.DeleteUserSessions(u.ID, keep)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- Cloudflare ----

func (s *Server) configureCloudflare(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token     string `json:"token"`
		AccountID string `json:"accountId"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	chosen, choices, err := s.Svc.ConfigureCloudflare(r.Context(), in.Token, in.AccountID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": chosen, "accounts": choices})
}

func (s *Server) checkCloudflare(w http.ResponseWriter, r *http.Request) {
	checks, err := s.Svc.CheckPermissions(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, checks)
}

func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	info, err := s.Updater.Check(r.Context(), r.URL.Query().Get("fresh") == "1")
	if err != nil {
		writeError(w, http.StatusBadGateway, "update", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// applyUpdate installs the latest release and then restarts the process; the
// managed tunnels reconnect once the new version is up.
func (s *Server) applyUpdate(w http.ResponseWriter, r *http.Request) {
	ver, err := s.Updater.Apply(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "update", err.Error())
		return
	}
	log.Printf("updated to %s by %s", ver, userOf(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{"version": ver})
	go func() {
		time.Sleep(500 * time.Millisecond) // let the response reach the browser
		s.Updater.Restart()
	}()
}

func (s *Server) zones(w http.ResponseWriter, r *http.Request) {
	zones, err := s.Svc.Zones(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, zones)
}

func (s *Server) accessStatus(w http.ResponseWriter, r *http.Request) {
	ok, msg := s.Svc.AccessAvailable(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"available": ok, "error": msg})
}

// ---- tunnels ----

type tunnelView struct {
	*store.Tunnel
	HostCount int                `json:"hostCount"`
	Runner    runner.Status      `json:"runner"`
	Remote    *core.RemoteStatus `json:"remote"`
}

func (s *Server) listTunnels(w http.ResponseWriter, r *http.Request) {
	tunnels, err := s.Svc.Store.Tunnels()
	if err != nil {
		fail(w, err)
		return
	}
	hosts, err := s.Svc.Store.Hosts()
	if err != nil {
		fail(w, err)
		return
	}
	counts := map[int64]int{}
	for _, h := range hosts {
		counts[h.TunnelID]++
	}
	views := make([]tunnelView, len(tunnels))
	var wg sync.WaitGroup
	for i, t := range tunnels {
		views[i] = tunnelView{Tunnel: t, HostCount: counts[t.ID], Runner: s.Svc.Runner.Status(t.ID)}
		wg.Add(1)
		go func() {
			defer wg.Done()
			views[i].Remote = s.Svc.RemoteStatus(r.Context(), t)
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) createTunnel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Mode string `json:"mode"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	t, err := s.Svc.CreateTunnel(r.Context(), in.Name, in.Mode)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) patchTunnel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Mode    string `json:"mode"`
		Enabled bool   `json:"enabled"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	t, err := s.Svc.SetTunnelRun(id, in.Mode, in.Enabled)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) deleteTunnel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Svc.DeleteTunnel(r.Context(), id, r.URL.Query().Get("force") == "1"); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) restartTunnel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Svc.RestartTunnel(id); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) syncTunnel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	res, err := s.Svc.SyncTunnel(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) tunnelToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	token, err := s.Svc.TunnelToken(id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token})
}

func (s *Server) tunnelMetrics(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	const codes = "cloudflared_tunnel_response_by_code"
	m, err := s.Svc.Runner.Metrics(r.Context(), id, map[string]string{codes: "status_code"})
	if err != nil || m == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	byCode := m.ByLabel[codes]
	if byCode == nil {
		byCode = map[string]float64{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available":          true,
		"totalRequests":      m.Totals["cloudflared_tunnel_total_requests"],
		"requestErrors":      m.Totals["cloudflared_tunnel_request_errors"],
		"concurrentRequests": m.Totals["cloudflared_tunnel_concurrent_requests_per_tunnel"],
		"haConnections":      m.Totals["cloudflared_tunnel_ha_connections"],
		"responseCodes":      byCode,
	})
}

// tunnelLogs streams the connector log as server-sent events: first the
// backlog, then new lines as cloudflared prints them.
func (s *Server) tunnelLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := s.Svc.Store.Tunnel(id); err != nil {
		fail(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming not available")
		return
	}
	buf := s.Svc.Runner.Logs(id)
	backlog, ch := buf.Subscribe()
	defer buf.Unsubscribe(ch)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	send := func(l runner.Line) {
		raw, _ := json.Marshal(l)
		fmt.Fprintf(w, "data: %s\n\n", raw)
	}
	for _, l := range backlog {
		send(l)
	}
	flusher.Flush()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case l := <-ch:
			send(l)
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		flusher.Flush()
	}
}

// ---- hosts ----

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.Svc.Store.Hosts()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hosts)
}

func (s *Server) createHost(w http.ResponseWriter, r *http.Request) {
	var in core.HostInput
	if !readJSON(w, r, &in) {
		return
	}
	h, err := s.Svc.CreateHost(r.Context(), in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) updateHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in core.HostInput
	if !readJSON(w, r, &in) {
		return
	}
	h, err := s.Svc.UpdateHost(r.Context(), id, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) patchHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	h, err := s.Svc.SetHostEnabled(r.Context(), id, in.Enabled)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Svc.DeleteHost(r.Context(), id, r.URL.Query().Get("force") == "1"); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
