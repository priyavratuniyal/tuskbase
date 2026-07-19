package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/priyavratuniyal/tuskbase/internal/daemon"
)

var defaultTestLifecycle = &testLifecycleController{
	result: lifecycleResult{Backend: "test", State: "running", LogPath: "test.log"},
	status: lifecycleStatus{Backend: "test", State: "running", Autostart: "enabled", LogPath: "test.log", Health: &daemon.Health{Status: "ok", MCP: true, REST: false, AuthPolicy: "local-api-key", AuthSource: "config"}},
}

func TestMain(m *testing.M) {
	newLifecycleController = func() lifecycleController { return defaultTestLifecycle }
	newDockerPostgresProvisioner = func() dockerPostgresProvisioner { return testDockerPostgresProvisioner{} }
	_ = os.Setenv("TUSKBASE_BACKUP_AUTO", "false")
	os.Exit(m.Run())
}

type testDockerPostgresProvisioner struct{}

func (testDockerPostgresProvisioner) Provision(_ context.Context, req dockerPostgresProvisionRequest) (dockerPostgresProvisionResult, error) {
	return dockerPostgresProvisionResult{Config: req.Config, DSN: postgresDSN(req.Config.Host, req.Config.Port, req.Config.User, req.Password, req.Config.Database), Ready: true, Detail: "ready"}, nil
}

func TestLifecycleHealthyDaemonSkipsStart(t *testing.T) {
	services := &testServiceManager{backend: "test"}
	controller := &defaultLifecycleController{
		client:      healthClient(func() (*http.Response, error) { return healthResponse(daemon.Health{Status: "ok", MCP: true}), nil }),
		services:    services,
		acquireLock: immediateLock,
		startDetached: func(context.Context, userConfig, string) error {
			t.Fatal("detached start should not run for healthy daemon")
			return nil
		},
		readyTimeout: 100 * time.Millisecond,
		pollPeriod:   time.Millisecond,
		logPath:      func() string { return "test.log" },
	}
	if err := controller.EnsureReady(context.Background(), testLocalConfig()); err != nil {
		t.Fatalf("EnsureReady() error = %v", err)
	}
	if got := atomic.LoadInt32(&services.startCalls); got != 0 {
		t.Fatalf("Start calls = %d, want 0", got)
	}
}

func TestLifecycleDownDaemonStartsAndWaits(t *testing.T) {
	services := &testServiceManager{backend: "test"}
	controller := &defaultLifecycleController{
		client: healthClient(func() (*http.Response, error) {
			if atomic.LoadInt32(&services.startCalls) == 0 {
				return nil, errors.New("connection refused")
			}
			return healthResponse(daemon.Health{Status: "ok", MCP: true}), nil
		}),
		services:      services,
		acquireLock:   immediateLock,
		startDetached: func(context.Context, userConfig, string) error { return nil },
		readyTimeout:  250 * time.Millisecond,
		pollPeriod:    time.Millisecond,
		logPath:       func() string { return "test.log" },
	}
	if err := controller.EnsureReady(context.Background(), testLocalConfig()); err != nil {
		t.Fatalf("EnsureReady() error = %v", err)
	}
	if got := atomic.LoadInt32(&services.startCalls); got != 1 {
		t.Fatalf("Start calls = %d, want 1", got)
	}
}

