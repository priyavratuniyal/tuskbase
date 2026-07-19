package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	storeSQLite            = "sqlite"
	storePostgres          = "postgres"
	defaultPostgresDriver  = "pgx"
	postgresSourceAuto     = "auto"
	postgresSourceDocker   = "docker"
	postgresSourceExisting = "existing"
	postgresSourceSupabase = "supabase"
)

type storeConfig struct {
	Type     string               `json:"type,omitempty"`
	Postgres *postgresStoreConfig `json:"postgres,omitempty"`
}

type postgresStoreConfig struct {
	Source string                `json:"source,omitempty"`
	Driver string                `json:"driver,omitempty"`
	DSN    string                `json:"dsn,omitempty"`
	Docker *dockerPostgresConfig `json:"docker,omitempty"`
}

type dockerPostgresConfig struct {
	Project     string `json:"project,omitempty"`
	ComposePath string `json:"compose_path,omitempty"`
	Context     string `json:"context,omitempty"`
	Service     string `json:"service,omitempty"`
	Image       string `json:"image,omitempty"`
	Host        string `json:"host,omitempty"`
	Port        int    `json:"port,omitempty"`
	Database    string `json:"database,omitempty"`
	User        string `json:"user,omitempty"`
	Volume      string `json:"volume,omitempty"`
}

type runtimeStoreConfig struct {
	Type           string
	SQLitePath     string
	PostgresDriver string
	PostgresDSN    string
}

type setupStoreOptions struct {
	PostgresDSN         string
	PostgresDriver      string
	PostgresSource      string
	DockerPostgresPort  int
	DockerPostgresImage string
	DockerContext       string
	DockerContextSet    bool
	PrintOnly           bool
	ConfigPath          string
}

type setupStoreResult struct {
	DockerPostgres *dockerPostgresProvisionResult
}

type storeRuntimeCheck struct {
	Checked      bool
	Ready        bool
	Status       string
	Error        string
	RepairHint   string
	FallbackHint string
}

func applySetupStoreConfig(cfg *userConfig, opts setupStoreOptions) (setupStoreResult, error) {
	switch cfg.Mode {
	case modeLocalBasic:
		cfg.Store = storeConfig{Type: storeSQLite}
		return setupStoreResult{}, nil
	case modeLocalShared:
		pg := postgresConfigForSetup(cfg.Store.Postgres, opts.PostgresDSN, opts.PostgresDriver)
		source, err := resolvePostgresSource(opts.PostgresSource, cfg.Store.Postgres, pg, opts.PostgresDSN)
		if err != nil {
			return setupStoreResult{}, err
		}
		pg.Source = source
		switch source {
		case postgresSourceDocker:
			provisioned, err := provisionDockerPostgresForSetup(pg, opts)
			if err != nil {
				return setupStoreResult{}, err
			}
			pg.DSN = provisioned.DSN
			pg.Docker = &provisioned.Config
			cfg.Store = storeConfig{Type: storePostgres, Postgres: &pg}
			return setupStoreResult{DockerPostgres: &provisioned}, nil
		case postgresSourceExisting, postgresSourceSupabase:
			if strings.TrimSpace(pg.DSN) == "" {
				return setupStoreResult{}, fmt.Errorf("postgres dsn is required for Local Shared source %q; pass --postgres-dsn or set TUSKBASE_POSTGRES_DSN", source)
			}
			pg.Docker = nil
		default:
			return setupStoreResult{}, fmt.Errorf("unsupported postgres source %q", source)
		}
		cfg.Store = storeConfig{Type: storePostgres, Postgres: &pg}
		return setupStoreResult{}, nil
	default:
		return setupStoreResult{}, fmt.Errorf("unsupported setup mode %q", cfg.Mode)
	}
}

func postgresConfigForSetup(existing *postgresStoreConfig, dsnFlag, driverFlag string) postgresStoreConfig {
	pg := postgresStoreConfig{}
	if existing != nil {
		pg = *existing
	}
	if dsn := strings.TrimSpace(dsnFlag); dsn != "" {
		pg.DSN = dsn
	} else if strings.TrimSpace(pg.DSN) == "" {
		pg.DSN = strings.TrimSpace(os.Getenv("TUSKBASE_POSTGRES_DSN"))
	}
	if driver := strings.TrimSpace(driverFlag); driver != "" {
		pg.Driver = driver
	} else if strings.TrimSpace(pg.Driver) == "" {
		pg.Driver = strings.TrimSpace(os.Getenv("TUSKBASE_POSTGRES_DRIVER"))
	}
	if strings.TrimSpace(pg.Driver) == "" {
		pg.Driver = defaultPostgresDriver
	}
	return pg
}

