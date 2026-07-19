package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/priyavratuniyal/tuskbase/internal/adapters/embeddings"
	"github.com/priyavratuniyal/tuskbase/internal/daemon"
)

func TestVersionCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"version"}, &out, &errb); err != nil {
		t.Fatalf("execute(version) error = %v", err)
	}
	if !strings.Contains(out.String(), "tuskbase ") || !strings.Contains(out.String(), "go:") {
		t.Fatalf("version output = %q", out.String())
	}
}

func TestUsagePrioritizesFriendlyCommands(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out)
	got := out.String()
	for _, want := range []string{"setup", "start", "status", "connect [client]", "bridge"} {
		if !strings.Contains(got, want) {
			t.Fatalf("usage missing %q: %s", want, got)
		}
	}
	if strings.Index(got, "setup") > strings.Index(got, "serve") {
		t.Fatalf("usage should show setup before advanced serve: %s", got)
	}
}

func TestInitMCPRejectsDemoMode(t *testing.T) {
	var out, errb bytes.Buffer
	err := execute(context.Background(), []string{"init-mcp", "codex", "--mode", "demo"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "unknown setup mode") {
		t.Fatalf("execute(init-mcp demo) error = %v, want unsupported mode", err)
	}
}

func TestConnectCodexPrintsMCPCommandAndWorkflow(t *testing.T) {
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"connect", "codex", "--mode", "local-basic"}, &out, &errb); err != nil {
		t.Fatalf("execute(connect codex) error = %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"codex mcp add tuskbase -- tuskbase bridge --client codex",
		"# Agent workflow instructions",
		"tuskbase_prepare_change before editing",
		"should_edit=false",
		"tuskbase_finish_change after verification",
		"durable repo decisions",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("connect output missing %q: %s", want, got)
		}
	}
}

func TestConnectClaudeLocalBasic(t *testing.T) {
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"connect", "claude", "--mode", "local-basic"}, &out, &errb); err != nil {
		t.Fatalf("execute(connect claude) error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "claude mcp add") || !strings.Contains(got, "tuskbase bridge --client claude") {
		t.Fatalf("connect output = %q", got)
	}
	if strings.Contains(got, "TUSKBASE_API_KEY") || strings.Contains(got, "Authorization: Bearer") {
		t.Fatalf("bridge output leaked HTTP auth setup = %q", got)
	}
	if !strings.Contains(got, "# Agent workflow instructions") {
		t.Fatalf("connect output missing workflow instructions = %q", got)
	}
}

func TestConnectCodexHTTPTransportRemainsAvailable(t *testing.T) {
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"connect", "codex", "--mode", "local-basic", "--transport", "http"}, &out, &errb); err != nil {
		t.Fatalf("execute(connect codex http) error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "--bearer-token-env-var TUSKBASE_API_KEY") {
		t.Fatalf("HTTP transport output = %q", got)
	}
}

func TestConnectCodexApplyOnlyAppliesMCPConfigAndPrintsWorkflow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell executable is Unix-specific")
	}
	root := t.TempDir()
	fakeCodex := filepath.Join(root, "codex")
	argsPath := filepath.Join(root, "codex-args.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$CODEX_ARGS_FILE\"\n"
	if err := os.WriteFile(fakeCodex, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_ARGS_FILE", argsPath)

	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"connect", "codex", "--mode", "local-basic", "--apply"}, &out, &errb); err != nil {
		t.Fatalf("execute(connect codex --apply) error = %v stderr=%s", err, errb.String())
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read fake codex args: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "mcp add tuskbase -- tuskbase bridge --client codex" {
		t.Fatalf("codex args = %q", got)
	}
	got := out.String()
	if !strings.Contains(got, "apply: codex ok") || !strings.Contains(got, "# Agent workflow instructions") {
		t.Fatalf("apply output = %q", got)
	}
	if strings.Contains(got, "AGENTS.md") || strings.Contains(got, "instructions file") {
		t.Fatalf("apply output suggests global instruction rewrite: %q", got)
	}
}

func TestInitExplainsSetup(t *testing.T) {
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"init"}, &out, &errb); err != nil {
		t.Fatalf("execute(init) error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "tuskbase setup") || !strings.Contains(got, "Local Basic") {
		t.Fatalf("init output = %q", got)
	}
}

