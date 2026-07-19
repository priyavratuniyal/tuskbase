package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/priyavratuniyal/tuskbase/internal/daemon"
)

const (
	modeLocalBasic  = "local-basic"
	modeLocalShared = "local-shared"
	defaultAddr     = "127.0.0.1:8765"
	configVersionV1 = 1
	transportBridge = "bridge"
	transportHTTP   = "http"
)

var supportedClients = []string{"codex", "claude", "cursor", "generic"}

type userConfig struct {
	ConfigVersion int                     `json:"config_version"`
	Mode          string                  `json:"mode"`
	Addr          string                  `json:"addr"`
	DBPath        string                  `json:"db_path,omitempty"`
	Store         storeConfig             `json:"store,omitempty"`
	Daemon        daemonSurfaceConfig     `json:"daemon"`
	APIKey        string                  `json:"api_key,omitempty"`
	AgentKeys     []daemon.LocalSharedKey `json:"agent_keys,omitempty"`
	UpdatedAt     string                  `json:"updated_at"`
}

type daemonSurfaceConfig struct {
	MCPEnabled       bool `json:"mcp_enabled"`
	RESTEnabled      bool `json:"rest_enabled"`
	AutostartEnabled bool `json:"autostart_enabled"`
}

func (cfg userConfig) HasAuth() bool {
	return strings.TrimSpace(cfg.APIKey) != "" || len(cfg.AgentKeys) > 0
}

func (cfg userConfig) daemonMCPEnabled() bool {
	return cfg.Daemon.MCPEnabled
}

func (cfg userConfig) daemonRESTEnabled() bool {
	return cfg.Daemon.RESTEnabled
}

func (cfg userConfig) daemonAutostartEnabled() bool {
	return cfg.Daemon.AutostartEnabled
}

func applyDaemonDefaults(cfg *userConfig) {
	if !cfg.Daemon.MCPEnabled && !cfg.Daemon.RESTEnabled && !cfg.Daemon.AutostartEnabled {
		cfg.Daemon = daemonSurfaceConfig{MCPEnabled: true, RESTEnabled: false, AutostartEnabled: true}
		return
	}
	if !cfg.Daemon.MCPEnabled {
		cfg.Daemon.MCPEnabled = true
	}
}

