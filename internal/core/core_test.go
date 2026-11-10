package core

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"tunnelmanager/internal/cf"
	"tunnelmanager/internal/cftest"
	"tunnelmanager/internal/runner"
	"tunnelmanager/internal/secret"
	"tunnelmanager/internal/store"
)

func setup(t *testing.T) (*Service, *cftest.Fake) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir + "/with space.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secret.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}
	fake := cftest.New()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	svc := New(st, box, runner.NewManager("/nonexistent/cloudflared"))
	svc.NewClient = func(token string) *cf.Client { return cf.NewWithBase(token, srv.URL) }
	if acc, _, err := svc.ConfigureCloudflare(context.Background(), "tok", ""); err != nil || acc == nil {
		t.Fatalf("configure: %v %v", acc, err)
	}
	return svc, fake
}

func hostnames(rules []cf.IngressRule) string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Hostname+r.Path+"→"+r.Service)
	}
	return strings.Join(out, " | ")
}

func TestHostLifecycle(t *testing.T) {
	ctx := context.Background()
	svc, fake := setup(t)
	tun, err := svc.CreateTunnel(ctx, "casa", store.ModeExternal)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostnames(fake.Ingress[tun.CFID]); got != "→http_status:404" {
		t.Fatalf("initial ingress: %s", got)
	}

	// Migration case: an A record already points at the home IP.
	fake.DNS["old"] = cf.DNSRecord{ID: "old", Type: "A", Name: "app.example.com", Content: "1.2.3.4"}
	fake.DNS["txt"] = cf.DNSRecord{ID: "txt", Type: "TXT", Name: "app.example.com", Content: "keep"}
	in := HostInput{TunnelID: tun.ID, Hostname: "App.Example.com", Service: "http://192.168.1.10:8080/", Enabled: true,
		Options: store.OriginOptions{NoTLSVerify: true, ConnectTimeout: 15}}
	_, err = svc.CreateHost(ctx, in)
	var conflict *DNSConflictError
	if !errors.As(err, &conflict) || len(conflict.Records) != 1 {
		t.Fatalf("want DNS conflict with 1 record, got %v", err)
	}
	if hosts, _ := svc.Store.Hosts(); len(hosts) != 0 {
		t.Fatal("host stored despite conflict")
	}
	in.OverwriteDNS = true
	h, err := svc.CreateHost(ctx, in)
	if err != nil || h.SyncError != "" {
		t.Fatalf("create: %v / %s", err, h.SyncError)
	}
	if _, ok := fake.DNS["old"]; ok {
		t.Fatal("old A record not replaced")
	}
	if _, ok := fake.DNS["txt"]; !ok {
		t.Fatal("TXT record must be left alone")
	}
	rec := fake.DNS[h.DNSRecordID]
	if rec.Type != "CNAME" || rec.Content != tun.CFID+".cfargotunnel.com" || !rec.Proxied || h.ZoneID != "z1" {
		t.Fatalf("bad record %+v zone %s", rec, h.ZoneID)
	}
	rules := fake.Ingress[tun.CFID]
	if hostnames(rules) != "app.example.com→http://192.168.1.10:8080 | →http_status:404" {
		t.Fatalf("ingress: %s", hostnames(rules))
	}
	if rules[0].OriginRequest["noTLSVerify"] != true || rules[0].OriginRequest["connectTimeout"] != float64(15) {
		t.Fatalf("origin: %v", rules[0].OriginRequest)
	}

	// Path rule + wildcard: ordering and shared DNS record.
	hp, err := svc.CreateHost(ctx, HostInput{TunnelID: tun.ID, Hostname: "app.example.com", Path: "^/api", Service: "http://10.0.0.2:3000", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateHost(ctx, HostInput{TunnelID: tun.ID, Hostname: "*.example.com", Service: "https://10.0.0.3", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	want := "app.example.com^/api→http://10.0.0.2:3000 | app.example.com→http://192.168.1.10:8080 | *.example.com→https://10.0.0.3 | →http_status:404"
	if got := hostnames(fake.Ingress[tun.CFID]); got != want {
		t.Fatalf("ingress order:\n got %s\nwant %s", got, want)
	}
	if hp.DNSRecordID != h.DNSRecordID {
		t.Fatal("path host should share the DNS record")
	}
	if _, err := svc.CreateHost(ctx, HostInput{TunnelID: tun.ID, Hostname: "app.example.com", Path: "^/api", Service: "http://x:1", Enabled: true}); err == nil {
		t.Fatal("duplicate host accepted")
	}

	// Most specific zone wins.
	hl, err := svc.CreateHost(ctx, HostInput{TunnelID: tun.ID, Hostname: "nas.lab.example.com", Service: "ssh://10.0.0.9:22", Enabled: true})
	if err != nil || hl.ZoneID != "z2" {
		t.Fatalf("zone: %v %+v", err, hl)
	}

	// Access: when the app cannot be created the host must stay unpublished.
	fake.FailApps = true
	up := in
	up.Access = store.AccessConfig{Enabled: true, Emails: []string{"Me@Example.com"}, EmailDomains: []string{"corp.com"}}
	h, err = svc.UpdateHost(ctx, h.ID, up)
	if err != nil || h.SyncError == "" || h.AccessAppID != "" {
		t.Fatalf("want sync error: %v %+v", err, h)
	}
	if strings.Contains(hostnames(fake.Ingress[tun.CFID]), "app.example.com→") {
		t.Fatal("host published without its Access app")
	}
	fake.FailApps = false
	res, err := svc.SyncTunnel(ctx, tun.ID)
	if err != nil || len(res.Errors) != 0 {
		t.Fatalf("sync: %v %+v", err, res)
	}
	h, _ = svc.Store.Host(h.ID)
	if h.AccessAppID == "" || h.AccessPolicyID == "" || h.SyncError != "" {
		t.Fatalf("access not applied: %+v", h)
	}
	pol := fake.Policies[h.AccessPolicyID]
	if pol["app"] != h.AccessAppID || len(pol["include"].([]any)) != 2 {
		t.Fatalf("policy: %v", pol)
	}
	if !strings.Contains(hostnames(fake.Ingress[tun.CFID]), "app.example.com→") {
		t.Fatal("host not published after sync")
	}

	// Rename: Access app follows, the old DNS stays because the path host uses it.
	up.Hostname = "new.example.com"
	appID := h.AccessAppID
	h, err = svc.UpdateHost(ctx, h.ID, up)
	if err != nil || h.SyncError != "" || h.AccessAppID != appID {
		t.Fatalf("rename: %v %+v", err, h)
	}
	if fake.Apps[appID]["domain"] != "new.example.com" {
		t.Fatalf("app domain: %v", fake.Apps[appID])
	}
	if _, ok := fake.DNS[hp.DNSRecordID]; !ok {
		t.Fatal("DNS still needed by the path host was deleted")
	}

	// Disable takes it out of the ingress; delete cleans everything.
	if _, err := svc.SetHostEnabled(ctx, h.ID, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hostnames(fake.Ingress[tun.CFID]), "new.example.com") {
		t.Fatal("disabled host still published")
	}
	if err := svc.DeleteHost(ctx, h.ID, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.Apps) != 0 || fake.DNS[h.DNSRecordID].ID != "" {
		t.Fatalf("leftovers: apps=%v dns=%v", fake.Apps, fake.DNS)
	}

	// Deleting the tunnel removes every record we created, and only those.
	if err := svc.DeleteTunnel(ctx, tun.ID, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.Tunnels) != 0 || len(fake.DNS) != 1 || fake.DNS["txt"].ID == "" {
		t.Fatalf("after tunnel delete: tunnels=%v dns=%v", fake.Tunnels, fake.DNS)
	}
	if hosts, _ := svc.Store.Hosts(); len(hosts) != 0 {
		t.Fatal("hosts left behind")
	}
}

func TestNormalizeService(t *testing.T) {
	good := map[string]string{
		"http://localhost:8080/": "http://localhost:8080", "https://10.0.0.1": "https://10.0.0.1",
		"ssh://nas:22": "ssh://nas:22", "http_status:503": "http_status:503", "unix:/run/app.sock": "unix:/run/app.sock",
		"http://[::1]:80": "http://[::1]:80",
	}
	for in, want := range good {
		if got, err := NormalizeService(in); err != nil || got != want {
			t.Errorf("%s → %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "localhost:8080", "ftp://x", "http://", "http://x/path", "ssh://nas", "http://x:99999", "http://u:p@x"} {
		if _, err := NormalizeService(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}