func TestSetupPrintOnlyDoesNotWriteConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"setup", "--print-only", "--mode", "local-basic", "--client", "all"}, &out, &errb); err != nil {
		t.Fatalf("execute(setup --print-only) error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config file exists after print-only, stat err = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "write: skipped") || !strings.Contains(got, "Codex MCP config") || !strings.Contains(got, "Cursor MCP config") {
		t.Fatalf("setup print output = %q", got)
	}
}

func TestSetupWritesGeneratedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"setup", "--mode", "local-basic", "--yes"}, &out, &errb); err != nil {
		t.Fatalf("execute(setup) error = %v", err)
	}
	cfg, found, err := loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() error = %v", err)
	}
	if !found {
		t.Fatal("loadUserConfig() found = false")
	}
	if cfg.Mode != modeLocalBasic || !strings.HasPrefix(cfg.APIKey, "tbk_") {
		t.Fatalf("config = %+v", cfg)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %o, want 600", got)
	}
	if !strings.Contains(out.String(), "secret: generated and stored locally") {
		t.Fatalf("setup output = %q", out.String())
	}
}

func TestEnvExampleDocumentsSupportedVariables(t *testing.T) {
	data, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	got := string(data)
	for _, want := range []string{"TUSKBASE_API_KEY", "TUSKBASE_AGENT_KEYS", "TUSKBASE_ADDR", "TUSKBASE_STORE", "TUSKBASE_DB_PATH", "TUSKBASE_BACKUP_DIR", "TUSKBASE_BACKUP_AUTO", "TUSKBASE_BACKUP_AUTO_RETENTION", "TUSKBASE_POSTGRES_DSN", "TUSKBASE_POSTGRES_DRIVER", "TUSKBASE_DOCKER_POSTGRES_IMAGE", "TUSKBASE_DOCKER_POSTGRES_PORT", "TUSKBASE_DOCKER_CONTEXT", "TUSKBASE_CONFIG_PATH", "OPENAI_API_KEY"} {
		if !strings.Contains(got, want) {
			t.Fatalf(".env.example missing %q", want)
		}
	}
}

func TestLoadRuntimeConfigUsesLocalAPIKeyAuth(t *testing.T) {
	t.Setenv("TUSKBASE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	t.Setenv("TUSKBASE_API_KEY", "local-secret")
	cfg, err := loadRuntimeConfig("127.0.0.1:8765", "tuskbase.db", true, false)
	if err != nil {
		t.Fatalf("loadRuntimeConfig() error = %v", err)
	}
	if got := cfg.Auth.Name(); got != "local-api-key" {
		t.Fatalf("auth policy = %q, want local-api-key", got)
	}
}

func TestLoadRuntimeConfigUsesStoredConfigAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	if err := saveUserConfig(path, userConfig{Mode: modeLocalBasic, Addr: defaultAddr, APIKey: "stored-secret"}); err != nil {
		t.Fatalf("saveUserConfig() error = %v", err)
	}
	cfg, err := loadRuntimeConfig("127.0.0.1:8765", "tuskbase.db", true, false)
	if err != nil {
		t.Fatalf("loadRuntimeConfig() error = %v", err)
	}
	if got := cfg.Auth.Name(); got != "local-api-key" {
		t.Fatalf("auth policy = %q, want local-api-key", got)
	}
}

func TestLoadRuntimeConfigUsesLocalSharedKeys(t *testing.T) {
	t.Setenv("TUSKBASE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "codex:agent:codex-secret,claude:reader:claude-secret")
	cfg, err := loadRuntimeConfig("127.0.0.1:8765", "tuskbase.db", true, false)
	if err != nil {
		t.Fatalf("loadRuntimeConfig() error = %v", err)
	}
	if got := cfg.Auth.Name(); got != "local-shared-keys" {
		t.Fatalf("auth policy = %q, want local-shared-keys", got)
	}
}

func TestSetupLocalSharedStoresPostgresConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"setup", "--mode", "local-shared", "--postgres-dsn", "postgres://tuskbase:secret@localhost:5432/tuskbase?sslmode=disable", "--yes"}, &out, &errb); err != nil {
		t.Fatalf("setup local-shared error = %v", err)
	}
	cfg, found, err := loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() error = %v", err)
	}
	if !found {
		t.Fatal("loadUserConfig() found = false")
	}
	if cfg.Store.Type != storePostgres || cfg.Store.Postgres == nil || cfg.Store.Postgres.Driver != defaultPostgresDriver || cfg.Store.Postgres.Source != postgresSourceExisting || !strings.Contains(cfg.Store.Postgres.DSN, "localhost:5432") {
		t.Fatalf("store config = %+v", cfg.Store)
	}
	if !strings.Contains(out.String(), "postgres_source: existing") || !strings.Contains(out.String(), "postgres_dsn: configured") {
		t.Fatalf("setup output = %q", out.String())
	}
}

