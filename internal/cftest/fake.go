// Package cftest is an in-memory stand-in for the Cloudflare API endpoints the
// manager uses. It backs the tests and the cmd/fakecf development server.
package cftest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"tunnelmanager/internal/cf"
)

// Fake holds the simulated account state; fields may be inspected by tests.
type Fake struct {
	mu       sync.Mutex
	seq      int
	Tunnels  map[string]bool
	Ingress  map[string][]cf.IngressRule
	DNS      map[string]cf.DNSRecord
	Apps     map[string]map[string]any
	Policies map[string]map[string]any // policy id -> body (with "app")
	FailApps bool
}

func New() *Fake {
	return &Fake{Tunnels: map[string]bool{}, Ingress: map[string][]cf.IngressRule{}, DNS: map[string]cf.DNSRecord{},
		Apps: map[string]map[string]any{}, Policies: map[string]map[string]any{}}
}

func (f *Fake) id(prefix string) string { f.seq++; return fmt.Sprintf("%s%d", prefix, f.seq) }

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(v any) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": v})
	}
	bad := func(status int, msg string) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 1000, "message": msg}}})
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	key := r.Method + " " + r.URL.Path
	switch {
	case key == "GET /zones":
		acc := map[string]any{"id": "acc", "name": "My account"}
		ok([]any{map[string]any{"id": "z1", "name": "example.com", "account": acc},
			map[string]any{"id": "z2", "name": "lab.example.com", "account": acc}})
	case key == "GET /accounts/acc/cfd_tunnel":
		ok([]any{})
	case key == "POST /accounts/acc/cfd_tunnel":
		id := f.id("tun")
		f.Tunnels[id] = true
		ok(map[string]any{"id": id, "name": body["name"], "status": "inactive"})
	case len(p) == 5 && p[2] == "cfd_tunnel" && p[4] == "token":
		ok("token-" + p[3])
	case len(p) == 5 && p[2] == "cfd_tunnel" && p[4] == "configurations" && r.Method == "PUT":
		raw, _ := json.Marshal(body["config"].(map[string]any)["ingress"])
		var rules []cf.IngressRule
		json.Unmarshal(raw, &rules)
		f.Ingress[p[3]] = rules
		ok(nil)
	case len(p) == 5 && p[2] == "cfd_tunnel" && p[4] == "connections" && r.Method == "GET":
		ok([]any{map[string]any{"id": "conn1", "version": "2026.10.7", "arch": "linux_amd64", "conns": []any{
			map[string]any{"colo_name": "MAD", "origin_ip": "203.0.113.7"}, map[string]any{"colo_name": "BCN", "origin_ip": "203.0.113.7"}}}})
	case len(p) == 5 && p[2] == "cfd_tunnel" && p[4] == "connections":
		ok(nil)
	case len(p) == 4 && p[2] == "cfd_tunnel" && r.Method == "GET":
		ok(map[string]any{"id": p[3], "name": p[3], "status": "healthy"})
	case key == "GET /accounts/acc/access/apps":
		ok([]any{})
	case len(p) == 4 && p[2] == "cfd_tunnel" && r.Method == "DELETE":
		delete(f.Tunnels, p[3])
		ok(nil)
	case len(p) == 3 && p[0] == "zones" && p[2] == "dns_records" && r.Method == "GET":
		out := []cf.DNSRecord{}
		for _, rec := range f.DNS {
			if rec.Name == r.URL.Query().Get("name") {
				out = append(out, rec)
			}
		}
		ok(out)
	case len(p) == 3 && p[0] == "zones" && p[2] == "dns_records" && r.Method == "POST":
		raw, _ := json.Marshal(body)
		var rec cf.DNSRecord
		json.Unmarshal(raw, &rec)
		rec.ID = f.id("dns")
		f.DNS[rec.ID] = rec
		ok(rec)
	case len(p) == 4 && p[0] == "zones" && p[2] == "dns_records" && r.Method == "PUT":
		raw, _ := json.Marshal(body)
		var rec cf.DNSRecord
		json.Unmarshal(raw, &rec)
		rec.ID = p[3]
		f.DNS[rec.ID] = rec
		ok(rec)
	case len(p) == 4 && p[0] == "zones" && p[2] == "dns_records" && r.Method == "DELETE":
		delete(f.DNS, p[3])
		ok(map[string]any{"id": p[3]})
	case key == "POST /accounts/acc/access/apps":
		if f.FailApps {
			bad(403, "access not enabled")
			return
		}
		id := f.id("app")
		f.Apps[id] = body
		ok(map[string]any{"id": id})
	case len(p) == 5 && p[2] == "access" && p[3] == "apps" && r.Method == "PUT":
		if f.Apps[p[4]] == nil {
			bad(404, "not found")
			return
		}
		f.Apps[p[4]] = body
		ok(map[string]any{"id": p[4]})
	case len(p) == 5 && p[2] == "access" && p[3] == "apps" && r.Method == "DELETE":
		delete(f.Apps, p[4])
		ok(map[string]any{"id": p[4]})
	case len(p) == 6 && p[5] == "policies" && r.Method == "POST":
		id := f.id("pol")
		body["app"] = p[4]
		f.Policies[id] = body
		ok(map[string]any{"id": id})
	case len(p) == 7 && p[5] == "policies" && r.Method == "PUT":
		if f.Policies[p[6]] == nil {
			bad(404, "not found")
			return
		}
		body["app"] = p[4]
		f.Policies[p[6]] = body
		ok(map[string]any{"id": p[6]})
	default:
		bad(404, "fake: unhandled "+key)
	}
}