func runSetup(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", modeLocalBasic, "local-basic or local-shared")
	clients := fs.String("client", "", "comma-separated clients to show after setup: codex, claude, cursor, generic, or all")
	printOnly := fs.Bool("print-only", false, "print the planned setup without writing config")
	reveal := fs.Bool("reveal", false, "show generated secrets in printed output")
	transport := fs.String("transport", transportBridge, "MCP client transport to print/apply: bridge or http")
	postgresDSN := fs.String("postgres-dsn", "", "Postgres DSN for local-shared setup")
	postgresDriver := fs.String("postgres-driver", "", "Postgres database/sql driver for local-shared setup")
	postgresSource := fs.String("postgres-source", postgresSourceAuto, "Local Shared Postgres source: auto, docker, or existing")
	dockerContextDefault := configuredDockerContext()
	dockerPostgresPort := fs.Int("docker-postgres-port", configuredDockerPostgresPort(), "host port for Docker-managed Local Shared Postgres")
	dockerPostgresImage := fs.String("docker-postgres-image", configuredDockerPostgresImage(), "Docker image for Docker-managed Local Shared pgvector Postgres")
	dockerContext := fs.String("docker-context", dockerContextDefault, "Docker context for Docker-managed Local Shared Postgres: context name or auto")
	apply := fs.Bool("apply", false, "apply supported client config instead of only printing it")
	yes := fs.Bool("yes", false, "accept defaults for non-interactive setup")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = yes // The current setup only writes Tuskbase-owned config, so no external confirmation is needed.

	selectedClients, err := parseClients(*clients)
	if err != nil {
		return err
	}
	selectedMode, err := normalizeMode(*mode)
	if err != nil {
		return err
	}
	selectedTransport, err := normalizeTransport(*transport)
	if err != nil {
		return err
	}
	if *printOnly && *apply {
		return errors.New("use either --print-only or --apply, not both")
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	cfg, found, err := loadUserConfig()
	if err != nil {
		return err
	}
	cfg.ConfigVersion = configVersionV1
	cfg.Mode = selectedMode
	if strings.TrimSpace(cfg.Addr) == "" {
		cfg.Addr = defaultAddr
	}
	if strings.TrimSpace(cfg.DBPath) == "" {
		cfg.DBPath = defaultDBPath()
	}
	storeResult, err := applySetupStoreConfig(&cfg, setupStoreOptions{
		PostgresDSN:         *postgresDSN,
		PostgresDriver:      *postgresDriver,
		PostgresSource:      *postgresSource,
		DockerPostgresPort:  *dockerPostgresPort,
		DockerPostgresImage: *dockerPostgresImage,
		DockerContext:       *dockerContext,
		DockerContextSet:    flagWasSet(fs, "docker-context") || strings.TrimSpace(dockerContextDefault) != "",
		PrintOnly:           *printOnly,
		ConfigPath:          path,
	})
	if err != nil {
		return err
	}
	cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	applyDaemonDefaults(&cfg)
	generatedSecret := false

	switch selectedMode {
	case modeLocalBasic:
		if strings.TrimSpace(cfg.APIKey) == "" {
			cfg.APIKey, err = generateSecret()
			if err != nil {
				return err
			}
			generatedSecret = true
		}
		cfg.AgentKeys = nil
	case modeLocalShared:
		if len(cfg.AgentKeys) == 0 {
			cfg.AgentKeys, err = defaultLocalSharedKeys()
			if err != nil {
				return err
			}
			generatedSecret = true
		}
		cfg.APIKey = ""
	}

	p := newPresenter(stdout)
	if p.pretty {
		p.Header()
		p.Section("Setup")
	} else {
		fmt.Fprintf(stdout, "Tuskbase setup\n")
	}
	p.KV("mode", cfg.Mode)
	p.KV("addr", cfg.Addr)
	if p.pretty {
		p.Section("Store")
	}
	printStoreSummary(stdout, cfg)
	printSetupStoreResult(stdout, storeResult)
	if p.pretty {
		p.Section("Runtime")
	}
	p.KV("config", path)
	if found {
		p.KV("existing config", "reused")
	}
	if *printOnly {
		p.KV("write", "skipped (--print-only)")
	} else if err := saveUserConfig(path, cfg); err != nil {
		return err
	} else {
		p.KV("write", "ok")
	}
	switch {
	case *printOnly && generatedSecret:
		p.KV("secret", "generated for preview; not stored")
	case *printOnly:
		p.KV("secret", "reused from existing config; not shown")
	case generatedSecret:
		p.KV("secret", "generated and stored locally")
	default:
		p.KV("secret", "reused from existing config")
	}
	if p.pretty {
		p.Section("Service")
	}
	if *printOnly {
		p.KV("service", "skipped (--print-only)")
	} else if cfg.Mode == modeLocalShared && !hasPostgresDSN(cfg) {
		p.KV("service", "skipped (postgres dsn required for local-shared)")
	} else if cfg.daemonAutostartEnabled() {
		result := newLifecycleController().InstallAndStart(context.Background(), cfg)
		printLifecycleResult(stdout, "service", result)
	} else {
		p.KV("service", "autostart disabled")
	}
	if *reveal {
		if p.pretty {
			p.Section("Auth")
		}
		printSecrets(stdout, cfg)
	}
	if len(selectedClients) == 0 && p.pretty {
		p.Section("Next")
		p.Hint("Run `tuskbase connect codex` to print MCP client setup.")
	}
	for _, client := range selectedClients {
		fmt.Fprintln(stdout)
		if p.pretty {
			p.Section("Connect")
			p.Hint("Exact MCP config command for %s.", client)
		}
		printConnectConfigBody(stdout, client, cfg, selectedTransport, *reveal, false)
		if *apply {
			if err := applyConnectConfig(client, cfg, selectedTransport, stdout, stderr); err != nil {
				return err
			}
		}
	}
	return nil
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func printSetupStoreResult(w io.Writer, result setupStoreResult) {
	if result.DockerPostgres == nil {
		return
	}
	p := newPresenter(w)
	detail := strings.TrimSpace(result.DockerPostgres.Detail)
	switch {
	case result.DockerPostgres.Skipped:
		if detail == "" {
			detail = "skipped"
		}
		p.KV("docker_postgres", detail)
	case result.DockerPostgres.Ready:
		p.KV("docker_postgres", "ready")
	default:
		if detail == "" {
			detail = "configured"
		}
		p.KV("docker_postgres", detail)
	}
}

func runConnect(args []string, stdout, stderr io.Writer) error {
	client := "generic"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		client = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "", "override setup mode for printed config: local-basic or local-shared")
	reveal := fs.Bool("reveal", false, "include stored secrets in printed commands")
	transport := fs.String("transport", transportBridge, "MCP client transport to print: bridge or http")
	apply := fs.Bool("apply", false, "apply supported client config instead of only printing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		client = fs.Arg(0)
	}
	client, err := canonicalClient(client)
	if err != nil {
		return err
	}
	cfg, found, err := loadUserConfig()
	if err != nil {
		return err
	}
	if !found {
		cfg = userConfig{Mode: modeLocalBasic, Addr: defaultAddr}
	}
	selectedTransport, err := normalizeTransport(*transport)
	if err != nil {
		return err
	}
	if *mode != "" {
		cfg.Mode, err = normalizeMode(*mode)
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(cfg.Addr) == "" {
		cfg.Addr = defaultAddr
	}
	printConnectConfig(stdout, client, cfg, selectedTransport, *reveal)
	if *apply {
		if err := applyConnectConfig(client, cfg, selectedTransport, stdout, stderr); err != nil {
			return err
		}
	}
	printAgentWorkflowInstructions(stdout)
	return nil
}

func runAuthCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stdout, "Usage: tuskbase auth <show|list|rotate|add|remove|set-role>")
		return nil
	}
	switch args[0] {
	case "show", "list":
		return runAuthList(args[1:], stdout, stderr)
	case "rotate":
		return runAuthRotate(args[1:], stdout, stderr)
	case "add":
		return runAuthAdd(args[1:], stdout, stderr)
	case "remove":
		return runAuthRemove(args[1:], stdout, stderr)
	case "set-role":
		return runAuthSetRole(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown auth command %q", args[0])
	}
}