func TestSetupLocalSharedDefaultsToDockerPostgres(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"setup", "--mode", "local-shared", "--yes", "--docker-postgres-port", "9876", "--docker-postgres-image", "pgvector/pgvector:test"}, &out, &errb); err != nil {
		t.Fatalf("setup local-shared docker error = %v", err)
	}
	cfg, found, err := loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() error = %v", err)
	}
	if !found {
		t.Fatal("loadUserConfig() found = false")
	}
	if cfg.Store.Type != storePostgres || cfg.Store.Postgres == nil || cfg.Store.Postgres.Source != postgresSourceDocker || cfg.Store.Postgres.Docker == nil {
		t.Fatalf("store config = %+v", cfg.Store)
	}
	if cfg.Store.Postgres.Docker.Port != 9876 || cfg.Store.Postgres.Docker.Image != "pgvector/pgvector:test" {
		t.Fatalf("docker config = %+v", cfg.Store.Postgres.Docker)
	}
	if !strings.Contains(cfg.Store.Postgres.DSN, "127.0.0.1:9876") {
		t.Fatalf("docker dsn = %q", cfg.Store.Postgres.DSN)
	}
	if _, err := os.Stat(cfg.Store.Postgres.Docker.ComposePath); err != nil {
		t.Fatalf("docker compose file was not written: %v", err)
	}
	got := out.String()
	for _, want := range []string{"postgres_source: docker", "docker_postgres: ready", "docker_postgres_port: 9876", "postgres_dsn: configured"} {
		if !strings.Contains(got, want) {
			t.Fatalf("setup docker output missing %q: %s", want, got)
		}
	}
}

func TestSetupLocalSharedDockerContextStoredAndPrinted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"setup", "--mode", "local-shared", "--yes", "--docker-context", "desktop-linux"}, &out, &errb); err != nil {
		t.Fatalf("setup local-shared docker context error = %v", err)
	}
	cfg, found, err := loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() error = %v", err)
	}
	if !found {
		t.Fatal("loadUserConfig() found = false")
	}
	if cfg.Store.Postgres == nil || cfg.Store.Postgres.Docker == nil || cfg.Store.Postgres.Docker.Context != "desktop-linux" {
		t.Fatalf("docker config = %+v", cfg.Store)
	}
	if !strings.Contains(out.String(), "docker_context: desktop-linux") {
		t.Fatalf("setup output = %q", out.String())
	}
}

func TestSetupLocalSharedDockerPrintOnlyDoesNotWriteCompose(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"setup", "--mode", "local-shared", "--print-only", "--yes"}, &out, &errb); err != nil {
		t.Fatalf("setup local-shared print-only error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config file exists after print-only, stat err = %v", err)
	}
	composePath := filepath.Join(root, "local-shared", "docker-compose.yml")
	if _, err := os.Stat(composePath); !os.IsNotExist(err) {
		t.Fatalf("compose file exists after print-only, stat err = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "postgres_source: docker") || !strings.Contains(got, "docker_postgres: skipped (--print-only)") {
		t.Fatalf("setup print-only output = %q", got)
	}
}

