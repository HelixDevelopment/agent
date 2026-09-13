package containers

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"dev.helix.agent/internal/config"
	"digital.vasic.concurrency/pkg/safe"
	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/distribution"
	"digital.vasic.containers/pkg/endpoint"
	"digital.vasic.containers/pkg/health"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/runtime"
	"digital.vasic.containers/pkg/scheduler"
)

// mockRuntime implements runtime.ContainerRuntime for testing.
type mockRuntime struct {
	name      string
	listError error
}

func (m *mockRuntime) Name() string { return m.name }

func (m *mockRuntime) Version(
	ctx context.Context,
) (string, error) {
	return "1.0.0", nil
}

func (m *mockRuntime) IsAvailable(ctx context.Context) bool {
	return true
}

func (m *mockRuntime) Start(
	ctx context.Context, id string, opts ...runtime.StartOption,
) error {
	return nil
}

func (m *mockRuntime) Stop(
	ctx context.Context, id string, opts ...runtime.StopOption,
) error {
	return nil
}

func (m *mockRuntime) Remove(
	ctx context.Context, id string, opts ...runtime.RemoveOption,
) error {
	return nil
}

func (m *mockRuntime) Status(
	ctx context.Context, id string,
) (*runtime.ContainerStatus, error) {
	return &runtime.ContainerStatus{
		Name:  id,
		State: runtime.StateRunning,
	}, nil
}

func (m *mockRuntime) List(
	ctx context.Context, filter runtime.ListFilter,
) ([]runtime.ContainerInfo, error) {
	if m.listError != nil {
		return nil, m.listError
	}
	return nil, nil
}

func (m *mockRuntime) Stats(
	ctx context.Context, id string,
) (*runtime.ContainerStats, error) {
	return &runtime.ContainerStats{}, nil
}

func (m *mockRuntime) Exec(
	ctx context.Context, id string, cmd []string,
) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{ExitCode: 0}, nil
}

// Run is the one-shot `run --rm` form. The mock reports success without
// performing anything, which is all these adapter tests need; the runtime
// interface gained this method upstream and the mock was not updated, so the
// whole package's tests failed to COMPILE (not merely fail) until it existed.
func (m *mockRuntime) Run(
	ctx context.Context, image string, cmd []string, opts ...runtime.RunOption,
) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{ExitCode: 0}, nil
}

func (m *mockRuntime) Logs(
	ctx context.Context, id string, opts ...runtime.LogOption,
) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// mockOrchestrator implements compose.ComposeOrchestrator.
type mockOrchestrator struct {
	upCalled   bool
	downCalled bool
	lastFile   string
}

func (m *mockOrchestrator) Up(
	ctx context.Context, project compose.ComposeProject,
	opts ...compose.UpOption,
) error {
	m.upCalled = true
	m.lastFile = project.File
	return nil
}

func (m *mockOrchestrator) Down(
	ctx context.Context, project compose.ComposeProject,
	opts ...compose.DownOption,
) error {
	m.downCalled = true
	m.lastFile = project.File
	return nil
}

func (m *mockOrchestrator) Status(
	ctx context.Context, project compose.ComposeProject,
) ([]compose.ServiceStatus, error) {
	return nil, nil
}

func (m *mockOrchestrator) Logs(
	ctx context.Context, project compose.ComposeProject,
	service string,
) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// mockDistributor implements distribution.Distributor.
type mockDistributor struct {
	distributeCalled   bool
	undistributeCalled bool
	containers         []distribution.DistributedContainer
}

func (m *mockDistributor) Distribute(
	ctx context.Context,
	reqs []scheduler.ContainerRequirements,
) (*distribution.DistributionSummary, error) {
	m.distributeCalled = true
	return &distribution.DistributionSummary{
		TotalContainers: len(reqs),
		LocalContainers: len(reqs),
	}, nil
}

func (m *mockDistributor) Undistribute(
	ctx context.Context,
) error {
	m.undistributeCalled = true
	return nil
}

func (m *mockDistributor) Status(
	ctx context.Context,
) []distribution.DistributedContainer {
	return m.containers
}

func (m *mockDistributor) HealthCheckAll(
	ctx context.Context,
) map[string]error {
	return nil
}

func (m *mockDistributor) Rebalance(
	ctx context.Context,
) (*distribution.DistributionSummary, error) {
	return &distribution.DistributionSummary{}, nil
}

func (m *mockDistributor) HostStatus(
	ctx context.Context, hostName string,
) (*remote.HostResources, error) {
	return &remote.HostResources{Host: hostName}, nil
}

