package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ClientCredential struct {
	Token  string
	Source string
}

// CredentialProvider is the client-side auth boundary for local daemon modes.
type CredentialProvider interface {
	Credential(context.Context, string) (ClientCredential, error)
}

type ConfigCredentialProvider struct {
	load func() (userConfig, bool, error)
}

func NewConfigCredentialProvider(load func() (userConfig, bool, error)) ConfigCredentialProvider {
	return ConfigCredentialProvider{load: load}
}

func (p ConfigCredentialProvider) Credential(ctx context.Context, client string) (ClientCredential, error) {
	if p.load == nil {
		return ClientCredential{}, errors.New("credential config loader is required")
	}
	cfg, found, err := p.load()
	if err != nil {
		return ClientCredential{}, err
	}
	if !found {
		return ClientCredential{}, errors.New("no Tuskbase setup found; run `tuskbase setup` first")
	}
	profile, err := AuthProfileForConfig(cfg)
	if err != nil {
		return ClientCredential{}, err
	}
	return profile.Credential(ctx, client)
}

type AuthProfile interface {
	Credential(context.Context, string) (ClientCredential, error)
}

type LocalBasicProfile struct {
	key string
}

func (p LocalBasicProfile) Credential(context.Context, string) (ClientCredential, error) {
	if strings.TrimSpace(p.key) == "" {
		return ClientCredential{}, errors.New("Local Basic key is missing; run `tuskbase setup --mode local-basic`")
	}
	return ClientCredential{Token: strings.TrimSpace(p.key), Source: "config:local-basic"}, nil
}

type LocalSharedProfile struct {
	keys []localSharedCredential
}

type localSharedCredential struct {
	Name string
	Key  string
}

func (p LocalSharedProfile) Credential(ctx context.Context, client string) (ClientCredential, error) {
	client, err := validateKeyName(client)
	if err != nil {
		return ClientCredential{}, err
	}
	for _, key := range p.keys {
		if strings.EqualFold(key.Name, client) {
			return ClientCredential{Token: strings.TrimSpace(key.Key), Source: "config:local-shared:" + key.Name}, nil
		}
	}
	return ClientCredential{}, fmt.Errorf("Local Shared key %q is missing; run `tuskbase auth add --name %s --role agent`", client, client)
}

func AuthProfileForConfig(cfg userConfig) (AuthProfile, error) {
	switch cfg.Mode {
	case modeLocalBasic, "":
		return LocalBasicProfile{key: cfg.APIKey}, nil
	case modeLocalShared:
		keys := make([]localSharedCredential, 0, len(cfg.AgentKeys))
		for _, key := range cfg.AgentKeys {
			keys = append(keys, localSharedCredential{Name: key.Name, Key: key.Key})
		}
		return LocalSharedProfile{keys: keys}, nil
	default:
		return nil, fmt.Errorf("unsupported auth profile %q", cfg.Mode)
	}
}

func runBridge(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	client := fs.String("client", "generic", "client key name to use for Local Shared attribution")
	addr := fs.String("addr", configuredAddr(), "daemon address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	clientName, err := validateKeyName(*client)
	if err != nil {
		return err
	}
	credentials := NewConfigCredentialProvider(loadUserConfig)
	if _, err := credentials.Credential(ctx, clientName); err != nil {
		return err
	}
	cfg, found, err := loadUserConfig()
	if err != nil {
		return err
	}
	if !found {
		return errors.New("no Tuskbase setup found; run `tuskbase setup` first")
	}
	cfg.Addr = *addr
	cfg = normalizedDaemonConfig(cfg)
	if err := newLifecycleController().EnsureReady(ctx, cfg); err != nil {
		return newBridgeDiagnosticServer(ctx, cfg, err).Run(ctx, &mcp.StdioTransport{})
	}
	server, closeRemote, err := newBridgeServer(ctx, "http://"+*addr+"/mcp", credentials, clientName)
	if err != nil {
		return newBridgeDiagnosticServer(ctx, cfg, err).Run(ctx, &mcp.StdioTransport{})
	}
	defer closeRemote()
	return server.Run(ctx, &mcp.StdioTransport{})
}

type bridgeDiagnosticInput struct{}

type bridgeDiagnosticOutput struct {
	Status          string `json:"status"`
	Mode            string `json:"mode"`
	Store           string `json:"store,omitempty"`
	Detail          string `json:"detail"`
	PostgresConnect string `json:"postgres_connect,omitempty"`
	PostgresError   string `json:"postgres_error,omitempty"`
	RepairHint      string `json:"repair_hint,omitempty"`
	FallbackHint    string `json:"fallback_hint,omitempty"`
	LogPath         string `json:"log_path,omitempty"`
}

func newBridgeDiagnosticServer(ctx context.Context, cfg userConfig, cause error) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "tuskbase-bridge-diagnostics", Version: version}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "tuskbase_diagnostics",
		Description: "Explain why Tuskbase memory tools are unavailable and show local repair commands.",
	}, func(context.Context, *mcp.CallToolRequest, bridgeDiagnosticInput) (*mcp.CallToolResult, bridgeDiagnosticOutput, error) {
		out := bridgeDiagnostic(ctx, cfg, cause)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: bridgeDiagnosticText(out)}}}, out, nil
	})
	return server
}

