package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v0.2.0", "v0.1.9", true}, {"v0.1.10", "v0.1.9", true}, {"v1.0.0", "v0.9.9", true},
		{"v0.1.0", "v0.1.0", false}, {"v0.1.0", "v0.2.0", false}, {"v0.2.0", "dev", false}, {"nightly", "v0.1.0", false},
	}
	for _, c := range cases {
		if got := newer(c.latest, c.current); got != c.want {
			t.Errorf("newer(%s, %s) = %v", c.latest, c.current, got)
		}
	}
}

// fakeGitHub serves one release whose "binary" is a shell script.
func fakeGitHub(t *testing.T, tag, binary, checksumOf string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/latest":
			json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "html_url": srv.URL + "/release", "assets": []map[string]string{
				{"name": assetName(), "browser_download_url": srv.URL + "/bin"},
				{"name": "checksums.txt", "browser_download_url": srv.URL + "/sums"},
			}})
		case "/bin":
			w.Write([]byte(binary))
		case "/sums":
			sum := sha256.Sum256([]byte(checksumOf))
			w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + assetName() + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestApply(t *testing.T) {
	ctx := context.Background()
	exe := filepath.Join(t.TempDir(), "ctm")
	os.WriteFile(exe, []byte("old"), 0o755)
	script := "#!/bin/sh\necho v0.2.0\n"

	srv := fakeGitHub(t, "v0.2.0", script, script)
	u := &Updater{Repo: "o/r", Current: "v0.1.0", APIBase: srv.URL, ExePath: exe}

	info, err := u.Check(ctx, true)
	if err != nil || !info.UpdateAvailable || info.CanSelfUpdate || info.Why == "" {
		t.Fatalf("disabled updater: %+v %v", info, err)
	}
	if _, err := u.Apply(ctx); err == nil {
		t.Fatal("apply must be refused when in-place updates are disabled")
	}

	u.Enabled = true
	ver, err := u.Apply(ctx)
	if err != nil || ver != "v0.2.0" {
		t.Fatalf("apply: %q %v", ver, err)
	}
	if got, _ := os.ReadFile(exe); string(got) != script {
		t.Fatalf("binary not replaced: %q", got)
	}
	if st, _ := os.Stat(exe); st.Mode().Perm()&0o100 == 0 {
		t.Fatal("new binary is not executable")
	}

	u.Current = "v0.2.0"
	if _, err := u.Apply(ctx); err == nil || !strings.Contains(err.Error(), "up to date") {
		t.Fatalf("want up-to-date error, got %v", err)
	}

	// A tampered download must leave the binary alone.
	bad := fakeGitHub(t, "v0.3.0", "#!/bin/sh\necho v0.3.0 evil\n", "something else")
	u = &Updater{Repo: "o/r", Current: "v0.2.0", APIBase: bad.URL, ExePath: exe, Enabled: true}
	if _, err := u.Apply(ctx); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
	// So must one that reports a different version than the release.
	wrong := "#!/bin/sh\necho v9.9.9\n"
	mismatch := fakeGitHub(t, "v0.3.0", wrong, wrong)
	u.APIBase = mismatch.URL
	if _, err := u.Apply(ctx); err == nil || !strings.Contains(err.Error(), "self-check") {
		t.Fatalf("want self-check error, got %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != script {
		t.Fatal("binary changed by a rejected update")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".ctm-update-*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}
