// Command hosutod is the hosuto daemon: a game-server hoster for the holistic services landscape.
//
// It runs unprivileged on a loopback port behind Caddy, validates the same session cookie as every
// other holistic service, and escalates only through the narrow allow-listed wrapper
// /usr/local/sbin/hosuto-server.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"hosuto/internal/api"
	"hosuto/internal/auth"
	"hosuto/internal/contax"
	"hosuto/internal/directory"
	"hosuto/internal/hconfig"
	"hosuto/internal/mcapi"
	"hosuto/internal/mcp"
	"hosuto/internal/modrinth"
	"hosuto/internal/notify"
	"hosuto/internal/router"
	"hosuto/internal/runtime"
	"hosuto/internal/skin"
	"hosuto/internal/store"
	"hosuto/internal/versions"
)

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// readSecret takes a secret from an env var, else from the file named by its *_FILE partner. This is
// the landscape's standard shape: the unit points at a root-provisioned file in /etc/holistic which
// the service user may read by virtue of being in the `holistic` group.
func readSecret(env, fileEnv string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	if path := strings.TrimSpace(os.Getenv(fileEnv)); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8779", "address to listen on")
	statePath := flag.String("state", store.DefaultPath, "path to the state file")
	flag.Parse()

	secret, err := auth.LoadSecret()
	if err != nil {
		log.Fatalf("hosutod: %v", err)
	}
	v := auth.NewVerifier(secret, os.Getenv("HOSUTO_ADMIN_GROUP"))

	st, err := store.Open(*statePath)
	if err != nil {
		log.Fatalf("hosutod: state: %v", err)
	}

	// MCP bearer tokens live beside the state file (same 0600, same owner). They let external MCP
	// clients — and the Ask AI chat via aigentic — authenticate to hosuto's tool endpoint.
	tok, err := mcp.OpenTokenStore(filepath.Join(filepath.Dir(*statePath), "mcp-tokens.json"))
	if err != nil {
		log.Fatalf("hosutod: mcp tokens: %v", err)
	}

	// Admin configuration comes from the central Configuration tab, not from hosuto's own UI: the
	// manifest in /etc/holistic/config.d declares the knobs, the dashboard writes the values, and this
	// reader picks them up live — no restart, no RPC.
	cfg := hconfig.New("hosuto", "", "")

	// Every sibling service is reached through a thin client that DEGRADES SILENTLY when it is not
	// configured. A host without notify, contax or mc-router must still boot hosuto and serve its UI;
	// the features that depend on them simply stay inert rather than taking the daemon down.
	nt := notify.New(getenv("HOSUTO_NOTIFY_URL", "http://127.0.0.1:8778"),
		readSecret("HOSUTO_NOTIFY_SECRET", "HOSUTO_NOTIFY_SECRET_FILE"))
	cx := contax.New(getenv("HOSUTO_CONTAX_URL", "http://127.0.0.1:8777"),
		readSecret("HOSUTO_CONTAX_SECRET", "HOSUTO_CONTAX_SECRET_FILE"))
	rt := router.New(cfg.String("routerApi", "http://127.0.0.1:25580"), nil)

	dir := directory.New("")
	mc := mcapi.New("", nil)
	mr := modrinth.New("", cfg.String("modrinthUserAgent", "sxty9/hosuto (holistic)"), nil)
	vc := versions.New(nil)
	sk := skin.New("", nil)
	mgr := runtime.New(st, cfg, rt, vc)

	// Reconcile mc-router's live route table with the servers hosuto actually has. A crash between
	// "delete the server" and "delete the route" would otherwise leave a public domain pointing at a
	// port that is about to be handed to someone else's server — the one failure mode here that is
	// worse than an outage.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := mgr.SyncRoutes(ctx); err != nil {
		log.Printf("hosutod: route sync: %v (continuing)", err)
	}
	cancel()

	// The `defaultServer` setting is edited in the dashboard's Configuration tab, and hosuto reads its
	// config live — but mc-router's fallback is state we PUSH, not state it reads. Without this ticker
	// an admin's change would sit there doing nothing until the next daemon restart, which is exactly
	// the kind of "I changed it and nothing happened" that makes a config surface untrustworthy.
	// A loopback POST every half minute costs nothing.
	go func() {
		for range time.Tick(30 * time.Second) {
			c, done := context.WithTimeout(context.Background(), 5*time.Second)
			if err := mgr.SyncDefault(c); err != nil {
				log.Printf("hosutod: default route: %v", err)
			}
			done()
		}
	}()

	srv := &http.Server{
		Handler:           api.New(v, st, cfg, mgr, dir, cx, nt, mc, mr, vc, sk, tok).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Bind synchronously so "address already in use" surfaces here, not inside a goroutine.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("hosutod: listen %s: %v", *listen, err)
	}
	go func() {
		log.Printf("hosutod listening on %s", *listen)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("hosutod: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdown)
	log.Print("hosutod stopped")
}
