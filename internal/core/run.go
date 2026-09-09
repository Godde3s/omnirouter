// run.go — OmniRouter boot: wire embedded bridges onto internal listeners,
// assemble the public mux, serve, and handle graceful shutdown.

package core

import (
        "context"
        "errors"
        "fmt"
        "log"
        "net"
        "net/http"
        "net/http/httputil"
        "net/url"
        "os"
        "os/signal"
        "strings"
        "syscall"
        "time"
)

// Router bundles the live state of the whole service.
type Router struct {
        cfg       *Config
        store     *Store
        registry  *Registry
        forwarder *forwarder
        internal  []*internalServer
        stop      chan struct{}
}

// internalServer hosts one embedded bridge on a loopback port.
type internalServer struct {
        id   string
        srv  *http.Server
        addr string
}

// Run is the entry point called from the root main().
func Run() {
        loadDotEnvCore()
        cfg := loadCoreConfig()

        store := NewStore(cfg.DataDir)
        seedKey, created := store.EnsureSeedKey(cfg.RouterKey)

        // ── Embedded bridges on internal loopback listeners ─────────────────
        var internal []*internalServer

        // GLM
        func() {
                zbInit()
                ln, err := net.Listen("tcp", "127.0.0.1:0")
                if err != nil {
                        log.Printf("[Router] GLM internal listener failed: %v", err)
                        return
                }
                srv := &http.Server{Handler: zbHandler()}
                go srv.Serve(ln)
                internal = append(internal, &internalServer{id: "glm", srv: srv, addr: "http://" + ln.Addr().String()})
        }()

        // Qwen
        func() {
                qbInit()
                ln, err := net.Listen("tcp", "127.0.0.1:0")
                if err != nil {
                        log.Printf("[Router] Qwen internal listener failed: %v", err)
                        return
                }
                srv := &http.Server{Handler: qbHandler()}
                go srv.Serve(ln)
                internal = append(internal, &internalServer{id: "qwen", srv: srv, addr: "http://" + ln.Addr().String()})
        }()

        // DeepSeek
        func() {
                dsInit()
                ln, err := net.Listen("tcp", "127.0.0.1:0")
                if err != nil {
                        log.Printf("[Router] DeepSeek internal listener failed: %v", err)
                        return
                }
                srv := &http.Server{Handler: dsHandler()}
                go srv.Serve(ln)
                internal = append(internal, &internalServer{id: "ds", srv: srv, addr: "http://" + ln.Addr().String()})
        }()

        // Gemini
        func() {
                gbInit()
                ln, err := net.Listen("tcp", "127.0.0.1:0")
                if err != nil {
                        log.Printf("[Router] Gemini internal listener failed: %v", err)
                        return
                }
                srv := &http.Server{Handler: gbHandler()}
                go srv.Serve(ln)
                internal = append(internal, &internalServer{id: "gemini", srv: srv, addr: "http://" + ln.Addr().String()})
        }()

        registry := NewRegistry(cfg.InternalToken, store.ListProviders())
        for _, is := range internal {
                registry.SetBridgeURL(is.id, is.addr)
        }
        // A bridge without credentials still boots (its handlers answer with
        // bilingual guidance); "auto" routing skips it when unhealthy —
        // health is probed live in RefreshModels, so nothing to force here.

        forwarder := newForwarder(registry, store, cfg.InternalToken, cfg.RetryPerProvider, cfg.CooldownSeconds, cfg.RequestTimeoutSec)

        rt := &Router{
                cfg:       cfg,
                store:     store,
                registry:  registry,
                forwarder: forwarder,
                internal:  internal,
                stop:      make(chan struct{}),
        }

        // Warm the catalog.
        registry.RefreshModels(context.Background())

        // ── Public mux ──────────────────────────────────────────────────────
        mux := http.NewServeMux()
        mux.HandleFunc("/", rt.dashboardHandler)
        mux.HandleFunc("/health", rt.healthHandler)

        rt.registerV1(mux)
        registerAdminRoutes(mux, store, registry, forwarder, cfg)

        // Bridge dashboards for debugging (/glm/, /qwen/, /ds/ → their surfaces).
        for _, is := range internal {
                is := is
                prefix := "/" + is.id + "/"
                target, _ := url.Parse(is.addr)
                proxy := httputil.NewSingleHostReverseProxy(target)
                proxy.FlushInterval = -1 // streaming
                mux.Handle(prefix, http.StripPrefix(strings.TrimSuffix(prefix, "/"), proxy))
        }

        handler := cors(mux)

        addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
        srv := &http.Server{Addr: addr, Handler: handler}

        modelCount := func() int {
                models, _ := registry.Catalog()
                return len(models)
        }

        fmt.Printf(`
╔════════════════════════════════════════════════════════════════════╗
║                        🌐  OmniRouter  v%s                            ║
║        یک روتر برای همه‌ی مدل‌ها — one router for every model        ║
╠════════════════════════════════════════════════════════════════════╣
║  Providers (embedded):  Qwen (chat.qwen.ai) · GLM (chat.z.ai) ·    ║
║                         DeepSeek · Gemini (gemini.google.com)      ║
║                         + هر API سازگار OpenAI (داشبورد)           ║
║  Models available:      %-44d║
║  Dashboard:             http://localhost:%d/  (رمز: ADMIN_PASSWORD)║
║  Health:                http://localhost:%d/health            ║
╠════════════════════════════════════════════════════════════════════╣
║  OpenAI API:      http://localhost:%d/v1/chat/completions     ║
║  Anthropic API:   http://localhost:%d/v1/messages             ║
║  Models:          http://localhost:%d/v1/models               ║
╠════════════════════════════════════════════════════════════════════╣
║  Client API Key:  %-49s║
║  %s║
╚════════════════════════════════════════════════════════════════════╝
`, routerVersion, modelCount(), cfg.Port, cfg.Port, cfg.Port, cfg.Port, cfg.Port,
                seedKey,
                keyNote(created))

        // Periodic catalog refresh + stats flush.
        stopRefresh := rt.stop
        go func() {
                t := time.NewTicker(60 * time.Second)
                defer t.Stop()
                for {
                        select {
                        case <-t.C:
                                registry.RefreshModels(context.Background())
                        case <-stopRefresh:
                                return
                        }
                }
        }()
        go func() {
                t := time.NewTicker(5 * time.Second)
                defer t.Stop()
                for {
                        select {
                        case <-t.C:
                                store.Flush(false)
                        case <-stopRefresh:
                                store.Flush(true)
                                return
                        }
                }
        }()

        serveErr := make(chan error, 1)
        go func() { serveErr <- srv.ListenAndServe() }()

        ctx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
        defer stopSignal()

        select {
        case err := <-serveErr:
                if err != nil && !errors.Is(err, http.ErrServerClosed) {
                        log.Fatal(err)
                }
        case <-ctx.Done():
                stopSignal()
                close(rt.stop)
                log.Println("[Router] Graceful shutdown — draining connections...")
                drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
                if err := srv.Shutdown(drainCtx); err != nil {
                        _ = srv.Close()
                }
                cancel()
                for _, is := range internal {
                        shutdownCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
                        _ = is.srv.Shutdown(shutdownCtx)
                        c()
                }
                log.Println("[Router] Goodbye.")
        }
}

func keyNote(created bool) string {
        if created {
                return "  (کلید تازه ساخته شد — در داشبورد مدیریتش کن / fresh key created — manage in dashboard)   "
        }
        return "  (از .env یا داشبورد — from ROUTER_KEY/dashboard)                          "
}

func (rt *Router) healthHandler(w http.ResponseWriter, r *http.Request) {
        provs := rt.registry.Providers()
        items := make([]map[string]interface{}, 0, len(provs))
        healthy := 0
        for _, p := range provs {
                items = append(items, map[string]interface{}{
                        "id": p.ID, "label": p.Label, "healthy": p.Healthy, "enabled": p.Enabled, "models": len(p.Models),
                })
                if p.Healthy {
                        healthy++
                }
        }
        writeJSON(w, 200, map[string]interface{}{
                "service": "omnirouter", "version": routerVersion, "status": "ok",
                "providers": items, "healthy_providers": healthy,
        })
}

func cors(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                w.Header().Set("Access-Control-Allow-Origin", "*")
                w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
                w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, x-omni-admin")
                if r.Method == "OPTIONS" {
                        w.WriteHeader(200)
                        return
                }
                next.ServeHTTP(w, r)
        })
}