func TestLifecycleServiceStartAcceptedThenDetachedFallback(t *testing.T) {
	services := &testServiceManager{backend: "test"}
	var detachedCalls int32
	controller := &defaultLifecycleController{
		client: healthClient(func() (*http.Response, error) {
			if atomic.LoadInt32(&detachedCalls) == 0 {
				return nil, errors.New("connection refused")
			}
			return healthResponse(daemon.Health{Status: "ok", MCP: true}), nil
		}),
		services:    services,
		acquireLock: immediateLock,
		startDetached: func(context.Context, userConfig, string) error {
			atomic.AddInt32(&detachedCalls, 1)
			return nil
		},
		readyTimeout: 10 * time.Millisecond,
		pollPeriod:   time.Millisecond,
		logPath:      func() string { return "test.log" },
	}
	if err := controller.EnsureReady(context.Background(), testLocalConfig()); err != nil {
		t.Fatalf("EnsureReady() error = %v", err)
	}
	if got := atomic.LoadInt32(&services.startCalls); got != 1 {
		t.Fatalf("Start calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&detachedCalls); got != 1 {
		t.Fatalf("detached calls = %d, want 1", got)
	}
}

func TestLifecycleBadHealthAfterServiceStartDoesNotSpawnFallback(t *testing.T) {
	services := &testServiceManager{backend: "test"}
	var detachedCalls int32
	controller := &defaultLifecycleController{
		client: healthClient(func() (*http.Response, error) {
			if atomic.LoadInt32(&services.startCalls) == 0 {
				return nil, errors.New("connection refused")
			}
			return healthResponse(daemon.Health{Status: "ok", MCP: false}), nil
		}),
		services:    services,
		acquireLock: immediateLock,
		startDetached: func(context.Context, userConfig, string) error {
			atomic.AddInt32(&detachedCalls, 1)
			return nil
		},
		readyTimeout: 50 * time.Millisecond,
		pollPeriod:   time.Millisecond,
		logPath:      func() string { return "test.log" },
	}
	err := controller.EnsureReady(context.Background(), testLocalConfig())
	if err == nil || !strings.Contains(err.Error(), "MCP surface is disabled") {
		t.Fatalf("EnsureReady() error = %v, want MCP-disabled health error", err)
	}
	if got := atomic.LoadInt32(&detachedCalls); got != 0 {
		t.Fatalf("detached calls = %d, want 0", got)
	}
}

func TestLifecycleTimeoutIncludesLogPath(t *testing.T) {
	services := &testServiceManager{backend: "test"}
	controller := &defaultLifecycleController{
		client:        healthClient(func() (*http.Response, error) { return nil, errors.New("connection refused") }),
		services:      services,
		acquireLock:   immediateLock,
		startDetached: func(context.Context, userConfig, string) error { return nil },
		readyTimeout:  5 * time.Millisecond,
		pollPeriod:    time.Millisecond,
		logPath:       func() string { return "/tmp/tuskbase-test.log" },
	}
	err := controller.EnsureReady(context.Background(), testLocalConfig())
	if err == nil || !strings.Contains(err.Error(), "/tmp/tuskbase-test.log") {
		t.Fatalf("EnsureReady() error = %v, want log path", err)
	}
}

func TestLifecycleConcurrentStartsUseOneLock(t *testing.T) {
	services := &testServiceManager{backend: "test"}
	lockPath := filepath.Join(t.TempDir(), "daemon.lock")
	controller := &defaultLifecycleController{
		client: healthClient(func() (*http.Response, error) {
			if atomic.LoadInt32(&services.startCalls) == 0 {
				return nil, errors.New("connection refused")
			}
			return healthResponse(daemon.Health{Status: "ok", MCP: true}), nil
		}),
		services: services,
		acquireLock: func(ctx context.Context) (lifecycleLock, error) {
			return acquireFileLifecycleLock(ctx, lockPath, time.Second)
		},
		startDetached: func(context.Context, userConfig, string) error { return nil },
		readyTimeout:  time.Second,
		pollPeriod:    time.Millisecond,
		logPath:       func() string { return "test.log" },
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- controller.EnsureReady(context.Background(), testLocalConfig())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureReady() error = %v", err)
		}
	}
	if got := atomic.LoadInt32(&services.startCalls); got != 1 {
		t.Fatalf("Start calls = %d, want 1", got)
	}
}

