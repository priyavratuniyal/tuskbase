package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	httpapi "github.com/priyavratuniyal/tuskbase/internal/adapters/http"
	mcpadapter "github.com/priyavratuniyal/tuskbase/internal/adapters/mcp"
	"github.com/priyavratuniyal/tuskbase/internal/app"
	"github.com/priyavratuniyal/tuskbase/internal/ports"
)

// Config describes a daemon runtime, not a specific storage mode.
type Config struct {
	Addr            string
	MCPPath         string
	EnableMCP       bool
	EnableREST      bool
	Name            string
	Version         string
	Logger          *slog.Logger
	ReadTimeout     time.Duration
	StoreMiddleware func(app.Store) app.Store
}

// StoreBundle groups canonical storage with retrieval so a tier can swap SQLite for Postgres without changing the daemon core.
type StoreBundle struct {
	Store  app.Store
	Search ports.SearchIndex
	Close  func() error
	Name   string
}

// StoreFactory is the composition boundary for Local Basic and Local Shared storage.
type StoreFactory interface {
	Open(context.Context) (StoreBundle, error)
}

// AuthPolicy keeps transport auth replaceable while exposing a generic app principal to handlers.
type AuthPolicy interface {
	WrapHTTP(http.Handler) http.Handler
	Name() string
	Source() string
}

// NoAuthPolicy is acceptable only for direct stdio runtime use; HTTP daemon modes should use an authenticated principal.
type NoAuthPolicy struct{}

func (NoAuthPolicy) WrapHTTP(h http.Handler) http.Handler { return h }
func (NoAuthPolicy) Name() string                         { return "none" }
func (NoAuthPolicy) Source() string                       { return "none" }

// AuthPolicyLoader lets local auth refresh credentials without changing the daemon
// or application use cases.
type AuthPolicyLoader func(context.Context) (AuthPolicy, error)

type DynamicAuthPolicy struct {
	source string
	load   AuthPolicyLoader
}

func NewDynamicAuthPolicy(source string, load AuthPolicyLoader) (DynamicAuthPolicy, error) {
	if load == nil {
		return DynamicAuthPolicy{}, errors.New("dynamic auth loader is required")
	}
	if source == "" {
		source = "dynamic"
	}
	return DynamicAuthPolicy{source: source, load: load}, nil
}

func (p DynamicAuthPolicy) WrapHTTP(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policy, err := p.load(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		policy.WrapHTTP(h).ServeHTTP(w, r)
	})
}

func (p DynamicAuthPolicy) Name() string {
	policy, err := p.load(context.Background())
	if err != nil {
		return "unavailable"
	}
	return policy.Name()
}

func (p DynamicAuthPolicy) Source() string { return p.source }

type Health struct {
	Status     string `json:"status"`
	Store      string `json:"store,omitempty"`
	MCP        bool   `json:"mcp"`
	REST       bool   `json:"rest"`
	AuthPolicy string `json:"auth_policy"`
	AuthSource string `json:"auth_source,omitempty"`
}

type TuskbaseDaemon struct {
	cfg     Config
	bundle  StoreBundle
	service *app.Service
	mcp     *mcp.Server
	auth    AuthPolicy
	handler http.Handler
	closed  bool
}

func New(ctx context.Context, cfg Config, stores StoreFactory, auth AuthPolicy) (*TuskbaseDaemon, error) {
	if stores == nil {
		return nil, errors.New("store factory is required")
	}
	if auth == nil {
		auth = NoAuthPolicy{}
	}
	if cfg.MCPPath == "" {
		cfg.MCPPath = "/mcp"
	}
	if cfg.Name == "" {
		cfg.Name = "tuskbase"
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 5 * time.Second
	}
	bundle, err := stores.Open(ctx)
	if err != nil {
		return nil, err
	}
	if bundle.Search == nil {
		_ = bundle.Close()
		return nil, errors.New("search index is required")
	}
	if cfg.StoreMiddleware != nil {
		bundle.Store = cfg.StoreMiddleware(bundle.Store)
	}
	service := app.NewService(bundle.Store, bundle.Search, app.UUIDGenerator{}, app.SystemClock{})
	d := &TuskbaseDaemon{cfg: cfg, bundle: bundle, service: service, auth: auth}
	d.mcp = mcpadapter.NewServerWithVersion(service, cfg.Version)
	d.handler = d.buildHandler()
	return d, nil
}

func (d *TuskbaseDaemon) Service() *app.Service  { return d.service }
func (d *TuskbaseDaemon) MCPServer() *mcp.Server { return d.mcp }
func (d *TuskbaseDaemon) Handler() http.Handler  { return d.handler }

func (d *TuskbaseDaemon) Health() Health {
	return Health{Status: "ok", Store: d.bundle.Name, MCP: d.cfg.EnableMCP, REST: d.cfg.EnableREST, AuthPolicy: d.auth.Name(), AuthSource: d.auth.Source()}
}

func (d *TuskbaseDaemon) RunStdio(ctx context.Context) error {
	return d.mcp.Run(ctx, &mcp.StdioTransport{})
}

func (d *TuskbaseDaemon) RunHTTP(ctx context.Context) error {
	server := &http.Server{Addr: d.cfg.Addr, Handler: d.handler, ReadHeaderTimeout: d.cfg.ReadTimeout}
	errc := make(chan error, 1)
	go func() {
		if d.cfg.Logger != nil {
			d.cfg.Logger.Info("starting tuskbase daemon", "addr", d.cfg.Addr, "mcp", d.cfg.EnableMCP, "rest", d.cfg.EnableREST)
		}
		errc <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (d *TuskbaseDaemon) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	if d.bundle.Close != nil {
		return d.bundle.Close()
	}
	return nil
}

// buildHandler mounts MCP and REST independently so users can run an MCP-only daemon without exposing REST endpoints.
func (d *TuskbaseDaemon) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(d.Health())
	})
	if d.cfg.EnableMCP {
		mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return d.mcp }, nil)
		mux.Handle(d.cfg.MCPPath, d.auth.WrapHTTP(mcpHandler))
	}
	mux.Handle("/control/", d.auth.WrapHTTP(httpapi.NewControlServer(d.service)))
	if d.cfg.EnableREST {
		mux.Handle("/", d.auth.WrapHTTP(httpapi.NewServer(d.service)))
	}
	return mux
}