func resolvePostgresSource(sourceFlag string, existing *postgresStoreConfig, pg postgresStoreConfig, dsnFlag string) (string, error) {
	source, err := normalizePostgresSource(sourceFlag)
	if err != nil {
		return "", err
	}
	if source != postgresSourceAuto {
		return source, nil
	}
	if strings.TrimSpace(dsnFlag) != "" || strings.TrimSpace(os.Getenv("TUSKBASE_POSTGRES_DSN")) != "" {
		if existing != nil && existing.Source == postgresSourceSupabase {
			return postgresSourceSupabase, nil
		}
		return postgresSourceExisting, nil
	}
	if existing != nil && strings.TrimSpace(pg.DSN) != "" {
		switch existing.Source {
		case postgresSourceDocker, postgresSourceExisting, postgresSourceSupabase:
			return existing.Source, nil
		default:
			return postgresSourceExisting, nil
		}
	}
	return postgresSourceDocker, nil
}

func normalizePostgresSource(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", postgresSourceAuto:
		return postgresSourceAuto, nil
	case postgresSourceDocker:
		return postgresSourceDocker, nil
	case postgresSourceExisting, "postgres":
		return postgresSourceExisting, nil
	case postgresSourceSupabase:
		return postgresSourceSupabase, nil
	default:
		return "", fmt.Errorf("unknown postgres source %q; expected auto, docker, or existing", value)
	}
}

func runtimeStoreConfigFromUserConfig(cfg userConfig) runtimeStoreConfig {
	store := runtimeStoreConfig{Type: defaultStoreTypeForMode(cfg.Mode), SQLitePath: cfg.DBPath, PostgresDriver: defaultPostgresDriver}
	if cfg.Store.Type != "" {
		store.Type = cfg.Store.Type
	}
	if cfg.Store.Postgres != nil {
		store.PostgresDriver = cfg.Store.Postgres.Driver
		store.PostgresDSN = cfg.Store.Postgres.DSN
	}
	if strings.TrimSpace(store.SQLitePath) == "" {
		store.SQLitePath = defaultDBPath()
	}
	if strings.TrimSpace(store.PostgresDriver) == "" {
		store.PostgresDriver = defaultPostgresDriver
	}
	return store
}

func loadRuntimeStoreConfig(dbPath string) (runtimeStoreConfig, error) {
	store := runtimeStoreConfig{Type: storeSQLite, SQLitePath: dbPath, PostgresDriver: defaultPostgresDriver}
	if cfg, found, err := loadUserConfig(); err != nil {
		return runtimeStoreConfig{}, err
	} else if found {
		store.Type = defaultStoreTypeForMode(cfg.Mode)
		if cfg.Store.Type != "" {
			store.Type = cfg.Store.Type
		}
		if cfg.Store.Postgres != nil {
			store.PostgresDriver = cfg.Store.Postgres.Driver
			store.PostgresDSN = cfg.Store.Postgres.DSN
		}
	}
	if envStore := strings.TrimSpace(os.Getenv("TUSKBASE_STORE")); envStore != "" {
		store.Type = envStore
	}
	if envDriver := strings.TrimSpace(os.Getenv("TUSKBASE_POSTGRES_DRIVER")); envDriver != "" {
		store.PostgresDriver = envDriver
	}
	if envDSN := strings.TrimSpace(os.Getenv("TUSKBASE_POSTGRES_DSN")); envDSN != "" {
		store.PostgresDSN = envDSN
		if strings.TrimSpace(os.Getenv("TUSKBASE_STORE")) == "" {
			store.Type = storePostgres
		}
	}
	storeType, err := normalizeStoreType(store.Type)
	if err != nil {
		return runtimeStoreConfig{}, err
	}
	store.Type = storeType
	if strings.TrimSpace(store.SQLitePath) == "" {
		store.SQLitePath = defaultDBPath()
	}
	if strings.TrimSpace(store.PostgresDriver) == "" {
		store.PostgresDriver = defaultPostgresDriver
	}
	if store.Type == storePostgres && strings.TrimSpace(store.PostgresDSN) == "" {
		return store, errors.New("postgres dsn is required for Local Shared; set TUSKBASE_POSTGRES_DSN or run `tuskbase setup --mode local-shared --postgres-dsn <dsn>`")
	}
	return store, nil
}