func runAuthList(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("auth list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reveal := fs.Bool("reveal", false, "reveal stored local secrets")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := loadRequiredUserConfig()
	if err != nil {
		return err
	}
	printAuthSummary(stdout, cfg, *reveal)
	return nil
}

func runAuthRotate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("auth rotate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "Local Shared key name to rotate")
	all := fs.Bool("all", false, "rotate every Local Shared key")
	yes := fs.Bool("yes", false, "accept rotation without prompting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = yes
	cfg, path, err := loadRequiredUserConfig()
	if err != nil {
		return err
	}
	rotated := []string{}
	switch cfg.Mode {
	case modeLocalBasic:
		if *name != "" || *all {
			return errors.New("Local Basic has one key; run `tuskbase auth rotate` without --name or --all")
		}
		key, err := generateSecret()
		if err != nil {
			return err
		}
		cfg.APIKey = key
		rotated = append(rotated, "local-api-key")
	case modeLocalShared:
		if *name != "" && *all {
			return errors.New("use either --name or --all, not both")
		}
		if *name == "" && !*all {
			return errors.New("Local Shared rotation requires --name <key> or --all")
		}
		if *all {
			for i := range cfg.AgentKeys {
				key, err := generateSecret()
				if err != nil {
					return err
				}
				cfg.AgentKeys[i].Key = key
				rotated = append(rotated, cfg.AgentKeys[i].Name)
			}
		} else {
			keyName, err := validateKeyName(*name)
			if err != nil {
				return err
			}
			idx := findAgentKey(cfg.AgentKeys, keyName)
			if idx < 0 {
				return fmt.Errorf("local shared key %q not found", keyName)
			}
			key, err := generateSecret()
			if err != nil {
				return err
			}
			cfg.AgentKeys[idx].Key = key
			rotated = append(rotated, keyName)
		}
	default:
		return errors.New("auth rotate requires local-basic or local-shared setup")
	}
	cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := saveUserConfig(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "rotated: %s\n", strings.Join(rotated, ","))
	return nil
}

func runAuthAdd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("auth add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "Local Shared key name")
	role := fs.String("role", "agent", "reader, agent, or admin")
	reveal := fs.Bool("reveal", false, "print the generated secret")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, path, err := loadRequiredUserConfig()
	if err != nil {
		return err
	}
	if cfg.Mode != modeLocalShared {
		return errors.New("auth add requires local-shared setup")
	}
	keyName, err := validateKeyName(*name)
	if err != nil {
		return err
	}
	if findAgentKey(cfg.AgentKeys, keyName) >= 0 {
		return fmt.Errorf("local shared key %q already exists", keyName)
	}
	cleanRole, err := daemon.NormalizeLocalRole(*role)
	if err != nil {
		return err
	}
	secret, err := generateSecret()
	if err != nil {
		return err
	}
	cfg.AgentKeys = append(cfg.AgentKeys, daemon.LocalSharedKey{Name: keyName, Role: cleanRole, Key: secret})
	sortAgentKeys(cfg.AgentKeys)
	cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := saveUserConfig(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "added: %s role=%s\n", keyName, cleanRole)
	if *reveal {
		fmt.Fprintf(stdout, "%s:%s:%s\n", keyName, cleanRole, secret)
	}
	return nil
}

func runAuthRemove(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("auth remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "Local Shared key name")
	yes := fs.Bool("yes", false, "accept removal without prompting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = yes
	cfg, path, err := loadRequiredUserConfig()
	if err != nil {
		return err
	}
	if cfg.Mode != modeLocalShared {
		return errors.New("auth remove requires local-shared setup")
	}
	if len(cfg.AgentKeys) <= 1 {
		return errors.New("cannot remove the final Local Shared key")
	}
	keyName, err := validateKeyName(*name)
	if err != nil {
		return err
	}
	idx := findAgentKey(cfg.AgentKeys, keyName)
	if idx < 0 {
		return fmt.Errorf("local shared key %q not found", keyName)
	}
	cfg.AgentKeys = append(cfg.AgentKeys[:idx], cfg.AgentKeys[idx+1:]...)
	cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := saveUserConfig(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "removed: %s\n", keyName)
	return nil
}

func runAuthSetRole(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("auth set-role", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "Local Shared key name")
	role := fs.String("role", "", "reader, agent, or admin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, path, err := loadRequiredUserConfig()
	if err != nil {
		return err
	}
	if cfg.Mode != modeLocalShared {
		return errors.New("auth set-role requires local-shared setup")
	}
	keyName, err := validateKeyName(*name)
	if err != nil {
		return err
	}
	cleanRole, err := daemon.NormalizeLocalRole(*role)
	if err != nil {
		return err
	}
	idx := findAgentKey(cfg.AgentKeys, keyName)
	if idx < 0 {
		return fmt.Errorf("local shared key %q not found", keyName)
	}
	cfg.AgentKeys[idx].Role = cleanRole
	cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := saveUserConfig(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "updated: %s role=%s\n", keyName, cleanRole)
	return nil
}

func normalizeMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", modeLocalBasic, "basic", "local":
		return modeLocalBasic, nil
	case modeLocalShared, "shared":
		return modeLocalShared, nil
	default:
		return "", fmt.Errorf("unknown setup mode %q", mode)
	}
}

func parseClients(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if strings.EqualFold(raw, "all") {
		return []string{"codex", "claude", "cursor"}, nil
	}
	parts := strings.Split(raw, ",")
	clients := make([]string, 0, len(parts))
	for _, part := range parts {
		client, err := canonicalClient(part)
		if err != nil {
			return nil, err
		}
		clients = append(clients, client)
	}
	return clients, nil
}

func normalizeTransport(transport string) (string, error) {
	transport = strings.ToLower(strings.TrimSpace(transport))
	switch transport {
	case "", transportBridge:
		return transportBridge, nil
	case transportHTTP, "streamable-http":
		return transportHTTP, nil
	default:
		return "", fmt.Errorf("unknown transport %q; expected bridge or http", transport)
	}
}

func canonicalClient(client string) (string, error) {
	client = strings.ToLower(strings.TrimSpace(client))
	if client == "" {
		return "generic", nil
	}
	if client == "claude-code" {
		return "claude", nil
	}
	for _, supported := range supportedClients {
		if client == supported {
			return client, nil
		}
	}
	return "", fmt.Errorf("unsupported client %q; supported clients: %s", client, strings.Join(supportedClients, ", "))
}

func printConnectConfig(w io.Writer, client string, cfg userConfig, transport string, reveal bool) {
	printConnectConfigBody(w, client, cfg, transport, reveal, true)
}

func printConnectConfigBody(w io.Writer, client string, cfg userConfig, transport string, reveal bool, showHeader bool) {
	p := newPresenter(w)
	client, err := canonicalClient(client)
	if err != nil {
		fmt.Fprintf(w, "%s\n", err)
		return
	}
	transport, err = normalizeTransport(transport)
	if err != nil {
		fmt.Fprintf(w, "%s\n", err)
		return
	}
	if transport == transportBridge {
		if p.pretty && showHeader {
			p.Header()
			p.Section("Connect")
			p.Hint("Bridge transport keeps bearer tokens in Tuskbase-owned local config.")
		}
		fmt.Fprintf(w, "# Tuskbase bridge keeps bearer tokens in Tuskbase-owned local config.\n")
		bridgeClient := bridgeClientName(cfg, client)
		printStdioConfig(w, client, []string{"bridge", "--client", bridgeClient})
		return
	}
	if p.pretty && showHeader {
		p.Header()
		p.Section("Connect")
		p.Hint("HTTP transport uses an MCP URL plus bearer-token auth.")
	}
	printHTTPConnectConfig(w, client, cfg, reveal)
}

func printStdioConfig(w io.Writer, client string, args []string) {
	switch client {
	case "codex":
		fmt.Fprintf(w, "# Codex MCP config\n")
		fmt.Fprintf(w, "codex mcp add tuskbase -- tuskbase")
		for _, arg := range args {
			fmt.Fprintf(w, " %s", shellWord(arg))
		}
		fmt.Fprintf(w, "\n")
	case "claude":
		fmt.Fprintf(w, "# Claude Code MCP config\n")
		fmt.Fprintf(w, "claude mcp add --transport stdio tuskbase -- tuskbase")
		for _, arg := range args {
			fmt.Fprintf(w, " %s", shellWord(arg))
		}
		fmt.Fprintf(w, "\n")
	case "cursor":
		fmt.Fprintf(w, "# Cursor MCP config (~/.cursor/mcp.json)\n")
		fmt.Fprintf(w, "{\n  \"mcpServers\": {\n    \"tuskbase\": {\n      \"command\": \"tuskbase\",\n      \"args\": %s\n    }\n  }\n}\n", jsonArray(args))
	default:
		fmt.Fprintf(w, "# Generic stdio MCP config\n")
		fmt.Fprintf(w, "command: tuskbase\nargs: %s\n", jsonArray(args))
	}
}

func printHTTPConnectConfig(w io.Writer, client string, cfg userConfig, reveal bool) {
	url := "http://" + cfg.Addr + "/mcp"
	token := tokenForClient(cfg, client, reveal)
	fmt.Fprintf(w, "# Authenticated HTTP clients can omit actor; Tuskbase derives it from the bearer token.\n")
	switch client {
	case "codex":
		envVar := tokenEnvVar(cfg, client)
		fmt.Fprintf(w, "# Codex HTTP MCP config\n")
		fmt.Fprintf(w, "codex mcp add tuskbase --url %s --bearer-token-env-var %s\n", url, envVar)
		fmt.Fprintf(w, "# Codex reads the bearer token from %s. Prefer bridge transport for local setup.\n", envVar)
		if reveal && token != "" {
			fmt.Fprintf(w, "export %s=%s\n", envVar, token)
		} else {
			fmt.Fprintf(w, "# Run `tuskbase auth list --reveal` if you need to copy the token.\n")
		}
	case "claude":
		fmt.Fprintf(w, "# Claude Code HTTP MCP config\n")
		fmt.Fprintf(w, "claude mcp add --transport http tuskbase %s --header \"Authorization: Bearer %s\"\n", url, token)
	case "cursor":
		fmt.Fprintf(w, "# Cursor HTTP MCP config (~/.cursor/mcp.json)\n")
		fmt.Fprintf(w, "{\n  \"mcpServers\": {\n    \"tuskbase\": {\n      \"type\": \"streamable-http\",\n      \"url\": %q,\n      \"headers\": {\n        \"Authorization\": \"Bearer %s\"\n      }\n    }\n  }\n}\n", url, token)
	default:
		fmt.Fprintf(w, "# Generic HTTP MCP config\n")
		fmt.Fprintf(w, "url: %s\nAuthorization: Bearer %s\n", url, token)
	}
}

func printAgentWorkflowInstructions(w io.Writer) {
	p := newPresenter(w)
	if p.pretty {
		p.Section("Agent workflow instructions")
		p.Hint("Compliant agents should use the high-level workflow tools automatically.")
	}
	fmt.Fprintln(w, "# Agent workflow instructions")
	fmt.Fprintln(w, "- Call tuskbase_prepare_change before editing.")
	fmt.Fprintln(w, "- Do not edit when should_edit=false; ask the user how to proceed.")
	fmt.Fprintln(w, "- Call tuskbase_finish_change after verification.")
	fmt.Fprintln(w, "- Remember only durable repo decisions, not routine summaries or chat logs.")
}

func bridgeClientName(cfg userConfig, client string) string {
	if cfg.Mode == modeLocalShared && client == "generic" {
		return "<key-name>"
	}
	return client
}

func jsonArray(values []string) string {
	data, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func shellWord(value string) string {
	if strings.ContainsAny(value, " \t\n\r\"'") {
		return fmt.Sprintf("%q", value)
	}
	return value
}

func tokenEnvVar(cfg userConfig, client string) string {
	if cfg.Mode == modeLocalShared && client != "generic" {
		return "TUSKBASE_" + strings.ToUpper(strings.ReplaceAll(client, "-", "_")) + "_KEY"
	}
	return "TUSKBASE_API_KEY"
}

func tokenForClient(cfg userConfig, client string, reveal bool) string {
	if cfg.Mode == modeLocalShared {
		for _, key := range cfg.AgentKeys {
			if strings.EqualFold(key.Name, client) {
				if reveal {
					return key.Key
				}
				return "<" + key.Name + "-key>"
			}
		}
		if client == "generic" {
			return "<local-shared-key>"
		}
	}
	if reveal && cfg.APIKey != "" {
		return cfg.APIKey
	}
	return "<tuskbase-key>"
}

func printAuthSummary(w io.Writer, cfg userConfig, reveal bool) {
	p := newPresenter(w)
	if p.pretty {
		p.Header()
		p.Section("Auth")
	}
	p.KV("mode", cfg.Mode)
	p.KV("auth_policy", authPolicyName(cfg))
	p.KV("auth_source", "config")
	switch cfg.Mode {
	case modeLocalBasic:
		if p.pretty {
			p.Section("Keys")
			fmt.Fprintf(w, "  %-16s %-8s %s\n", "name", "role", "key")
			fmt.Fprintf(w, "  %-16s %-8s %s\n", "local-api-key", "agent", secretForPrint(cfg.APIKey, reveal))
		} else {
			fmt.Fprintf(w, "local-api-key role=agent key=%s\n", secretForPrint(cfg.APIKey, reveal))
		}
	case modeLocalShared:
		keys := append([]daemon.LocalSharedKey(nil), cfg.AgentKeys...)
		sortAgentKeys(keys)
		if p.pretty {
			p.Section("Keys")
			fmt.Fprintf(w, "  %-16s %-8s %s\n", "name", "role", "key")
		}
		for _, key := range keys {
			if p.pretty {
				fmt.Fprintf(w, "  %-16s %-8s %s\n", key.Name, key.Role, secretForPrint(key.Key, reveal))
			} else {
				fmt.Fprintf(w, "%s role=%s key=%s\n", key.Name, key.Role, secretForPrint(key.Key, reveal))
			}
		}
	default:
		p.KV("secret", "none")
	}
}

func printSecrets(w io.Writer, cfg userConfig) {
	if cfg.APIKey != "" {
		fmt.Fprintf(w, "TUSKBASE_API_KEY=%s\n", cfg.APIKey)
	}
	if len(cfg.AgentKeys) > 0 {
		parts := make([]string, 0, len(cfg.AgentKeys))
		for _, key := range cfg.AgentKeys {
			parts = append(parts, fmt.Sprintf("%s:%s:%s", key.Name, key.Role, key.Key))
		}
		sort.Strings(parts)
		fmt.Fprintf(w, "TUSKBASE_AGENT_KEYS=%s\n", strings.Join(parts, ","))
	}
}

func secretForPrint(secret string, reveal bool) string {
	if reveal {
		return secret
	}
	return "<hidden>"
}

func authPolicyName(cfg userConfig) string {
	if len(cfg.AgentKeys) > 0 {
		return "local-shared-keys"
	}
	if cfg.APIKey != "" {
		return "local-api-key"
	}
	return "none"
}

func defaultLocalSharedKeys() ([]daemon.LocalSharedKey, error) {
	keys := make([]daemon.LocalSharedKey, 0, 3)
	for _, client := range []string{"codex", "claude", "cursor"} {
		key, err := generateSecret()
		if err != nil {
			return nil, err
		}
		keys = append(keys, daemon.LocalSharedKey{Name: client, Role: "agent", Key: key})
	}
	return keys, nil
}

func generateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "tbk_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func validateKeyName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", errors.New("local shared key name is required")
	}
	if strings.ContainsAny(name, ",:\t\n\r ") {
		return "", fmt.Errorf("local shared key name %q cannot contain whitespace, comma, or colon", name)
	}
	return name, nil
}

func findAgentKey(keys []daemon.LocalSharedKey, name string) int {
	for i, key := range keys {
		if strings.EqualFold(key.Name, name) {
			return i
		}
	}
	return -1
}

func sortAgentKeys(keys []daemon.LocalSharedKey) {
	sort.SliceStable(keys, func(i, j int) bool {
		return keys[i].Name < keys[j].Name
	})
}

func applyConnectConfig(client string, cfg userConfig, transport string, stdout, stderr io.Writer) error {
	client, err := canonicalClient(client)
	if err != nil {
		return err
	}
	transport, err = normalizeTransport(transport)
	if err != nil {
		return err
	}
	if client != "codex" {
		return fmt.Errorf("--apply currently supports codex only; printed %s config instead", client)
	}
	args := []string{"mcp", "add", "tuskbase"}
	if transport == transportBridge {
		args = append(args, "--", "tuskbase", "bridge", "--client", bridgeClientName(cfg, client))
	} else {
		args = append(args, "--url", "http://"+cfg.Addr+"/mcp", "--bearer-token-env-var", tokenEnvVar(cfg, client))
	}
	cmd := exec.Command("codex", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("apply codex MCP config: %w", err)
	}
	fmt.Fprintf(stdout, "apply: codex ok\n")
	return nil
}

func configPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("TUSKBASE_CONFIG_PATH")); path != "" {
		return path, nil
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "tuskbase", "config.json"), nil
}