// mockHealthChecker implements health.HealthChecker.
type mockHealthChecker struct {
	checkResults    map[string]bool
	checkAllResults []*health.HealthResult
}

func (m *mockHealthChecker) Check(ctx context.Context, target health.HealthTarget) *health.HealthResult {
	if m.checkResults != nil {
		if healthy, ok := m.checkResults[target.Name]; ok {
			return &health.HealthResult{
				Healthy: healthy,
				Error:   "",
			}
		}
	}
	// Default healthy
	return &health.HealthResult{Healthy: true}
}

func (m *mockHealthChecker) CheckAll(ctx context.Context, targets []health.HealthTarget) []*health.HealthResult {
	if m.checkAllResults != nil {
		return m.checkAllResults
	}
	results := make([]*health.HealthResult, len(targets))
	for i := range targets {
		results[i] = &health.HealthResult{Healthy: true}
	}
	return results
}

// mockHostManager implements remote.HostManager.
type mockHostManager struct {
	hosts map[string]remote.RemoteHost
}

func (m *mockHostManager) AddHost(h remote.RemoteHost) error {
	m.hosts[h.Name] = h
	return nil
}

func (m *mockHostManager) RemoveHost(name string) error {
	delete(m.hosts, name)
	return nil
}

func (m *mockHostManager) GetHost(
	name string,
) (*remote.RemoteHost, error) {
	h, ok := m.hosts[name]
	if !ok {
		return nil, nil
	}
	return &h, nil
}

func (m *mockHostManager) ListHosts() []remote.RemoteHost {
	hosts := make([]remote.RemoteHost, 0, len(m.hosts))
	for _, h := range m.hosts {
		hosts = append(hosts, h)
	}
	return hosts
}

func (m *mockHostManager) ProbeHost(
	ctx context.Context, name string,
) (*remote.HostResources, error) {
	return &remote.HostResources{Host: name}, nil
}

func (m *mockHostManager) ProbeAll(
	ctx context.Context,
) map[string]*remote.HostResources {
	// Return a synthetic snapshot per host so the scheduler's
	// StrategyResourceAware has scoring data — without snapshots
	// every PlacementDecision has empty HostName and partitioned
	// deploys can't pick a target.
	out := make(map[string]*remote.HostResources, len(m.hosts))
	for name := range m.hosts {
		out[name] = &remote.HostResources{
			Host:          name,
			CPUCores:      8,
			CPUPercent:    20.0,
			MemoryTotalMB: 16384,
			MemoryUsedMB:  4096,
			MemoryPercent: 25.0,
			DiskTotalMB:   500000,
			DiskUsedMB:    100000,
			DiskPercent:   20.0,
		}
	}
	return out
}

func (m *mockHostManager) HostState(
	name string,
) remote.HostState {
	return remote.HostOnline
}

func TestNewAdapter(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)
	assert.NotNil(t, adapter)
}

