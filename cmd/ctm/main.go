// Command ctm is the Cloudflare Tunnel Manager server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tunnelmanager/internal/cf"
	"tunnelmanager/internal/core"
	"tunnelmanager/internal/httpapi"
	"tunnelmanager/internal/runner"
	"tunnelmanager/internal/secret"
	"tunnelmanager/internal/store"
	"tunnelmanager/internal/updater"
	"tunnelmanager/internal/webui"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("tunnel-manager", version)
		return
	}
	listen := env("CTM_LISTEN", ":8088")
	dataDir := env("CTM_DATA_DIR", "./data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	box, err := secret.Load(os.Getenv("CTM_SECRET_KEY"), dataDir)
	if err != nil {
		log.Fatalf("secret key: %v", err)
	}
	st, err := store.Open(filepath.Join(dataDir, "ctm.db"))
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer st.Close()

	rn := runner.NewManager(env("CTM_CLOUDFLARED_BIN", "cloudflared"))
	svc := core.New(st, box, rn)
	if base := os.Getenv("CTM_CF_API_BASE"); base != "" { // development: see cmd/fakecf
		log.Printf("warning: using alternative Cloudflare API %s", base)
		svc.NewClient = func(token string) *cf.Client { return cf.NewWithBase(token, base) }
	}
	if ver, err := rn.BinaryVersion(); err != nil {
		log.Printf("warning: cloudflared not available (%v); managed tunnels will not start", err)
	} else {
		log.Printf("using %s", ver)
	}
	if err := svc.StartAll(); err != nil {
		log.Printf("warning: %v", err)
	}

	static := webui.FS()
	if static == nil {
		log.Printf("warning: web UI not built; serving the API only")
	}
	// In-place updates are opt-in: the systemd unit sets CTM_SELF_UPDATE and
	// restarts the process when it exits; container images are replaced instead.
	restart := make(chan struct{}, 1)
	upd := &updater.Updater{
		Repo:    env("CTM_UPDATE_REPO", "AitorConS/cloudflare-tunnel-manager"),
		Current: version,
		Enabled: os.Getenv("CTM_SELF_UPDATE") == "1",
		Restart: func() {
			select {
			case restart <- struct{}{}:
			default:
			}
		},
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           (&httpapi.Server{Svc: svc, Static: static, Version: version, Updater: upd}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("Cloudflare Tunnel Manager %s listening on %s", version, listen)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()
	select {
	case <-ctx.Done():
		log.Printf("shutting down…")
	case <-restart:
		log.Printf("update installed: exiting so the service manager starts the new version")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	rn.StopAll()
}