func TestLoadRuntimeConfigUsesPostgresStoreFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	if err := saveUserConfig(path, userConfig{Mode: modeLocalShared, Addr: defaultAddr, AgentKeys: []daemon.LocalSharedKey{{Name: "codex", Role: "agent", Key: "secret"}}, Store: storeConfig{Type: storePostgres, Postgres: &postgresStoreConfig{Driver: defaultPostgresDriver, DSN: "postgres://tuskbase:secret@localhost:5432/tuskbase?sslmode=disable"}}}); err != nil {
		t.Fatalf("saveUserConfig() error = %v", err)
	}
	cfg, err := loadRuntimeConfig("127.0.0.1:8765", "ignored.db", true, false)
	if err != nil {
		t.Fatalf("loadRuntimeConfig() error = %v", err)
	}
	if cfg.Store.Type != storePostgres || cfg.Store.PostgresDriver != defaultPostgresDriver || !strings.Contains(cfg.Store.PostgresDSN, "localhost:5432") {
		t.Fatalf("runtime store = %+v", cfg.Store)
	}
	if got := cfg.Auth.Name(); got != "local-shared-keys" {
		t.Fatalf("auth policy = %q, want local-shared-keys", got)
	}
}

func TestDoctorReportsIncompleteLocalSharedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	t.Setenv("TUSKBASE_POSTGRES_DSN", "")
	t.Setenv("TUSKBASE_STORE", "")
	if err := saveUserConfig(path, userConfig{Mode: modeLocalShared, Addr: defaultAddr, AgentKeys: []daemon.LocalSharedKey{{Name: "codex", Role: "agent", Key: "secret"}}}); err != nil {
		t.Fatalf("saveUserConfig() error = %v", err)
	}
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"doctor"}, &out, &errb); err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	got := out.String()
	for _, want := range []string{"setup_state: incomplete", "auth-only Local Shared config", "postgres_dsn: missing"} {
		if !strings.Contains(got, want) {
			t.Fatalf("doctor output missing %q: %s", want, got)
		}
	}
}

func TestDoctorReportsPostgresAuthFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	t.Setenv("TUSKBASE_POSTGRES_DSN", "")
	t.Setenv("TUSKBASE_STORE", "")
	oldVerify := verifyPostgresDSN
	verifyPostgresDSN = func(context.Context, string) error {
		return errors.New(`failed SASL auth: FATAL: password authentication failed for user "tuskbase" (SQLSTATE 28P01)`)
	}
	t.Cleanup(func() { verifyPostgresDSN = oldVerify })
	if err := saveUserConfig(path, userConfig{
		Mode:      modeLocalShared,
		Addr:      defaultAddr,
		AgentKeys: []daemon.LocalSharedKey{{Name: "codex", Role: "agent", Key: "secret"}},
		Store: storeConfig{Type: storePostgres, Postgres: &postgresStoreConfig{
			Source: postgresSourceDocker,
			Driver: defaultPostgresDriver,
			DSN:    "postgres://tuskbase:secret@127.0.0.1:8766/tuskbase?sslmode=disable",
			Docker: &dockerPostgresConfig{Host: "127.0.0.1", Port: 8766, Image: defaultDockerPostgresImage},
		}},
	}); err != nil {
		t.Fatalf("saveUserConfig() error = %v", err)
	}
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"doctor"}, &out, &errb); err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	got := out.String()
	for _, want := range []string{"store_runtime: not-ready", "postgres_connect: auth-failed", "existing Docker volume", "tuskbase setup --mode local-basic --yes"} {
		if !strings.Contains(got, want) {
			t.Fatalf("doctor output missing %q: %s", want, got)
		}
	}
}

func TestLoadRuntimeConfigRequiresPostgresDSNForStoredLocalShared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	if err := saveUserConfig(path, userConfig{Mode: modeLocalShared, Addr: defaultAddr, AgentKeys: []daemon.LocalSharedKey{{Name: "codex", Role: "agent", Key: "secret"}}, Store: storeConfig{Type: storePostgres, Postgres: &postgresStoreConfig{Driver: defaultPostgresDriver}}}); err != nil {
		t.Fatalf("saveUserConfig() error = %v", err)
	}
	_, err := loadRuntimeConfig("127.0.0.1:8765", "ignored.db", true, false)
	if err == nil || !strings.Contains(err.Error(), "postgres dsn is required") {
		t.Fatalf("loadRuntimeConfig() error = %v, want postgres dsn requirement", err)
	}
}

