// Command drydock is the Drydock server: an agentic kanban task platform.
// Data sources (webhooks, RSS, chat, …) create cards that auto-start; each
// card gets a Docker container and a coding-agent runtime that works the task
// to completion, pausing with an alarm badge when it needs the user. Finished
// cards pass through a separate verifier agent before closing.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dsbaha/drydock/internal/agents"
	"github.com/dsbaha/drydock/internal/auth"
	"github.com/dsbaha/drydock/internal/config"
	"github.com/dsbaha/drydock/internal/db"
	"github.com/dsbaha/drydock/internal/dockerx"
	"github.com/dsbaha/drydock/internal/flows"
	"github.com/dsbaha/drydock/internal/ingest"
	"github.com/dsbaha/drydock/internal/server"
	"github.com/dsbaha/drydock/web"
)

func main() {
	cfg := config.Load()

	database, err := db.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()

	dockerClient, err := dockerx.New()
	if err != nil {
		log.Printf("docker client init failed (%v); cards will run without containers", err)
		dockerClient = nil
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := dockerClient.Ping(ctx); err != nil {
			if uerr := dockerx.CheckUnixSocket(dockerx.ResolveHost()); uerr != nil {
				log.Printf("container runtime not reachable: %v; cards will run without containers", uerr)
			} else {
				log.Printf("docker daemon not reachable (%v); cards will run without containers", err)
			}
			dockerClient = nil
		}
		cancel()
	}

	authH := &auth.Handler{DB: database, Config: cfg}

	// The server owns the websocket hub; the orchestrator notifies through it.
	srv := &server.Server{DB: database, Auth: authH, Hub: server.NewHub()}
	orch := agents.New(database, dockerClient, cfg.DefaultImage, cfg.AgentRuntime, srv)
	srv.Orch = orch
	// The flow engine owns card creation: inbound items (and the manual
	// "New card" button) enter the board's flow, whose worker node creates
	// the card via the orchestrator. The default flow reproduces the
	// previous hardcoded pipeline exactly.
	flowEngine := flows.New(database, orch.IngestCardWithOptions, nil /* classifier wired below */)
	srv.Flows = flowEngine
	srv.Ing = ingest.New(database, func(boardID, kind, title, body, source string) (*flows.Result, error) {
		return flowEngine.TriggerInbox(boardID, kind, title, body, source)
	})
	// LLM intake filtering for RSS/webhook sources with filtering enabled.
	// Fail-open: sources without a filter, or filter failures, still ingest.
	srv.Ing.Filter = &ingest.Filter{Classify: &ingest.LLMClassifier{DB: database}}
	// The same classifier backs flow-level llm_filter nodes.
	flowEngine.SetClassifier(srv.Ing.Filter)

	// Seed the default board so ingestion and manual cards work immediately.
	if _, err := database.EnsureDefaultBoard(); err != nil {
		log.Printf("seed default board: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Boot: restart workers for cards that were mid-flight, then start the
	// RSS poll loop.
	go func() {
		orch.Reconcile(ctx)
		srv.Ing.PollLoop(ctx)
	}()

	mux := srv.Routes()

	// Frontend.
	mux.Handle("/", web.Handler())

	// Basic request logging.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		mux.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("drydock listening on %s (data: %s, runtime: %s, registration open: %v)",
			cfg.Addr, cfg.DataDir, cfg.AgentRuntime, cfg.RegOpen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}