func loadRequiredUserConfig() (userConfig, string, error) {
	path, err := configPath()
	if err != nil {
		return userConfig{}, "", err
	}
	cfg, found, err := loadUserConfig()
	if err != nil {
		return userConfig{}, "", err
	}
	if !found {
		return userConfig{}, "", errors.New("no Tuskbase setup found; run `tuskbase setup` first")
	}
	return cfg, path, nil
}

func loadUserConfig() (userConfig, bool, error) {
	path, err := configPath()
	if err != nil {
		return userConfig{}, false, err
	}
	found, err := repairConfigFileMode(path)
	if err != nil {
		return userConfig{}, false, err
	}
	if !found {
		return userConfig{}, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return userConfig{}, false, err
	}
	var cfg userConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return userConfig{}, false, err
	}
	if cfg.ConfigVersion == 0 {
		cfg.ConfigVersion = configVersionV1
	}
	applyDaemonDefaults(&cfg)
	return cfg, true, nil
}

func saveUserConfig(path string, cfg userConfig) error {
	if cfg.ConfigVersion == 0 {
		cfg.ConfigVersion = configVersionV1
	}
	if err := rejectConfigSymlink(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func repairConfigFileMode(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("refusing to read symlinked Tuskbase config %s", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return false, fmt.Errorf("repair config permissions: %w", err)
		}
	}
	return true, nil
}

func rejectConfigSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write symlinked Tuskbase config %s", path)
	}
	return nil
}