func TestLoadEmbeddingProviderSupportsOllama(t *testing.T) {
	t.Setenv("TUSKBASE_EMBEDDING_PROVIDER", "ollama")
	t.Setenv("TUSKBASE_EMBEDDING_MODEL", "all-minilm")
	t.Setenv("TUSKBASE_OLLAMA_BASE_URL", "127.0.0.1:11434")
	emb, err := loadEmbeddingProvider()
	if err != nil {
		t.Fatalf("loadEmbeddingProvider() error = %v", err)
	}
	provider, ok := emb.(*embeddings.OllamaProvider)
	if !ok {
		t.Fatalf("embedding provider = %T, want OllamaProvider", emb)
	}
	if provider.Model != "all-minilm" || provider.BaseURL != "http://127.0.0.1:11434" {
		t.Fatalf("ollama provider = %+v", provider)
	}
}

func TestLoadRuntimeConfigRequiresHTTPAuth(t *testing.T) {
	t.Setenv("TUSKBASE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	_, err := loadRuntimeConfig("127.0.0.1:8765", "tuskbase.db", true, false)
	if err == nil {
		t.Fatal("loadRuntimeConfig() error = nil, want missing auth error")
	}
	if !strings.Contains(err.Error(), "run `tuskbase setup`") {
		t.Fatalf("loadRuntimeConfig() error = %v", err)
	}
}

func TestLoadRuntimeConfigDefaultsToNoAuthForStdio(t *testing.T) {
	t.Setenv("TUSKBASE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	cfg, err := loadRuntimeConfig("127.0.0.1:8765", "tuskbase.db", false, false)
	if err != nil {
		t.Fatalf("loadRuntimeConfig() error = %v", err)
	}
	if got := cfg.Auth.Name(); got != "none" {
		t.Fatalf("auth policy = %q, want none", got)
	}
}

func TestLegacyBothEnablesHTTPMCPAndREST(t *testing.T) {
	args, err := legacyServeArgs("both")
	if err != nil {
		t.Fatalf("legacyServeArgs(both) error = %v", err)
	}
	got := strings.Join(args, " ")
	if got != "--http-mcp --rest" {
		t.Fatalf("legacy both args = %q", got)
	}
}

func TestAuthRotateLocalBasic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"setup", "--mode", "local-basic", "--yes"}, &out, &errb); err != nil {
		t.Fatalf("setup error = %v", err)
	}
	cfg, _, err := loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() error = %v", err)
	}
	oldKey := cfg.APIKey
	out.Reset()
	if err := execute(context.Background(), []string{"auth", "rotate"}, &out, &errb); err != nil {
		t.Fatalf("auth rotate error = %v", err)
	}
	cfg, _, err = loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() after rotate error = %v", err)
	}
	if cfg.APIKey == "" || cfg.APIKey == oldKey {
		t.Fatalf("rotated key = %q old = %q", cfg.APIKey, oldKey)
	}
	if !strings.Contains(out.String(), "rotated: local-api-key") {
		t.Fatalf("rotate output = %q", out.String())
	}
}

func TestAuthKeyAdminLocalShared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"setup", "--mode", "local-shared", "--yes"}, &out, &errb); err != nil {
		t.Fatalf("setup local-shared error = %v", err)
	}
	if err := execute(context.Background(), []string{"auth", "add", "--name", "windsurf", "--role", "reader"}, &out, &errb); err != nil {
		t.Fatalf("auth add error = %v", err)
	}
	cfg, _, err := loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() error = %v", err)
	}
	idx := findAgentKey(cfg.AgentKeys, "windsurf")
	if idx < 0 || cfg.AgentKeys[idx].Role != "reader" {
		t.Fatalf("windsurf key after add = %+v", cfg.AgentKeys)
	}
	oldKey := cfg.AgentKeys[idx].Key
	if err := execute(context.Background(), []string{"auth", "set-role", "--name", "windsurf", "--role", "admin"}, &out, &errb); err != nil {
		t.Fatalf("auth set-role error = %v", err)
	}
	if err := execute(context.Background(), []string{"auth", "rotate", "--name", "windsurf"}, &out, &errb); err != nil {
		t.Fatalf("auth rotate --name error = %v", err)
	}
	cfg, _, err = loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() after rotate error = %v", err)
	}
	idx = findAgentKey(cfg.AgentKeys, "windsurf")
	if idx < 0 || cfg.AgentKeys[idx].Role != "admin" || cfg.AgentKeys[idx].Key == oldKey {
		t.Fatalf("windsurf key after role/rotate = %+v", cfg.AgentKeys)
	}
	if err := execute(context.Background(), []string{"auth", "remove", "--name", "windsurf"}, &out, &errb); err != nil {
		t.Fatalf("auth remove error = %v", err)
	}
	cfg, _, err = loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() after remove error = %v", err)
	}
	if findAgentKey(cfg.AgentKeys, "windsurf") >= 0 {
		t.Fatalf("windsurf key still present = %+v", cfg.AgentKeys)
	}
}