func bridgeDiagnostic(ctx context.Context, cfg userConfig, cause error) bridgeDiagnosticOutput {
	out := bridgeDiagnosticOutput{
		Status:  "not-ready",
		Mode:    emptyDefault(cfg.Mode, modeLocalBasic),
		Detail:  cause.Error(),
		LogPath: defaultDaemonLogPath(),
	}
	store, err := loadRuntimeStoreConfig(cfg.DBPath)
	if err != nil {
		out.Store = "unavailable"
		out.RepairHint = err.Error()
		return out
	}
	out.Store = store.Type
	check := checkRuntimeStore(ctx, cfg, store)
	if check.Checked && !check.Ready {
		out.PostgresConnect = check.Status
		out.PostgresError = check.Error
		out.RepairHint = check.RepairHint
		out.FallbackHint = check.FallbackHint
		return out
	}
	if isDockerManagedLocalShared(cfg) {
		out.RepairHint = "Start Docker Desktop or Docker Engine, confirm Local Shared Postgres is running on the configured port, then run `tuskbase daemon restart`."
		out.FallbackHint = "Use `tuskbase setup --mode local-basic --yes` to switch this machine back to SQLite without deleting the Local Shared Docker volume."
		return out
	}
	out.RepairHint = "Run `tuskbase doctor` and inspect the daemon log for details."
	return out
}

func bridgeDiagnosticText(out bridgeDiagnosticOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Tuskbase is not ready.\n")
	fmt.Fprintf(&b, "mode: %s\n", out.Mode)
	if out.Store != "" {
		fmt.Fprintf(&b, "store: %s\n", out.Store)
	}
	if out.PostgresConnect != "" {
		fmt.Fprintf(&b, "postgres_connect: %s\n", out.PostgresConnect)
	}
	if out.PostgresError != "" {
		fmt.Fprintf(&b, "postgres_error: %s\n", out.PostgresError)
	}
	fmt.Fprintf(&b, "detail: %s\n", out.Detail)
	if out.RepairHint != "" {
		fmt.Fprintf(&b, "repair_hint: %s\n", out.RepairHint)
	}
	if out.FallbackHint != "" {
		fmt.Fprintf(&b, "fallback_hint: %s\n", out.FallbackHint)
	}
	if out.LogPath != "" {
		fmt.Fprintf(&b, "log_path: %s\n", out.LogPath)
	}
	return strings.TrimSpace(b.String())
}

func newBridgeServer(ctx context.Context, endpoint string, credentials CredentialProvider, clientName string) (*mcp.Server, func() error, error) {
	session, err := connectBridgeRemote(ctx, endpoint, credentials, clientName)
	if err != nil {
		return nil, nil, err
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		session.Close()
		return nil, nil, fmt.Errorf("list daemon MCP tools: %w", err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "tuskbase-bridge", Version: version}, nil)
	for _, remote := range tools.Tools {
		if remote == nil {
			continue
		}
		tool := *remote
		server.AddTool(&tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return session.CallTool(ctx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
		})
	}
	return server, session.Close, nil
}

func connectBridgeRemote(ctx context.Context, endpoint string, credentials CredentialProvider, clientName string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "tuskbase-bridge", Version: version}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: bridgeBearerTransport{credentials: credentials, clientName: clientName}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to Tuskbase daemon at %s: %w", endpoint, err)
	}
	return session, nil
}

type bridgeBearerTransport struct {
	credentials CredentialProvider
	clientName  string
	base        http.RoundTripper
}

func (t bridgeBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	credential, err := t.credentials.Credential(req.Context(), t.clientName)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+credential.Token)
	return base.RoundTrip(clone)
}