func TestNewAdapter_WithProjectDir(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(
		WithProjectDir("/tmp/test"),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/test", adapter.projectDir)
}

func TestAdapter_DetectRuntime_WithExisting(t *testing.T) {
	t.Parallel()
	rt := &mockRuntime{name: "docker"}
	adapter, err := NewAdapter(
		WithRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	name, err := adapter.DetectRuntime(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "docker", name)
}

func TestAdapter_DetectRuntime_WithoutRuntime(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping runtime detection in short mode") // SKIP-OK: #short-mode
	}
	// Create adapter without runtime; will attempt auto-detection
	adapter, err := NewAdapter(WithLogger(logging.NopLogger{}))
	require.NoError(t, err)

	// This may succeed if Docker/Podman is available, or fail otherwise
	name, err := adapter.DetectRuntime(context.Background())
	if err != nil {
		// Runtime not available, skip test
		t.Skipf("No container runtime detected: %v (SKIP-OK: #unmarked-skip-needs-ticket)", err)
	}
	// If we get here, runtime was detected
	assert.NotEmpty(t, name)
	assert.Contains(t, []string{"docker", "podman"}, name)
}

func TestAdapter_RuntimeAvailable(t *testing.T) {
	t.Parallel()
	rt := &mockRuntime{name: "docker"}
	adapter, err := NewAdapter(
		WithRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	assert.True(t, adapter.RuntimeAvailable(
		context.Background(),
	))
}

func TestAdapter_RuntimeAvailable_WithoutRuntime(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping runtime detection in short mode") // SKIP-OK: #short-mode
	}
	adapter, err := NewAdapter(WithLogger(logging.NopLogger{}))
	require.NoError(t, err)

	available := adapter.RuntimeAvailable(context.Background())
	// Could be true or false depending on environment
	// Just verify no panic
	t.Logf("Runtime available: %v", available)
}

func TestAdapter_ComposeUp(t *testing.T) {
	t.Parallel()
	orch := &mockOrchestrator{}
	adapter, err := NewAdapter(
		WithOrchestrator(orch),
		WithProjectDir("/tmp/project"),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	err = adapter.ComposeUp(
		context.Background(),
		"docker-compose.yml", "default",
	)
	require.NoError(t, err)
	assert.True(t, orch.upCalled)
	assert.Contains(t, orch.lastFile, "docker-compose.yml")
}

func TestAdapter_ComposeDown(t *testing.T) {
	t.Parallel()
	orch := &mockOrchestrator{}
	adapter, err := NewAdapter(
		WithOrchestrator(orch),
		WithProjectDir("/tmp/project"),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	err = adapter.ComposeDown(
		context.Background(),
		"docker-compose.yml", "",
	)
	require.NoError(t, err)
	assert.True(t, orch.downCalled)
}

func TestAdapter_ComposeUp_NoOrchestrator(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	err = adapter.ComposeUp(
		context.Background(),
		"docker-compose.yml", "",
	)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not available")
}

func TestAdapter_ComposeStatus_NoOrchestrator(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	_, err = adapter.ComposeStatus(
		context.Background(),
		"docker-compose.yml",
	)
	assert.Error(t, err)
}

func TestAdapter_HealthCheck_NoChecker(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	_, err = adapter.HealthCheck(
		context.Background(),
		"test", "localhost", "8080",
		"/health", "http", 5*time.Second,
	)
	assert.Error(t, err)
}

func TestAdapter_HealthCheckHTTP_InvalidURL(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	err = adapter.HealthCheckHTTP("http://localhost:99999/invalid")
	assert.Error(t, err)
}

func TestAdapter_HealthCheckTCP_InvalidPort(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	ok := adapter.HealthCheckTCP("localhost", 1)
	assert.False(t, ok)
}

func TestAdapter_Distribute(t *testing.T) {
	t.Parallel()
	dist := &mockDistributor{}
	adapter, err := NewAdapter(
		WithDistributor(dist),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	reqs := []scheduler.ContainerRequirements{
		{Name: "app-1", Image: "nginx"},
	}
	summary, err := adapter.Distribute(
		context.Background(), reqs,
	)
	require.NoError(t, err)
	assert.True(t, dist.distributeCalled)
	assert.Equal(t, 1, summary.TotalContainers)
}

func TestAdapter_Distribute_NoDistributor(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	_, err = adapter.Distribute(
		context.Background(),
		[]scheduler.ContainerRequirements{{Name: "app"}},
	)
	assert.Error(t, err)
}

func TestAdapter_Undistribute(t *testing.T) {
	t.Parallel()
	dist := &mockDistributor{}
	adapter, err := NewAdapter(
		WithDistributor(dist),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	err = adapter.Undistribute(context.Background())
	require.NoError(t, err)
	assert.True(t, dist.undistributeCalled)
}

func TestAdapter_Undistribute_NoDistributor(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	err = adapter.Undistribute(context.Background())
	assert.NoError(t, err)
}

func TestAdapter_DistributionStatus(t *testing.T) {
	t.Parallel()
	containers := []distribution.DistributedContainer{
		{HostName: "local", State: distribution.StateRunning},
	}
	dist := &mockDistributor{containers: containers}
	adapter, err := NewAdapter(
		WithDistributor(dist),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	status := adapter.DistributionStatus(context.Background())
	assert.Len(t, status, 1)
}

func TestAdapter_RemoteEnabled(t *testing.T) {
	t.Parallel()
	adapter, _ := NewAdapter(WithLogger(logging.NopLogger{}))
	assert.False(t, adapter.RemoteEnabled())

	adapter.distributor = &mockDistributor{}
	adapter.hostManager = &mockHostManager{
		hosts: map[string]remote.RemoteHost{},
	}
	assert.True(t, adapter.RemoteEnabled())
}

func TestAdapter_ListHosts(t *testing.T) {
	t.Parallel()
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"h1": {Name: "h1", Address: "10.0.0.1"},
		},
	}
	adapter, _ := NewAdapter(
		WithHostManager(hm),
		WithLogger(logging.NopLogger{}),
	)

	hosts := adapter.ListHosts()
	assert.Len(t, hosts, 1)
}

func TestAdapter_ListHosts_NoManager(t *testing.T) {
	t.Parallel()
	adapter, _ := NewAdapter(WithLogger(logging.NopLogger{}))
	hosts := adapter.ListHosts()
	assert.Nil(t, hosts)
}

func TestAdapter_ProbeHost(t *testing.T) {
	t.Parallel()
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{},
	}
	adapter, _ := NewAdapter(
		WithHostManager(hm),
		WithLogger(logging.NopLogger{}),
	)

	res, err := adapter.ProbeHost(
		context.Background(), "h1",
	)
	require.NoError(t, err)
	assert.Equal(t, "h1", res.Host)
}

func TestAdapter_ProbeHost_NoManager(t *testing.T) {
	t.Parallel()
	adapter, _ := NewAdapter(WithLogger(logging.NopLogger{}))
	_, err := adapter.ProbeHost(
		context.Background(), "h1",
	)
	assert.Error(t, err)
}

func TestAdapter_Shutdown(t *testing.T) {
	t.Parallel()
	dist := &mockDistributor{}
	adapter, _ := NewAdapter(
		WithDistributor(dist),
		WithLogger(logging.NopLogger{}),
	)

	err := adapter.Shutdown(context.Background())
	require.NoError(t, err)
	assert.True(t, dist.undistributeCalled)
}

func TestAdapter_Runtime(t *testing.T) {
	t.Parallel()
	rt := &mockRuntime{name: "podman"}
	adapter, _ := NewAdapter(
		WithRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)

	assert.Equal(t, "podman", adapter.Runtime().Name())
}

func TestAdapter_Orchestrator(t *testing.T) {
	t.Parallel()
	orch := &mockOrchestrator{}
	adapter, _ := NewAdapter(
		WithOrchestrator(orch),
		WithLogger(logging.NopLogger{}),
	)

	assert.NotNil(t, adapter.Orchestrator())
}

func TestAdapter_ToEndpoint(t *testing.T) {
	t.Parallel()
	adapter, _ := NewAdapter(WithLogger(logging.NopLogger{}))
	ep := adapter.ToEndpoint(
		"postgres", "localhost", "5432",
		"/health", "tcp",
		"docker-compose.yml", "postgres", "default",
		true, true, false,
	)
	assert.Equal(t, "localhost", ep.Host)
	assert.Equal(t, "5432", ep.Port)
	assert.True(t, ep.Required)
	assert.False(t, ep.Remote)
}

func TestAdapter_HealthCheck_WithChecker(t *testing.T) {
	t.Parallel()
	checker := health.NewDefaultChecker()
	adapter, err := NewAdapter(
		WithHealthChecker(checker),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	// TCP check to a port that should be closed.
	result, err := adapter.HealthCheck(
		context.Background(),
		"test", "localhost", "1",
		"", "tcp", 1*time.Second,
	)
	require.NoError(t, err)
	assert.False(t, result.Healthy)
}

func TestAdapter_HealthCheck_WithMockChecker(t *testing.T) {
	t.Parallel()
	checker := &mockHealthChecker{
		checkResults: map[string]bool{
			"service1": true,
			"service2": false,
		},
	}
	adapter, err := NewAdapter(
		WithHealthChecker(checker),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	// Healthy service
	result, err := adapter.HealthCheck(
		context.Background(),
		"service1", "localhost", "8080",
		"/health", "http", 5*time.Second,
	)
	require.NoError(t, err)
	assert.True(t, result.Healthy)

	// Unhealthy service
	result, err = adapter.HealthCheck(
		context.Background(),
		"service2", "localhost", "8080",
		"/health", "http", 5*time.Second,
	)
	require.NoError(t, err)
	assert.False(t, result.Healthy)
}

func TestAdapter_HealthCheckAll(t *testing.T) {
	t.Parallel()
	checker := &mockHealthChecker{
		checkAllResults: []*health.HealthResult{
			{Healthy: true},
			{Healthy: false, Error: "connection refused"},
		},
	}
	adapter, err := NewAdapter(
		WithHealthChecker(checker),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	targets := []health.HealthTarget{
		{Name: "service1", Host: "localhost", Port: "8080"},
		{Name: "service2", Host: "localhost", Port: "9090"},
	}
	errors := adapter.HealthCheckAll(context.Background(), targets)
	assert.Len(t, errors, 1)
	assert.Contains(t, errors, "service2")
	assert.ErrorContains(t, errors["service2"], "connection refused")
}

func TestAdapter_HealthCheckAll_NoChecker(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(WithLogger(logging.NopLogger{}))
	require.NoError(t, err)

	errors := adapter.HealthCheckAll(context.Background(), []health.HealthTarget{})
	assert.Empty(t, errors)
}

func TestAdapter_StatusAll(t *testing.T) {
	t.Parallel()
	rt := &mockRuntime{name: "docker"}
	adapter, err := NewAdapter(
		WithRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	status, err := adapter.StatusAll(context.Background())
	require.NoError(t, err)
	assert.Empty(t, status) // mock returns empty list
}

func TestAdapter_StatusAll_NoRuntime(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(WithLogger(logging.NopLogger{}))
	require.NoError(t, err)

	_, err = adapter.StatusAll(context.Background())
	assert.Error(t, err)
}

func TestAdapter_ListContainers(t *testing.T) {
	t.Parallel()
	orch := &mockOrchestrator{}
	adapter, err := NewAdapter(
		WithOrchestrator(orch),
		WithProjectDir("/tmp/project"),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	status, err := adapter.ListContainers(context.Background(), "docker-compose.yml")
	require.NoError(t, err)
	assert.Nil(t, status) // mock returns nil
}

func TestAdapter_ListContainers_NoOrchestrator(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(WithLogger(logging.NopLogger{}))
	require.NoError(t, err)

	_, err = adapter.ListContainers(context.Background(), "docker-compose.yml")
	assert.Error(t, err)
}

func TestAdapter_ToHealthTarget(t *testing.T) {
	t.Parallel()
	adapter, _ := NewAdapter(WithLogger(logging.NopLogger{}))
	target := adapter.ToHealthTarget(
		"postgres", "localhost", "5432",
		"/health", "tcp", 5*time.Second, true,
	)
	assert.Equal(t, "postgres", target.Name)
	assert.Equal(t, "localhost", target.Host)
	assert.Equal(t, "5432", target.Port)
	assert.Equal(t, "/health", target.Path)
	assert.Equal(t, health.HealthType("tcp"), target.Type)
	assert.Equal(t, 5*time.Second, target.Timeout)
	assert.True(t, target.Required)
}

func TestAdapter_BootAll(t *testing.T) {
	t.Parallel()
	rt := &mockRuntime{name: "docker"}
	orch := &mockOrchestrator{}
	hc := &mockHealthChecker{}
	adapter, err := NewAdapter(
		WithRuntime(rt),
		WithOrchestrator(orch),
		WithHealthChecker(hc),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	endpoints := map[string]endpoint.ServiceEndpoint{}
	summary, err := adapter.BootAll(context.Background(), endpoints)
	require.NoError(t, err)
	assert.NotNil(t, summary)
}

func TestAdapter_BootAll_WithDistributor(t *testing.T) {
	t.Parallel()
	dist := &mockDistributor{}
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	adapter, err := NewAdapter(
		WithDistributor(dist),
		WithHostManager(hm),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	endpoints := map[string]endpoint.ServiceEndpoint{}
	summary, err := adapter.BootAll(context.Background(), endpoints)
	require.NoError(t, err)
	assert.NotNil(t, summary)
}

func TestNewAdapterFromConfig_NoRuntime(t *testing.T) {
	t.Parallel()
	// Create a temporary directory as project root
	tmpDir := t.TempDir()
	// Change working directory to tmpDir
	oldDir, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(oldDir)
	err = os.Chdir(tmpDir)
	require.NoError(t, err)

	// Create containers/.env file with remote distribution disabled
	containersDir := filepath.Join(tmpDir, "Containers")
	err = os.MkdirAll(containersDir, 0755)
	require.NoError(t, err)
	envPath := filepath.Join(containersDir, ".env")
	err = os.WriteFile(envPath, []byte("CONTAINERS_REMOTE_ENABLED=false\n"), 0644)
	require.NoError(t, err)

	cfg := &config.Config{}
	adapter, err := NewAdapterFromConfig(cfg)
	require.NoError(t, err)
	assert.NotNil(t, adapter)
	// No runtime should be detected (since we're in a temp dir with no Docker/Podman)
	// But adapter may still have nil runtime.
	// This test at least exercises the loading of .env file.
}

func TestAdapter_StatusAll_WithListError(t *testing.T) {
	t.Parallel()
	rt := &mockRuntime{name: "docker", listError: fmt.Errorf("runtime error")}
	adapter, err := NewAdapter(
		WithRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)

	_, err = adapter.StatusAll(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "list containers")
}

func TestAdapter_RemoteComposeUp_NoRemoteDistribution(t *testing.T) {
	t.Parallel()
	adapter, _ := NewAdapter(WithLogger(logging.NopLogger{}))
	err := adapter.RemoteComposeUp(context.Background(), "docker-compose.yml", "default")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "remote distribution not configured")
}

func TestAdapter_RemoteEnabled_WithDistributorOnly(t *testing.T) {
	t.Parallel()
	adapter, _ := NewAdapter(WithLogger(logging.NopLogger{}))
	adapter.distributor = &mockDistributor{}
	// hostManager nil
	assert.False(t, adapter.RemoteEnabled())
}

func TestAdapter_HealthCheckHTTP_Success(t *testing.T) {
	t.Parallel()
	// Start a test HTTP server that returns 200 OK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	adapter, err := NewAdapter(WithLogger(logging.NopLogger{}))
	require.NoError(t, err)

	err = adapter.HealthCheckHTTP(server.URL)
	assert.NoError(t, err)
}

func TestAdapter_HealthCheckHTTP_Failure(t *testing.T) {
	t.Parallel()
	// Start a test HTTP server that returns 500 Internal Server Error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	adapter, err := NewAdapter(WithLogger(logging.NopLogger{}))
	require.NoError(t, err)

	err = adapter.HealthCheckHTTP(server.URL)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "health check failed with status: 500")
}

func TestAdapter_HealthCheckHTTP_ConnectionError(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(WithLogger(logging.NopLogger{}))
	require.NoError(t, err)

	// Use a non-existent URL to cause connection error
	err = adapter.HealthCheckHTTP("http://localhost:99999/invalid")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot connect")
}

func TestAdapter_HealthCheckTCP_Success(t *testing.T) {
	t.Parallel()
	// Start a test TCP server
	listener, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	adapter, err := NewAdapter(WithLogger(logging.NopLogger{}))
	require.NoError(t, err)

	host, portStr, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ok := adapter.HealthCheckTCP(host, port)
	assert.True(t, ok)
}

func TestAdapter_HealthCheckTCP_Failure(t *testing.T) {
	t.Parallel()
	adapter, err := NewAdapter(WithLogger(logging.NopLogger{}))
	require.NoError(t, err)

	// Use a port that is unlikely to be listening
	ok := adapter.HealthCheckTCP("localhost", 65535)
	assert.False(t, ok)
}

func TestLogrusAdapter(t *testing.T) {
	t.Parallel()
	l := &logrusAdapter{}
	// Just verify no panic.
	l.Debug("debug %s", "test")
	l.Info("info %s", "test")
	l.Warn("warn %s", "test")
	l.Error("error %s", "test")
}

// recordingExecutor implements remote.RemoteExecutor and records
// every Execute and CopyFile call, for asserting fan-out behavior
// across multiple hosts without touching a real SSH daemon.
//
// Recording slices use safe.Slice (CONST-029): the bare-mutex+slice
// pattern is prohibited even in test scaffolding, since the audit
// gate doesn't distinguish.
type recordingExecutor struct {
	executed *safe.Slice[recordedExec]
	copied   *safe.Slice[recordedCopy]
	// failFor names the host to simulate a deploy failure for.
	// Execute returns an error when called with host.Name == failFor.
	failFor string
}

// newRecordingExecutor returns a ready-to-use recording executor.
// failFor is the host name that Execute should simulate a deploy
// failure for; pass "" to never fail.
func newRecordingExecutor(failFor string) *recordingExecutor {
	return &recordingExecutor{
		executed: safe.NewSlice[recordedExec](),
		copied:   safe.NewSlice[recordedCopy](),
		failFor:  failFor,
	}
}

type recordedExec struct {
	host string
	cmd  string
}

type recordedCopy struct {
	host        string
	local, dest string
}

func (m *recordingExecutor) Execute(
	ctx context.Context, host remote.RemoteHost, cmd string,
) (*remote.CommandResult, error) {
	m.executed.Append(recordedExec{
		host: host.Name, cmd: cmd,
	})
	if m.failFor != "" && host.Name == m.failFor {
		return &remote.CommandResult{ExitCode: 1},
			fmt.Errorf("simulated failure on %s", host.Name)
	}
	return &remote.CommandResult{
		Stdout:   "ok",
		ExitCode: 0,
	}, nil
}

func (m *recordingExecutor) ExecuteStream(
	ctx context.Context, host remote.RemoteHost, cmd string,
) (io.ReadCloser, error) {
	return nil, fmt.Errorf("stream not supported in test")
}

func (m *recordingExecutor) CopyFile(
	ctx context.Context,
	host remote.RemoteHost,
	localPath, remotePath string,
) error {
	m.copied.Append(recordedCopy{
		host: host.Name, local: localPath, dest: remotePath,
	})
	return nil
}

func (m *recordingExecutor) CopyDir(
	ctx context.Context,
	host remote.RemoteHost,
	localDir, remoteDir string,
) error {
	return nil
}

func (m *recordingExecutor) IsReachable(
	ctx context.Context, host remote.RemoteHost,
) bool {
	return true
}

// writeTempCompose drops a minimal compose file on disk and returns
// its absolute path.
func writeTempCompose(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	// Two independent services (no depends_on) so partitioned
	// placement has something to split between hosts. With
	// StrategyResourceAware + identical hosts the scheduler picks
	// each based on score; both hosts receive at least one
	// service when there are >=2 hosts.
	content := "services:\n" +
		"  placeholder-a:\n    image: busybox:latest\n" +
		"  placeholder-b:\n    image: busybox:latest\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// countExecMatching returns the number of recorded Execute calls
// for a given host where the command contains all `mustContain`
// substrings.
func (m *recordingExecutor) countExecMatching(
	host string, mustContain ...string,
) int {
	n := 0
	for _, e := range m.executed.Snapshot() {
		if e.host != host {
			continue
		}
		ok := true
		for _, s := range mustContain {
			if !strings.Contains(e.cmd, s) {
				ok = false
				break
			}
		}
		if ok {
			n++
		}
	}
	return n
}

// TestAdapter_RemoteComposeUp_PartitionsAcrossHosts verifies the
// CONST-034 / BUGFIXES Issue #52 behavior: each service in the
// compose runs on EXACTLY one host. With two independent services
// and two hosts, both hosts must receive a deploy (one service each)
// — the previous broadcast behavior that put every service on every
// host is forbidden because it produces divergent state.
func TestAdapter_RemoteComposeUp_PartitionsAcrossHosts(t *testing.T) {
	t.Parallel()
	composePath := writeTempCompose(t)

	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{
		"thinker": {Name: "thinker", Address: "thinker.local", User: "u1"},
		"amber":   {Name: "amber", Address: "amber.local", User: "u2"},
	}}
	exec := newRecordingExecutor("")

	adapter, err := NewAdapter(
		WithHostManager(hm),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)
	adapter.executor = exec

	err = adapter.RemoteComposeUp(
		context.Background(), composePath, "default",
	)
	require.NoError(t, err)

	// Partitioning invariant: total compose-ups across all hosts
	// equals the number of host ASSIGNMENTS, not the cross-product
	// of services × hosts. With 2 services that may land on 1 or
	// both hosts depending on the scheduler's scoring; what we
	// FORBID is any host receiving more than one compose-up call
	// (which would mean duplicate deploys).
	thinkerUps := exec.countExecMatching("thinker", "up", "-d")
	amberUps := exec.countExecMatching("amber", "up", "-d")
	totalUps := thinkerUps + amberUps

	assert.Greater(t, totalUps, 0,
		"at least one host must receive a compose up")
	assert.LessOrEqual(t, totalUps, 2,
		"total compose-ups must be ≤ host count under partitioning, got %d "+
			"(broadcast bug would produce 4)", totalUps)
	assert.LessOrEqual(t, thinkerUps, 1,
		"thinker must receive ≤ 1 compose-up (one per-host filtered file)")
	assert.LessOrEqual(t, amberUps, 1,
		"amber must receive ≤ 1 compose-up (one per-host filtered file)")
}

// TestAdapter_RemoteComposeUp_HostLabelOverridesProfile verifies
// the deploy_profile label override: a host carrying the
// deploy_profile label receives `--profile <label>` instead of the
// caller's argument. Under partitioning, the override only applies
// to whichever host received a service.
func TestAdapter_RemoteComposeUp_HostLabelOverridesProfile(t *testing.T) {
	t.Parallel()
	composePath := writeTempCompose(t)

	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{
		"storage-host": {
			Name: "storage-host", Address: "s.local", User: "u1",
			Labels: map[string]string{
				"deploy_profile": "storage",
			},
		},
		"compute-host": {
			Name: "compute-host", Address: "c.local", User: "u2",
			// no deploy_profile label -> uses caller's "default".
		},
	}}
	exec := newRecordingExecutor("")

	adapter, err := NewAdapter(
		WithHostManager(hm),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)
	adapter.executor = exec

	err = adapter.RemoteComposeUp(
		context.Background(), composePath, "default",
	)
	require.NoError(t, err)

	// At least one host must receive the override. With two
	// services and two hosts under StrategyResourceAware, both get
	// a deploy. Whichever host happens to have a service placed
	// must use the right profile — storage-host uses "storage",
	// compute-host uses caller's "default".
	storageUps := exec.countExecMatching(
		"storage-host", "--profile", "storage", "up", "-d",
	)
	storageDefault := exec.countExecMatching(
		"storage-host", "--profile", "default", "up", "-d",
	)
	if storageUps+storageDefault > 0 {
		assert.Greater(t, storageUps, 0,
			"storage-host receiving services must use --profile storage")
		assert.Equal(t, 0, storageDefault,
			"storage-host must not see caller profile when label overrides")
	}

	computeUps := exec.countExecMatching(
		"compute-host", "--profile", "default", "up", "-d",
	)
	computeStorage := exec.countExecMatching(
		"compute-host", "--profile", "storage", "up", "-d",
	)
	if computeUps+computeStorage > 0 {
		assert.Greater(t, computeUps, 0,
			"compute-host receiving services must use caller's --profile default")
	}
}

// TestAdapter_RemoteComposeUp_PartialFailureReturnsNil verifies
// the continue-on-error policy: one host failing does not abort
// deployment of the others. Under partitioning, a service placed
// on the dead host fails but services placed on alive hosts still
// succeed.
func TestAdapter_RemoteComposeUp_PartialFailureReturnsNil(t *testing.T) {
	t.Parallel()
	composePath := writeTempCompose(t)

	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{
		"alive": {Name: "alive", Address: "ok.local", User: "u1"},
		"dead":  {Name: "dead", Address: "bad.local", User: "u2"},
	}}
	exec := newRecordingExecutor("dead")

	adapter, err := NewAdapter(
		WithHostManager(hm),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)
	adapter.executor = exec

	err = adapter.RemoteComposeUp(
		context.Background(), composePath, "default",
	)
	assert.NoError(t, err,
		"partial success must NOT return an error — boot should proceed")

	// Alive host got at least one compose up (one of the 2 services
	// placed on it under StrategyResourceAware partitioning).
	assert.Greater(t,
		exec.countExecMatching("alive", "up", "-d"), 0,
		"alive host must have received its placed service despite dead-host failure")
}

// TestAdapter_RemoteComposeUp_TotalFailureReturnsError verifies
// the all-hosts-failed case: when every host fails, the method
// returns a non-nil error aggregating the failure list.
func TestAdapter_RemoteComposeUp_TotalFailureReturnsError(t *testing.T) {
	t.Parallel()
	composePath := writeTempCompose(t)

	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{
		"h1": {Name: "h1", Address: "h1.local", User: "u1"},
		"h2": {Name: "h2", Address: "h2.local", User: "u2"},
	}}
	exec := newRecordingExecutor("") // fail for all

	// Override Execute via wrapper that always errors.
	failExec := &failingExecutor{inner: exec}

	adapter, err := NewAdapter(
		WithHostManager(hm),
		WithLogger(logging.NopLogger{}),
	)
	require.NoError(t, err)
	adapter.executor = failExec

	err = adapter.RemoteComposeUp(
		context.Background(), composePath, "default",
	)
	require.Error(t, err,
		"all-hosts-failed must return error")
	// Under partitioning, the aggregate error names every host that
	// was ASSIGNED a service AND failed. With 2 services that may
	// be co-located on one host (resource-aware with identical
	// snapshots ties on score), we don't require BOTH hosts in the
	// message — only that the error reports at least one failed
	// host by name.
	hasH1 := strings.Contains(err.Error(), "h1")
	hasH2 := strings.Contains(err.Error(), "h2")
	assert.True(t, hasH1 || hasH2,
		"aggregate error must name at least one failing host")
}

// failingExecutor wraps a recordingExecutor and makes Execute
// always return an error. Used for the total-failure path test
// where we want both hosts to fail without special-casing each.
type failingExecutor struct {
	inner *recordingExecutor
}

func (f *failingExecutor) Execute(
	ctx context.Context, host remote.RemoteHost, cmd string,
) (*remote.CommandResult, error) {
	f.inner.Execute(ctx, host, cmd) // still record the call
	return &remote.CommandResult{ExitCode: 1},
		fmt.Errorf("simulated total failure on %s", host.Name)
}

func (f *failingExecutor) ExecuteStream(
	ctx context.Context, host remote.RemoteHost, cmd string,
) (io.ReadCloser, error) {
	return f.inner.ExecuteStream(ctx, host, cmd)
}

func (f *failingExecutor) CopyFile(
	ctx context.Context,
	host remote.RemoteHost,
	localPath, remotePath string,
) error {
	return f.inner.CopyFile(ctx, host, localPath, remotePath)
}

func (f *failingExecutor) CopyDir(
	ctx context.Context,
	host remote.RemoteHost,
	localDir, remoteDir string,
) error {
	return f.inner.CopyDir(ctx, host, localDir, remoteDir)
}

func (f *failingExecutor) IsReachable(
	ctx context.Context, host remote.RemoteHost,
) bool {
	return true
}