func defaultStoreTypeForMode(mode string) string {
	switch mode {
	case modeLocalShared:
		return storePostgres
	default:
		return storeSQLite
	}
}

func normalizeStoreType(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", storeSQLite:
		return storeSQLite, nil
	case storePostgres, "pg":
		return storePostgres, nil
	default:
		return "", fmt.Errorf("unknown store %q; expected sqlite or postgres", value)
	}
}

func hasPostgresDSN(cfg userConfig) bool {
	return cfg.Store.Type == storePostgres && cfg.Store.Postgres != nil && strings.TrimSpace(cfg.Store.Postgres.DSN) != ""
}

func isDockerManagedLocalShared(cfg userConfig) bool {
	return cfg.Mode == modeLocalShared &&
		cfg.Store.Type == storePostgres &&
		cfg.Store.Postgres != nil &&
		cfg.Store.Postgres.Source == postgresSourceDocker &&
		cfg.Store.Postgres.Docker != nil
}

func checkRuntimeStore(ctx context.Context, cfg userConfig, store runtimeStoreConfig) storeRuntimeCheck {
	if store.Type != storePostgres || strings.TrimSpace(store.PostgresDSN) == "" {
		return storeRuntimeCheck{}
	}
	err := verifyPostgresDSN(ctx, store.PostgresDSN)
	if err == nil {
		return storeRuntimeCheck{Checked: true, Ready: true, Status: "ok"}
	}
	check := storeRuntimeCheck{Checked: true, Ready: false, Status: "connect-failed", Error: err.Error()}
	if isPostgresAuthError(err) {
		check.Status = "auth-failed"
	}
	if isDockerManagedLocalShared(cfg) {
		switch check.Status {
		case "auth-failed":
			check.RepairHint = "Docker Postgres is running but rejected the configured Tuskbase password; an existing Docker volume may have an older password. Rerun `tuskbase setup --mode local-shared --yes` with the current binary to reconcile the Tuskbase-owned Docker password, or provide the existing password with `--postgres-source existing --postgres-dsn <dsn>`."
		default:
			check.RepairHint = "Start Docker Desktop or Docker Engine, confirm the Local Shared Postgres container is running, then run `tuskbase daemon restart`."
		}
		check.FallbackHint = "Use `tuskbase setup --mode local-basic --yes` to switch this machine back to SQLite without deleting the Local Shared Docker volume."
		return check
	}
	check.RepairHint = "Check the configured Postgres DSN, credentials, network access, and pgvector-enabled database, then run `tuskbase daemon restart`."
	check.FallbackHint = "Use `tuskbase setup --mode local-basic --yes` for a single-machine SQLite setup."
	return check
}

func secretStatus(value string) string {
	if strings.TrimSpace(value) == "" {
		return "missing"
	}
	return "configured"
}

func printStoreSummary(w io.Writer, cfg userConfig) {
	p := newPresenter(w)
	switch cfg.Store.Type {
	case storePostgres:
		driver := defaultPostgresDriver
		source := ""
		if cfg.Store.Postgres != nil && strings.TrimSpace(cfg.Store.Postgres.Driver) != "" {
			driver = cfg.Store.Postgres.Driver
		}
		if cfg.Store.Postgres != nil {
			source = cfg.Store.Postgres.Source
		}
		p.KV("store", storePostgres)
		if strings.TrimSpace(source) != "" {
			p.KV("postgres_source", source)
		}
		p.KV("postgres_driver", driver)
		if hasPostgresDSN(cfg) {
			p.KV("postgres_dsn", "configured")
		} else {
			p.KV("postgres_dsn", "missing (set TUSKBASE_POSTGRES_DSN or rerun setup with --postgres-dsn)")
		}
		if cfg.Store.Postgres != nil && cfg.Store.Postgres.Docker != nil {
			docker := cfg.Store.Postgres.Docker
			p.KV("docker_postgres_project", docker.Project)
			if strings.TrimSpace(docker.Context) != "" {
				p.KV("docker_context", docker.Context)
			}
			p.KV("docker_postgres_image", docker.Image)
			p.KV("docker_postgres_port", fmt.Sprintf("%d", docker.Port))
			p.KV("docker_compose", docker.ComposePath)
		}
	default:
		p.KV("store", storeSQLite)
		p.KV("db_path", cfg.DBPath)
	}
}