func TestAuthRemoveRejectsFinalLocalSharedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	if err := saveUserConfig(path, userConfig{Mode: modeLocalShared, Addr: defaultAddr, AgentKeys: []daemon.LocalSharedKey{{Name: "solo", Role: "agent", Key: "secret"}}}); err != nil {
		t.Fatalf("saveUserConfig() error = %v", err)
	}
	var out, errb bytes.Buffer
	err := execute(context.Background(), []string{"auth", "remove", "--name", "solo"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "final Local Shared key") {
		t.Fatalf("auth remove error = %v, want final-key rejection", err)
	}
}

func TestLoadUserConfigRepairsPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	if err := os.WriteFile(path, []byte(`{"mode":"local-basic","addr":"127.0.0.1:8765","api_key":"secret","updated_at":"now"}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, _, err := loadUserConfig(); err != nil {
		t.Fatalf("loadUserConfig() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %o, want 600", got)
	}
}

func TestLoadUserConfigRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(target, []byte(`{"mode":"local-basic","addr":"127.0.0.1:8765","api_key":"secret","updated_at":"now"}`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	_, _, err := loadUserConfig()
	if err == nil || !strings.Contains(err.Error(), "symlinked") {
		t.Fatalf("loadUserConfig() error = %v, want symlink rejection", err)
	}
}

func TestConnectRejectsUnknownClient(t *testing.T) {
	var out, errb bytes.Buffer
	err := execute(context.Background(), []string{"connect", "unknown"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "supported clients") {
		t.Fatalf("connect error = %v, want supported clients", err)
	}
}

func TestLocalBasicSetupHTTPMCPSmokeAndRotation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("TUSKBASE_CONFIG_PATH", filepath.Join(root, "config.json"))
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	var out, errb bytes.Buffer
	if err := execute(ctx, []string{"setup", "--mode", "local-basic", "--yes"}, &out, &errb); err != nil {
		t.Fatalf("setup error = %v", err)
	}
	cfg, _, err := loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() error = %v", err)
	}
	oldKey := cfg.APIKey
	runMCPRememberSmoke(t, ctx, oldKey, filepath.Join(root, "first.db"), "local-api-key")

	if err := execute(ctx, []string{"auth", "rotate"}, &out, &errb); err != nil {
		t.Fatalf("auth rotate error = %v", err)
	}
	cfg, _, err = loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() after rotate error = %v", err)
	}
	if cfg.APIKey == oldKey {
		t.Fatal("rotate reused old key")
	}
	assertMCPConnectFails(t, ctx, oldKey, filepath.Join(root, "rotated-old.db"))
	runMCPRememberSmoke(t, ctx, cfg.APIKey, filepath.Join(root, "rotated-new.db"), "local-api-key")
}

func TestLocalSharedSetupHTTPMCPSmoke(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("TUSKBASE_CONFIG_PATH", filepath.Join(root, "config.json"))
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	t.Setenv("TUSKBASE_STORE", "sqlite")
	var out, errb bytes.Buffer
	if err := execute(ctx, []string{"setup", "--mode", "local-shared", "--yes"}, &out, &errb); err != nil {
		t.Fatalf("setup local-shared error = %v", err)
	}
	cfg, _, err := loadUserConfig()
	if err != nil {
		t.Fatalf("loadUserConfig() error = %v", err)
	}
	idx := findAgentKey(cfg.AgentKeys, "codex")
	if idx < 0 {
		t.Fatalf("codex key missing = %+v", cfg.AgentKeys)
	}
	runMCPRememberSmoke(t, ctx, cfg.AgentKeys[idx].Key, filepath.Join(root, "shared.db"), "codex")
}

func TestLocalBasicBridgeMCPSmoke(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("TUSKBASE_CONFIG_PATH", filepath.Join(root, "config.json"))
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	var out, errb bytes.Buffer
	if err := execute(ctx, []string{"setup", "--mode", "local-basic", "--yes"}, &out, &errb); err != nil {
		t.Fatalf("setup local-basic error = %v", err)
	}
	runBridgeRememberSmoke(t, ctx, filepath.Join(root, "bridge-basic.db"), "codex", "local-api-key")
}

func TestLocalSharedBridgeMCPSmoke(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("TUSKBASE_CONFIG_PATH", filepath.Join(root, "config.json"))
	t.Setenv("TUSKBASE_API_KEY", "")
	t.Setenv("TUSKBASE_AGENT_KEYS", "")
	t.Setenv("TUSKBASE_STORE", "sqlite")
	var out, errb bytes.Buffer
	if err := execute(ctx, []string{"setup", "--mode", "local-shared", "--yes"}, &out, &errb); err != nil {
		t.Fatalf("setup local-shared error = %v", err)
	}
	runBridgeRememberSmoke(t, ctx, filepath.Join(root, "bridge-shared.db"), "codex", "codex")
}

func TestLocalSharedBridgeMissingClientFailsClearly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("TUSKBASE_CONFIG_PATH", filepath.Join(root, "config.json"))
	if err := saveUserConfig(filepath.Join(root, "config.json"), userConfig{Mode: modeLocalShared, Addr: defaultAddr, AgentKeys: []daemon.LocalSharedKey{{Name: "codex", Role: "agent", Key: "secret"}}}); err != nil {
		t.Fatalf("saveUserConfig() error = %v", err)
	}
	_, err := NewConfigCredentialProvider(loadUserConfig).Credential(ctx, "cursor")
	if err == nil || !strings.Contains(err.Error(), "tuskbase auth add --name cursor") {
		t.Fatalf("missing client error = %v", err)
	}
}

func TestBridgeDiagnosticServerExposesPostgresAuthFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("TUSKBASE_CONFIG_PATH", filepath.Join(root, "config.json"))
	oldVerify := verifyPostgresDSN
	verifyPostgresDSN = func(context.Context, string) error {
		return errors.New(`failed SASL auth: FATAL: password authentication failed for user "tuskbase" (SQLSTATE 28P01)`)
	}
	t.Cleanup(func() { verifyPostgresDSN = oldVerify })
	cfg := userConfig{
		Mode:   modeLocalShared,
		Addr:   defaultAddr,
		DBPath: filepath.Join(root, "tuskbase.db"),
		Store: storeConfig{Type: storePostgres, Postgres: &postgresStoreConfig{
			Source: postgresSourceDocker,
			Driver: defaultPostgresDriver,
			DSN:    "postgres://tuskbase:secret@127.0.0.1:8766/tuskbase?sslmode=disable",
			Docker: &dockerPostgresConfig{Host: "127.0.0.1", Port: 8766, Image: defaultDockerPostgresImage},
		}},
	}
	if err := saveUserConfig(filepath.Join(root, "config.json"), cfg); err != nil {
		t.Fatalf("saveUserConfig() error = %v", err)
	}
	server := newBridgeDiagnosticServer(ctx, cfg, errors.New("daemon did not become ready"))
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	bridgeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { serverDone <- server.Run(bridgeCtx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "diagnostic-test", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("diagnostic client connect error = %v", err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "tuskbase_diagnostics" {
		t.Fatalf("diagnostic tools = %+v", tools.Tools)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "tuskbase_diagnostics"})
	if err != nil || result.IsError {
		t.Fatalf("diagnostic call result = %v error = %v", result, err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "postgres_connect: auth-failed") {
		t.Fatalf("diagnostic text = %#v", result.Content)
	}
	cancel()
	<-serverDone
}

func runBridgeRememberSmoke(t *testing.T, ctx context.Context, dbPath, clientName, wantActor string) {
	t.Helper()
	rt, err := loadRuntimeConfig("127.0.0.1:8765", dbPath, true, false)
	if err != nil {
		t.Fatalf("loadRuntimeConfig() error = %v", err)
	}
	d, err := newDaemon(ctx, rt)
	if err != nil {
		t.Fatalf("newDaemon() error = %v", err)
	}
	defer d.Close()
	httpServer := httptest.NewServer(d.Handler())
	defer httpServer.Close()
	bridgeServer, closeBridge, err := newBridgeServer(ctx, httpServer.URL+"/mcp", NewConfigCredentialProvider(loadUserConfig), clientName)
	if err != nil {
		t.Fatalf("newBridgeServer() error = %v", err)
	}
	defer closeBridge()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	bridgeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { serverDone <- bridgeServer.Run(bridgeCtx, serverTransport) }()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "bridge-test", Version: "v0.0.1"}, nil)
	session, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("bridge client connect error = %v", err)
	}
	defer session.Close()
	runRememberOverSession(t, ctx, session, wantActor)
	cancel()
	<-serverDone
}

func runMCPRememberSmoke(t *testing.T, ctx context.Context, token, dbPath, wantActor string) {
	t.Helper()
	rt, err := loadRuntimeConfig("127.0.0.1:8765", dbPath, true, false)
	if err != nil {
		t.Fatalf("loadRuntimeConfig() error = %v", err)
	}
	d, err := newDaemon(ctx, rt)
	if err != nil {
		t.Fatalf("newDaemon() error = %v", err)
	}
	defer d.Close()
	server := httptest.NewServer(d.Handler())
	defer server.Close()
	session := connectMCP(t, ctx, server.URL, token)
	defer session.Close()

	runRememberOverSession(t, ctx, session, wantActor)
}

func runRememberOverSession(t *testing.T, ctx context.Context, session *mcp.ClientSession, wantActor string) {
	t.Helper()
	attachResult, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "tuskbase_attach", Arguments: map[string]any{"repo_path": newSmokeRepo(t)}})
	if err != nil || attachResult.IsError {
		t.Fatalf("attach result = %v error = %v", attachResult, err)
	}
	var attached struct {
		Workspace struct {
			ID string `json:"id"`
		} `json:"workspace"`
	}
	decodeStructured(t, attachResult.StructuredContent, &attached)
	rememberResult, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "tuskbase_remember", Arguments: map[string]any{
		"workspace_id": attached.Workspace.ID,
		"type":         "architecture",
		"title":        "Use auth-derived actor",
		"outcome":      "MCP writes can omit actor when authenticated.",
		"confidence":   0.9,
	}})
	if err != nil || rememberResult.IsError {
		t.Fatalf("remember result = %v mcp_error = %v error = %v", rememberResult, rememberResult.GetError(), err)
	}
	var remembered struct {
		Decision struct {
			Actor struct {
				Name string `json:"name"`
			} `json:"actor"`
		} `json:"decision"`
	}
	decodeStructured(t, rememberResult.StructuredContent, &remembered)
	if remembered.Decision.Actor.Name != wantActor {
		t.Fatalf("actor = %q, want %q", remembered.Decision.Actor.Name, wantActor)
	}
}

func assertMCPConnectFails(t *testing.T, ctx context.Context, token, dbPath string) {
	t.Helper()
	rt, err := loadRuntimeConfig("127.0.0.1:8765", dbPath, true, false)
	if err != nil {
		t.Fatalf("loadRuntimeConfig() error = %v", err)
	}
	d, err := newDaemon(ctx, rt)
	if err != nil {
		t.Fatalf("newDaemon() error = %v", err)
	}
	defer d.Close()
	server := httptest.NewServer(d.Handler())
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: testBearerTransport{token: token}}, DisableStandaloneSSE: true}, nil)
	if err == nil {
		clientSession.Close()
		t.Fatal("Connect(old key) error = nil, want failure")
	}
}

func connectMCP(t *testing.T, ctx context.Context, serverURL, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: serverURL + "/mcp", HTTPClient: &http.Client{Transport: testBearerTransport{token: token}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}
	return session
}

func decodeStructured(t *testing.T, content any, out any) {
	t.Helper()
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
}

func newSmokeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Smoke Repo\n\nLocal MCP auth smoke."), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	return root
}

type testBearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t testBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return base.RoundTrip(clone)
}