func TestSetupInstallsAutostartForLocalModes(t *testing.T) {
	for _, mode := range []string{modeLocalBasic, modeLocalShared} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			t.Setenv("TUSKBASE_CONFIG_PATH", path)
			controller := &testLifecycleController{result: lifecycleResult{Backend: "test", State: "running", LogPath: "test.log"}}
			withLifecycle(t, controller)
			var out, errb bytes.Buffer
			args := []string{"setup", "--mode", mode, "--yes"}
			if mode == modeLocalShared {
				args = append(args, "--postgres-dsn", "postgres://tuskbase:secret@localhost:5432/tuskbase?sslmode=disable")
			}
			if err := execute(context.Background(), args, &out, &errb); err != nil {
				t.Fatalf("setup error = %v", err)
			}
			if got := atomic.LoadInt32(&controller.installCalls); got != 1 {
				t.Fatalf("InstallAndStart calls = %d, want 1; output=%s", got, out.String())
			}
			cfg, _, err := loadUserConfig()
			if err != nil {
				t.Fatalf("loadUserConfig() error = %v", err)
			}
			if !cfg.Daemon.MCPEnabled || cfg.Daemon.RESTEnabled || !cfg.Daemon.AutostartEnabled {
				t.Fatalf("daemon defaults = %+v", cfg.Daemon)
			}
		})
	}
}

func TestSetupLocalSharedExistingPostgresWithoutDSNFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	controller := &testLifecycleController{result: lifecycleResult{Backend: "test", State: "running"}}
	withLifecycle(t, controller)
	var out, errb bytes.Buffer
	err := execute(context.Background(), []string{"setup", "--mode", "local-shared", "--postgres-source", "existing", "--yes"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "postgres dsn is required") {
		t.Fatalf("setup local-shared error = %v, want postgres dsn requirement", err)
	}
	if got := atomic.LoadInt32(&controller.installCalls); got != 0 {
		t.Fatalf("InstallAndStart calls = %d, want 0", got)
	}
}

func TestSetupRejectsDemoMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	controller := &testLifecycleController{result: lifecycleResult{Backend: "test", State: "running"}}
	withLifecycle(t, controller)
	var out, errb bytes.Buffer
	err := execute(context.Background(), []string{"setup", "--mode", "demo", "--yes"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "unknown setup mode") {
		t.Fatalf("setup demo error = %v, want unsupported mode", err)
	}
	if got := atomic.LoadInt32(&controller.installCalls); got != 0 {
		t.Fatalf("InstallAndStart calls = %d, want 0", got)
	}
}

func TestSetupAutostartFailureIsNonFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	controller := &testLifecycleController{result: lifecycleResult{Backend: "unsupported", State: "degraded", Degraded: true, Err: errServiceUnsupported, Detail: "bridge fallback"}}
	withLifecycle(t, controller)
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"setup", "--mode", "local-basic", "--yes"}, &out, &errb); err != nil {
		t.Fatalf("setup error = %v", err)
	}
	if !strings.Contains(out.String(), "service: degraded") || !strings.Contains(out.String(), "bridge fallback") {
		t.Fatalf("setup output = %q", out.String())
	}
}

func TestDaemonAdminCommandsReturnErrorsForDegradedResults(t *testing.T) {
	for _, command := range []string{"install", "restart", "uninstall"} {
		t.Run(command, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			t.Setenv("TUSKBASE_CONFIG_PATH", path)
			if err := saveUserConfig(path, testLocalConfig()); err != nil {
				t.Fatalf("saveUserConfig() error = %v", err)
			}
			controller := &testLifecycleController{result: lifecycleResult{Backend: "test", State: "degraded", Degraded: true, Err: errors.New("service failed"), Detail: "service failed"}}
			withLifecycle(t, controller)
			var out, errb bytes.Buffer
			err := execute(context.Background(), []string{"daemon", command}, &out, &errb)
			if err == nil || !strings.Contains(err.Error(), "service failed") {
				t.Fatalf("daemon %s error = %v, output=%q", command, err, out.String())
			}
			if !strings.Contains(out.String(), "service: degraded") {
				t.Fatalf("daemon %s output = %q", command, out.String())
			}
		})
	}
}

func TestDaemonAdminCommandReturnsErrorForDegradedResultWithoutCause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	if err := saveUserConfig(path, testLocalConfig()); err != nil {
		t.Fatalf("saveUserConfig() error = %v", err)
	}
	controller := &testLifecycleController{result: lifecycleResult{Backend: "test", State: "degraded", Degraded: true, Detail: "readiness timed out"}}
	withLifecycle(t, controller)
	var out, errb bytes.Buffer
	err := execute(context.Background(), []string{"daemon", "restart"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "degraded") {
		t.Fatalf("daemon restart error = %v, output=%q", err, out.String())
	}
}

func TestDaemonStopUsesLifecycleStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TUSKBASE_CONFIG_PATH", path)
	if err := saveUserConfig(path, testLocalConfig()); err != nil {
		t.Fatalf("saveUserConfig() error = %v", err)
	}
	controller := &testLifecycleController{result: lifecycleResult{Backend: "test", State: "stopped"}}
	withLifecycle(t, controller)
	var out, errb bytes.Buffer
	if err := execute(context.Background(), []string{"daemon", "stop"}, &out, &errb); err != nil {
		t.Fatalf("daemon stop error = %v", err)
	}
	if got := atomic.LoadInt32(&controller.stopCalls); got != 1 {
		t.Fatalf("Stop calls = %d, want 1", got)
	}
	if !strings.Contains(out.String(), "service: stopped") {
		t.Fatalf("daemon stop output = %q", out.String())
	}
}

func TestPlatformRenderersContainDaemonSurfacesAndNoSecrets(t *testing.T) {
	cfg := testLocalConfig()
	cfg.APIKey = "tbk_secret"
	cfg.DBPath = "/tmp/tuskbase.db"
	cfg.Addr = "127.0.0.1:8765"
	rendered := []string{
		renderSystemdUnit("/usr/local/bin/tuskbase", cfg),
		renderLaunchAgentPlist("/usr/local/bin/tuskbase", cfg),
		strings.Join(renderWindowsScheduledTaskCommand(`C:\\Program Files\\Tuskbase\\tuskbase.exe`, cfg), " "),
	}
	for _, got := range rendered {
		for _, want := range []string{"tuskbase", "serve", "--http-mcp", "--addr", "127.0.0.1:8765", "--db", "/tmp/tuskbase.db"} {
			if !strings.Contains(got, want) {
				t.Fatalf("rendered service missing %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "--rest") {
			t.Fatalf("rendered service mounted REST by default:\n%s", got)
		}
		if strings.Contains(got, "tbk_secret") {
			t.Fatalf("rendered service leaked secret:\n%s", got)
		}
	}
}

func TestValidateServiceExecutablePathStable(t *testing.T) {
	exe := writeTestExecutable(t, filepath.Join(t.TempDir(), "tuskbase"))
	got, err := validateServiceExecutablePath(exe)
	if err != nil {
		t.Fatalf("validateServiceExecutablePath() error = %v", err)
	}
	want, err := filepath.Abs(exe)
	if err != nil {
		t.Fatalf("Abs() error = %v", err)
	}
	if got != want {
		t.Fatalf("validateServiceExecutablePath() = %q, want %q", got, want)
	}
}

func TestValidateServiceExecutablePathRejectsGoBuildTempPath(t *testing.T) {
	exe := writeTestExecutable(t, filepath.Join(t.TempDir(), "go-build123", "b001", "exe", "tuskbase"))
	_, err := validateServiceExecutablePath(exe)
	if err == nil || !strings.Contains(err.Error(), "temporary Go build artifact") {
		t.Fatalf("validateServiceExecutablePath() error = %v, want go-build rejection", err)
	}
}

func TestValidateServiceExecutablePathRejectsMissingPath(t *testing.T) {
	_, err := validateServiceExecutablePath(filepath.Join(t.TempDir(), "tuskbase"))
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("validateServiceExecutablePath() error = %v, want missing-path rejection", err)
	}
}

func TestServiceRenderersUseResolvedExecutable(t *testing.T) {
	exe := writeTestExecutable(t, filepath.Join(t.TempDir(), "tuskbase"))
	oldExecutable := currentExecutable
	currentExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { currentExecutable = oldExecutable })
	cfg := testLocalConfig()
	systemdUnit, err := renderSystemdUnitForService(cfg)
	if err != nil {
		t.Fatalf("renderSystemdUnitForService() error = %v", err)
	}
	launchAgent, err := renderLaunchAgentPlistForService(cfg)
	if err != nil {
		t.Fatalf("renderLaunchAgentPlistForService() error = %v", err)
	}
	windowsTask, err := renderWindowsScheduledTaskCommandForService(cfg)
	if err != nil {
		t.Fatalf("renderWindowsScheduledTaskCommandForService() error = %v", err)
	}
	for _, got := range []string{systemdUnit, launchAgent, strings.Join(windowsTask, " ")} {
		if !strings.Contains(got, exe) {
			t.Fatalf("rendered service missing resolved executable %q:\n%s", exe, got)
		}
	}
}

func TestStartDetachedDaemonHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := startDetachedDaemon(ctx, testLocalConfig(), filepath.Join(t.TempDir(), "daemon.log"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("startDetachedDaemon() error = %v, want context.Canceled", err)
	}
}

func withLifecycle(t *testing.T, controller lifecycleController) {
	t.Helper()
	old := newLifecycleController
	newLifecycleController = func() lifecycleController { return controller }
	t.Cleanup(func() { newLifecycleController = old })
}

func writeTestExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir executable dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
}

func testLocalConfig() userConfig {
	cfg := userConfig{Mode: modeLocalBasic, Addr: "127.0.0.1:8765", DBPath: "test.db", APIKey: "secret"}
	applyDaemonDefaults(&cfg)
	return cfg
}

type testLifecycleController struct {
	installCalls int32
	ensureCalls  int32
	stopCalls    int32
	result       lifecycleResult
	status       lifecycleStatus
	ensureErr    error
}

func (c *testLifecycleController) EnsureReady(context.Context, userConfig) error {
	atomic.AddInt32(&c.ensureCalls, 1)
	return c.ensureErr
}

func (c *testLifecycleController) InstallAndStart(context.Context, userConfig) lifecycleResult {
	atomic.AddInt32(&c.installCalls, 1)
	return c.result
}

func (c *testLifecycleController) Uninstall(context.Context, userConfig) lifecycleResult {
	return c.result
}
func (c *testLifecycleController) Restart(context.Context, userConfig) lifecycleResult {
	return c.result
}
func (c *testLifecycleController) Stop(context.Context, userConfig) lifecycleResult {
	atomic.AddInt32(&c.stopCalls, 1)
	return c.result
}
func (c *testLifecycleController) Status(context.Context, userConfig) lifecycleStatus {
	return c.status
}

type testServiceManager struct {
	backend    string
	startCalls int32
}

func (m *testServiceManager) Backend() string                             { return m.backend }
func (m *testServiceManager) Install(context.Context, userConfig) error   { return nil }
func (m *testServiceManager) Uninstall(context.Context, userConfig) error { return nil }
func (m *testServiceManager) Start(context.Context, userConfig) error {
	atomic.AddInt32(&m.startCalls, 1)
	return nil
}
func (m *testServiceManager) Restart(context.Context, userConfig) error {
	return m.Start(context.Background(), userConfig{})
}
func (m *testServiceManager) Stop(context.Context, userConfig) error { return nil }
func (m *testServiceManager) Status(context.Context, userConfig) serviceStatus {
	return serviceStatus{Backend: m.backend, State: "test", Autostart: "test"}
}

type noopLock struct{}

func immediateLock(context.Context) (lifecycleLock, error) { return noopLock{}, nil }
func (noopLock) Unlock() error                             { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func healthClient(fn func() (*http.Response, error)) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return fn() })}
}

func healthResponse(health daemon.Health) *http.Response {
	if health.AuthPolicy == "" {
		health.AuthPolicy = "local-api-key"
	}
	var body bytes.Buffer
	_, _ = fmt.Fprintf(&body, `{"status":%q,"mcp":%t,"rest":%t,"auth_policy":%q}`, health.Status, health.MCP, health.REST, health.AuthPolicy)
	return &http.Response{StatusCode: http.StatusOK, Body: ioNopCloser{bytes.NewReader(body.Bytes())}, Header: make(http.Header)}
}

type ioNopCloser struct{ *bytes.Reader }

func (c ioNopCloser) Close() error { return nil }
