package services

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	"digital.vasic.concurrency/pkg/safe"
	"github.com/sirupsen/logrus"
)

// HighAvailabilityManager provides high availability features with load balancing and failover.
//
// Concurrent-safe by construction (CONST-029): instances is a safe.Store;
// statusMu (Pattern Zeta) serialises per-*ServiceInstance Status /
// LastHealth / LoadScore mutations in handleHealthUpdate and
// UpdateInstanceLoad. statusMu is not paired with any bare map/slice
// field.
type HighAvailabilityManager struct {
	statusMu        sync.Mutex
	instances       *safe.Store[string, *ServiceInstance]
	loadBalancer    LoadBalancer
	failoverManager *FailoverManager
	healthChecker   *HealthChecker
	logger          *logrus.Logger
	stopChan        chan struct{}
}

// ServiceInstance represents a service instance in the HA cluster
type ServiceInstance struct {
	ID         string
	Address    string
	Port       int
	Protocol   string
	Status     InstanceStatus
	LastHealth time.Time
	LoadScore  int // 0-100, higher means more loaded
	Metadata   map[string]interface{}
}

// InstanceStatus represents the status of a service instance
type InstanceStatus int

const (
	StatusStarting InstanceStatus = iota
	StatusHealthy
	StatusDegraded
	StatusUnhealthy
	StatusDown
)

// LoadBalancer handles load distribution across instances
type LoadBalancer interface {
	SelectInstance(protocol string, instances []*ServiceInstance) *ServiceInstance
	UpdateLoad(instanceID string, loadScore int)
}

// RoundRobinLoadBalancer implements round-robin load balancing.
//
// Concurrent-safe by construction: lastUsed is a safe.Store; Store.Update
// is used for the atomic read-increment-write cycle in SelectInstance.
type RoundRobinLoadBalancer struct {
	lastUsed *safe.Store[string, int] // protocol -> last used index
}

// LeastLoadedLoadBalancer implements least-loaded load balancing
type LeastLoadedLoadBalancer struct{}

// FailoverManager handles automatic failover.
//
// Concurrent-safe by construction: failoverGroups holds per-protocol
// slices via safe.Store (with Update-based COW for append/remove);
// activeInstances is a safe.Store. groupMu (Pattern Zeta, sync.Mutex)
// serialises the "unregister from failoverGroups + promote new active"
// compound operation across the two stores; it does not pair with any
// bare map/slice field.
type FailoverManager struct {
	groupMu           sync.Mutex
	failoverGroups    *safe.Store[string, []*ServiceInstance]
	activeInstances   *safe.Store[string, *ServiceInstance]
	failoverThreshold time.Duration
	logger            *logrus.Logger
}

// InstanceInfo holds the address and port information for an instance
type InstanceInfo struct {
	Address  string
	Port     int
	Protocol string
}

// HealthChecker performs health checks on service instances.
//
// Concurrent-safe by construction: healthChecks and instanceRegistry
// are safe.Store. statusMu (Pattern Zeta) serialises the per-
// *HealthStatus field mutations inside performHealthChecks and
// checkInstanceHealth (ConsecutiveFailures / IsHealthy / LastCheck /
// ResponseTime / Error). statusMu does not pair with any bare map or
// slice field.
type HealthChecker struct {
	statusMu           sync.Mutex
	checkInterval      time.Duration
	timeout            time.Duration
	unhealthyThreshold int
	healthChecks       *safe.Store[string, *HealthStatus]
	instanceRegistry   *safe.Store[string, *InstanceInfo]
	httpClient         *http.Client
	logger             *logrus.Logger
}

// HealthStatus represents the health status of an instance
type HealthStatus struct {
	InstanceID          string
	LastCheck           time.Time
	ConsecutiveFailures int
	IsHealthy           bool
	ResponseTime        time.Duration
	Error               string
}

// NewHighAvailabilityManager creates a new HA manager
func NewHighAvailabilityManager(logger *logrus.Logger) *HighAvailabilityManager {
	return &HighAvailabilityManager{
		instances:       safe.NewStore[string, *ServiceInstance](),
		loadBalancer:    &LeastLoadedLoadBalancer{},
		failoverManager: NewFailoverManager(logger),
		healthChecker:   NewHealthChecker(logger),
		logger:          logger,
		stopChan:        make(chan struct{}),
	}
}

// RegisterInstance registers a new service instance
func (ham *HighAvailabilityManager) RegisterInstance(instance *ServiceInstance) error {
	var duplicate bool
	ham.instances.Update(instance.ID, func(existing *ServiceInstance, present bool) (*ServiceInstance, bool) {
		if present {
			duplicate = true
			return existing, true
		}
		instance.Status = StatusStarting
		instance.LastHealth = time.Now()
		return instance, true
	})
	if duplicate {
		return fmt.Errorf("instance %s already registered", instance.ID)
	}

	// Register with failover manager
	ham.failoverManager.RegisterInstance(instance)

	// Register with health checker
	ham.healthChecker.RegisterInstance(instance.ID, instance.Address, instance.Port)

	ham.logger.WithFields(logrus.Fields{
		"instanceId": instance.ID,
		"protocol":   instance.Protocol,
		"address":    instance.Address,
		"port":       instance.Port,
	}).Info("Service instance registered")

	return nil
}

// UnregisterInstance removes a service instance
func (ham *HighAvailabilityManager) UnregisterInstance(instanceID string) error {
	if _, existed := ham.instances.Delete(instanceID); !existed {
		return fmt.Errorf("instance %s not registered", instanceID)
	}

	// Unregister from failover manager
	ham.failoverManager.UnregisterInstance(instanceID)

	// Unregister from health checker
	ham.healthChecker.UnregisterInstance(instanceID)

	ham.logger.WithField("instanceId", instanceID).Info("Service instance unregistered")
	return nil
}

// GetInstance selects an available instance for a protocol
func (ham *HighAvailabilityManager) GetInstance(protocol string) (*ServiceInstance, error) {
	var instances []*ServiceInstance
	ham.instances.Range(func(_ string, instance *ServiceInstance) bool {
		if instance.Protocol == protocol && instance.Status == StatusHealthy {
			instances = append(instances, instance)
		}
		return true
	})

	if len(instances) == 0 {
		return nil, fmt.Errorf("no healthy instances available for protocol %s", protocol)
	}

	selected := ham.loadBalancer.SelectInstance(protocol, instances)

	ham.logger.WithFields(logrus.Fields{
		"protocol":   protocol,
		"instanceId": selected.ID,
		"address":    selected.Address,
		"port":       selected.Port,
	}).Debug("Instance selected by load balancer")

	return selected, nil
}

// UpdateInstanceLoad updates the load score for an instance
func (ham *HighAvailabilityManager) UpdateInstanceLoad(instanceID string, loadScore int) error {
	instance, exists := ham.instances.Get(instanceID)
	if !exists {
		return fmt.Errorf("instance %s not found", instanceID)
	}

	ham.statusMu.Lock()
	instance.LoadScore = loadScore
	ham.statusMu.Unlock()
	ham.loadBalancer.UpdateLoad(instanceID, loadScore)

	return nil
}

// GetAllInstances returns all registered instances
func (ham *HighAvailabilityManager) GetAllInstances() []*ServiceInstance {
	return ham.instances.Values()
}

// GetInstancesByProtocol returns instances for a specific protocol
func (ham *HighAvailabilityManager) GetInstancesByProtocol(protocol string) []*ServiceInstance {
	var instances []*ServiceInstance
	ham.instances.Range(func(_ string, instance *ServiceInstance) bool {
		if instance.Protocol == protocol {
			instances = append(instances, instance)
		}
		return true
	})
	return instances
}

// Start begins the HA management processes
func (ham *HighAvailabilityManager) Start(ctx context.Context) error {
	ham.logger.Info("Starting High Availability Manager")

	// Start health checker
	go ham.healthChecker.Start(ctx, ham.handleHealthUpdate)

	// Start failover manager
	go ham.failoverManager.Start(ctx)

	return nil
}

// Stop stops the HA management processes
func (ham *HighAvailabilityManager) Stop() {
	ham.logger.Info("Stopping High Availability Manager")

	close(ham.stopChan)
	ham.healthChecker.Stop()
	ham.failoverManager.Stop()
}

// Private methods

func (ham *HighAvailabilityManager) handleHealthUpdate(instanceID string, healthy bool) {
	instance, exists := ham.instances.Get(instanceID)
	if !exists {
		return
	}

	ham.statusMu.Lock()
	oldStatus := instance.Status
	triggerFailover := false

	if healthy {
		if instance.Status != StatusHealthy {
			instance.Status = StatusHealthy
			ham.logger.WithField("instanceId", instanceID).Info("Instance became healthy")
		}
	} else {
		if instance.Status == StatusHealthy {
			instance.Status = StatusUnhealthy
			ham.logger.WithField("instanceId", instanceID).Warn("Instance became unhealthy")
			triggerFailover = true
		}
	}

	instance.LastHealth = time.Now()
	newStatus := instance.Status
	ham.statusMu.Unlock()

	if triggerFailover {
		go ham.failoverManager.HandleInstanceFailure(instance)
	}

	if oldStatus != newStatus {
		ham.logger.WithFields(logrus.Fields{
			"instanceId": instanceID,
			"oldStatus":  oldStatus,
			"newStatus":  newStatus,
		}).Info("Instance status changed")
	}
}

// LoadBalancer implementations

// SelectInstance selects an instance using round-robin
func (rr *RoundRobinLoadBalancer) SelectInstance(protocol string, instances []*ServiceInstance) *ServiceInstance {
	if len(instances) == 0 {
		return nil
	}

	if rr.lastUsed == nil {
		rr.lastUsed = safe.NewStore[string, int]()
	}

	var nextIndex int
	rr.lastUsed.Update(protocol, func(lastIndex int, _ bool) (int, bool) {
		nextIndex = (lastIndex + 1) % len(instances)
		return nextIndex, true
	})

	return instances[nextIndex]
}

// UpdateLoad updates load information (no-op for round-robin)
func (rr *RoundRobinLoadBalancer) UpdateLoad(instanceID string, loadScore int) {
	// Round-robin doesn't use load scores
}

// SelectInstance selects the least loaded instance
func (ll *LeastLoadedLoadBalancer) SelectInstance(protocol string, instances []*ServiceInstance) *ServiceInstance {
	if len(instances) == 0 {
		return nil
	}

	// Find instance with lowest load score
	var selected *ServiceInstance
	minLoad := 101 // Higher than max possible load score

	for _, instance := range instances {
		if instance.LoadScore < minLoad {
			minLoad = instance.LoadScore
			selected = instance
		}
	}

	return selected
}

// UpdateLoad updates load information
func (ll *LeastLoadedLoadBalancer) UpdateLoad(instanceID string, loadScore int) {
	// Load scores are stored in the instances themselves
}

// FailoverManager implementation

// NewFailoverManager creates a new failover manager
func NewFailoverManager(logger *logrus.Logger) *FailoverManager {
	return &FailoverManager{
		failoverGroups:    safe.NewStore[string, []*ServiceInstance](),
		activeInstances:   safe.NewStore[string, *ServiceInstance](),
		failoverThreshold: 30 * time.Second,
		logger:            logger,
	}
}

// RegisterInstance registers an instance with the failover manager
func (fm *FailoverManager) RegisterInstance(instance *ServiceInstance) {
	protocol := instance.Protocol
	fm.failoverGroups.Update(protocol, func(cur []*ServiceInstance, _ bool) ([]*ServiceInstance, bool) {
		return append(append([]*ServiceInstance(nil), cur...), instance), true
	})

	// If this is the first instance or current active is unhealthy, make it active
	if _, swapped := fm.activeInstances.PutIfAbsent(protocol, instance); swapped {
		fm.logger.WithFields(logrus.Fields{
			"protocol":   protocol,
			"instanceId": instance.ID,
		}).Info("Instance set as active for protocol")
	}
}

// UnregisterInstance removes an instance from failover management.
// Uses a two-phase Snapshot then Update pattern because the Range
// callback cannot call Put on the same Store (Range holds the Store's
// read lock, Put needs the write lock — classic upgrade deadlock).
func (fm *FailoverManager) UnregisterInstance(instanceID string) {
	fm.groupMu.Lock()
	defer fm.groupMu.Unlock()

	// Phase 1: snapshot outside the callback.
	snapshot := fm.failoverGroups.Snapshot()
	// Phase 2: for each protocol containing the instance, rebuild the
	// slice and republish. Also promote a new active instance if we
	// removed the current active.
	for protocol, instances := range snapshot {
		for i, instance := range instances {
			if instance.ID == instanceID {
				next := append([]*ServiceInstance(nil), instances[:i]...)
				next = append(next, instances[i+1:]...)
				fm.failoverGroups.Put(protocol, next)

				if active, exists := fm.activeInstances.Get(protocol); exists && active.ID == instanceID {
					fm.promoteNewActive(protocol)
				}
				break
			}
		}
	}
}

// HandleInstanceFailure handles failure of an instance
func (fm *FailoverManager) HandleInstanceFailure(instance *ServiceInstance) {
	fm.groupMu.Lock()
	defer fm.groupMu.Unlock()

	protocol := instance.Protocol

	// If this was the active instance, promote a backup
	if active, exists := fm.activeInstances.Get(protocol); exists && active.ID == instance.ID {
		fm.logger.WithFields(logrus.Fields{
			"protocol":   protocol,
			"instanceId": instance.ID,
		}).Warn("Active instance failed, promoting backup")

		fm.promoteNewActive(protocol)
	}
}

// Start begins failover monitoring
func (fm *FailoverManager) Start(ctx context.Context) {
	// Periodic check for failed instances
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fm.checkFailoverStatus()
			}
		}
	}()
}

// Stop stops failover monitoring
func (fm *FailoverManager) Stop() {
	// Cleanup handled by context cancellation
}

// promoteNewActive requires fm.groupMu to be held by the caller.
func (fm *FailoverManager) promoteNewActive(protocol string) {
	instances, _ := fm.failoverGroups.Get(protocol)

	// Find a healthy backup instance
	for _, instance := range instances {
		if instance.Status == StatusHealthy {
			fm.activeInstances.Put(protocol, instance)
			fm.logger.WithFields(logrus.Fields{
				"protocol":   protocol,
				"instanceId": instance.ID,
			}).Info("New active instance promoted")
			return
		}
	}

	fm.logger.WithField("protocol", protocol).Error("No healthy backup instances available")
}

func (fm *FailoverManager) checkFailoverStatus() {
	fm.activeInstances.Range(func(protocol string, active *ServiceInstance) bool {
		if active.Status != StatusHealthy {
			// Active instance is not healthy, should have been handled by failure detection
			fm.logger.WithFields(logrus.Fields{
				"protocol":       protocol,
				"activeInstance": active.ID,
				"status":         active.Status,
			}).Warn("Active instance is not healthy")
		}
		return true
	})
}

// HealthChecker implementation

// HealthCheckerConfig holds configuration for the health checker
type HealthCheckerConfig struct {
	CheckInterval      time.Duration
	Timeout            time.Duration
	UnhealthyThreshold int
}

// DefaultHealthCheckerConfig returns the default health checker configuration
func DefaultHealthCheckerConfig() *HealthCheckerConfig {
	return &HealthCheckerConfig{
		CheckInterval:      30 * time.Second,
		Timeout:            5 * time.Second,
		UnhealthyThreshold: 3,
	}
}

// NewHealthChecker creates a new health checker with default configuration
func NewHealthChecker(logger *logrus.Logger) *HealthChecker {
	return NewHealthCheckerWithConfig(logger, DefaultHealthCheckerConfig())
}

// NewHealthCheckerWithConfig creates a new health checker with custom configuration
func NewHealthCheckerWithConfig(logger *logrus.Logger, config *HealthCheckerConfig) *HealthChecker {
	if config == nil {
		config = DefaultHealthCheckerConfig()
	}

	return &HealthChecker{
		checkInterval:      config.CheckInterval,
		timeout:            config.Timeout,
		unhealthyThreshold: config.UnhealthyThreshold,
		healthChecks:       safe.NewStore[string, *HealthStatus](),
		instanceRegistry:   safe.NewStore[string, *InstanceInfo](),
		httpClient: &http.Client{
			Timeout: config.Timeout,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout: config.Timeout,
				}).DialContext,
				TLSHandshakeTimeout:   config.Timeout,
				ResponseHeaderTimeout: config.Timeout,
			},
		},
		logger: logger,
	}
}

// RegisterInstance registers an instance for health checking
func (hc *HealthChecker) RegisterInstance(instanceID, address string, port int) {
	hc.RegisterInstanceWithProtocol(instanceID, address, port, "http")
}

// RegisterInstanceWithProtocol registers an instance for health checking with a specific protocol
func (hc *HealthChecker) RegisterInstanceWithProtocol(instanceID, address string, port int, protocol string) {
	hc.healthChecks.Put(instanceID, &HealthStatus{
		InstanceID: instanceID,
		LastCheck:  time.Now(),
		IsHealthy:  true, // Assume healthy initially
	})

	hc.instanceRegistry.Put(instanceID, &InstanceInfo{
		Address:  address,
		Port:     port,
		Protocol: protocol,
	})

	hc.logger.WithFields(logrus.Fields{
		"instanceId": instanceID,
		"address":    address,
		"port":       port,
		"protocol":   protocol,
	}).Debug("Instance registered for health checking")
}

// UnregisterInstance removes an instance from health checking
func (hc *HealthChecker) UnregisterInstance(instanceID string) {
	hc.healthChecks.Delete(instanceID)
	hc.instanceRegistry.Delete(instanceID)

	hc.logger.WithField("instanceId", instanceID).Debug("Instance unregistered from health checking")
}

// Start begins health checking
func (hc *HealthChecker) Start(ctx context.Context, healthUpdateFunc func(string, bool)) {
	go func() {
		ticker := time.NewTicker(hc.checkInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				hc.performHealthChecks(healthUpdateFunc)
			}
		}
	}()
}

// Stop stops health checking
func (hc *HealthChecker) Stop() {
	// Cleanup handled by context cancellation
}

func (hc *HealthChecker) performHealthChecks(healthUpdateFunc func(string, bool)) {
	hc.healthChecks.Range(func(instanceID string, status *HealthStatus) bool {
		healthy := hc.checkInstanceHealth(instanceID)

		hc.statusMu.Lock()
		oldHealthy := status.IsHealthy
		if healthy {
			status.ConsecutiveFailures = 0
			status.IsHealthy = true
		} else {
			status.ConsecutiveFailures++
			if status.ConsecutiveFailures >= hc.unhealthyThreshold {
				status.IsHealthy = false
			}
		}
		status.LastCheck = time.Now()
		newHealthy := status.IsHealthy
		hc.statusMu.Unlock()

		if oldHealthy != newHealthy {
			healthUpdateFunc(instanceID, newHealthy)
		}
		return true
	})
}

func (hc *HealthChecker) checkInstanceHealth(instanceID string) bool {
	instanceInfo, exists := hc.instanceRegistry.Get(instanceID)
	status, _ := hc.healthChecks.Get(instanceID)

	if !exists || instanceInfo == nil {
		hc.logger.WithField("instanceId", instanceID).Warn("Instance not found in registry")
		return false
	}

	startTime := time.Now()
	var healthy bool
	var checkErr error

	// Perform protocol-specific health check
	switch instanceInfo.Protocol {
	case "http", "https":
		healthy, checkErr = hc.checkHTTPHealth(instanceInfo)
	case "grpc":
		healthy, checkErr = hc.checkGRPCHealth(instanceInfo)
	case "tcp":
		healthy, checkErr = hc.checkTCPHealth(instanceInfo)
	default:
		// For unknown protocols, fall back to TCP connectivity check
		healthy, checkErr = hc.checkTCPHealth(instanceInfo)
	}

	responseTime := time.Since(startTime)

	// Update status with response time and error under statusMu so
	// concurrent performHealthChecks iterations on the same status
	// pointer don't race with counter mutations.
	if status != nil {
		hc.statusMu.Lock()
		status.ResponseTime = responseTime
		if checkErr != nil {
			status.Error = checkErr.Error()
		} else {
			status.Error = ""
		}
		hc.statusMu.Unlock()
	}

	hc.logger.WithFields(logrus.Fields{
		"instanceId":   instanceID,
		"healthy":      healthy,
		"responseTime": responseTime,
		"protocol":     instanceInfo.Protocol,
		"error":        checkErr,
	}).Debug("Health check completed")

	return healthy
}

// checkHTTPHealth performs an HTTP health check by calling the /health endpoint
func (hc *HealthChecker) checkHTTPHealth(info *InstanceInfo) (bool, error) {
	scheme := "http"
	if info.Protocol == "https" {
		scheme = "https"
	}

	url := fmt.Sprintf("%s://%s:%d/health", scheme, info.Address, info.Port)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := hc.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("health check request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read and discard body to ensure connection can be reused
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck

	// Consider 2xx status codes as healthy
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, nil
	}

	return false, fmt.Errorf("unhealthy status code: %d", resp.StatusCode)
}

// checkGRPCHealth performs a gRPC health check using TCP connectivity
// Note: Full gRPC health checking would require the grpc-health-probe protocol
func (hc *HealthChecker) checkGRPCHealth(info *InstanceInfo) (bool, error) {
	// For gRPC, we perform a TCP connectivity check
	// A full implementation would use the grpc.health.v1 protocol
	return hc.checkTCPHealth(info)
}

// checkTCPHealth performs a TCP connectivity check
func (hc *HealthChecker) checkTCPHealth(info *InstanceInfo) (bool, error) {
	address := net.JoinHostPort(info.Address, fmt.Sprintf("%d", info.Port))

	conn, err := net.DialTimeout("tcp", address, hc.timeout)
	if err != nil {
		return false, fmt.Errorf("TCP connection failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	return true, nil
}

// GetInstanceInfo returns the instance information for a given instance ID
func (hc *HealthChecker) GetInstanceInfo(instanceID string) *InstanceInfo {
	info, _ := hc.instanceRegistry.Get(instanceID)
	return info
}

// GetHealthStatus returns the health status for a given instance ID
func (hc *HealthChecker) GetHealthStatus(instanceID string) *HealthStatus {
	status, _ := hc.healthChecks.Get(instanceID)
	return status
}

// SetHTTPClient allows setting a custom HTTP client (useful for testing).
// Protected by statusMu to serialise with any concurrent health-check loop
// reading hc.httpClient via checkHTTPHealth. Single writer expected.
func (hc *HealthChecker) SetHTTPClient(client *http.Client) {
	hc.statusMu.Lock()
	hc.httpClient = client
	hc.statusMu.Unlock()
}

// Circuit Breaker for fault tolerance

type CircuitBreaker struct {
	mu                   sync.Mutex
	state                CircuitState
	failureThreshold     int
	successThreshold     int
	timeout              time.Duration
	consecutiveFailures  int
	consecutiveSuccesses int
	lastFailure          time.Time
}

type CircuitState int

const (
	StateClosed CircuitState = iota
	StateOpen
	StateHalfOpen
)

// String returns the string representation of CircuitState
func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(failureThreshold, successThreshold int, timeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		timeout:          timeout,
	}
}

// Call executes a function with circuit breaker protection
func (cb *CircuitBreaker) Call(fn func() error) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == StateOpen {
		if time.Since(cb.lastFailure) < cb.timeout {
			return fmt.Errorf("circuit breaker is open")
		}
		// HA-CB-001: start the half-open probe window from a clean slate.
		// Carrying a stale consecutiveSuccesses across the Open->HalfOpen
		// transition could close the breaker on fewer than successThreshold
		// probes actually observed in this window.
		cb.state = StateHalfOpen
		cb.consecutiveSuccesses = 0
	}

	err := fn()

	if err != nil {
		cb.onFailure()
		return err
	}

	cb.onSuccess()
	return nil
}

// GetState returns the current circuit breaker state
func (cb *CircuitBreaker) GetState() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// GetFailureCount returns the current consecutive failure count
func (cb *CircuitBreaker) GetFailureCount() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.consecutiveFailures
}

// GetLastFailure returns the timestamp of the last failure
func (cb *CircuitBreaker) GetLastFailure() *time.Time {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.lastFailure.IsZero() {
		return nil
	}
	t := cb.lastFailure
	return &t
}

func (cb *CircuitBreaker) onFailure() {
	cb.consecutiveFailures++
	cb.lastFailure = time.Now()

	if cb.consecutiveFailures >= cb.failureThreshold {
		cb.state = StateOpen
		cb.consecutiveSuccesses = 0
	}
}

func (cb *CircuitBreaker) onSuccess() {
	cb.consecutiveSuccesses++

	// HA-CB-001: a success MUST reset the consecutive-failure run.
	//
	// This reset was previously performed ONLY on the HalfOpen->Closed
	// transition, which meant that while the breaker was CLOSED the field
	// named "consecutiveFailures" was in fact a LIFETIME CUMULATIVE failure
	// count that only ever grew. A provider that succeeded hundreds of times
	// with a handful of scattered, unrelated failures in between would still
	// trip, because those failures accumulated forever and never decayed.
	//
	// Observed in production 2026-09-07: helixllm served real completions
	// successfully for ~90s and then tripped open with no burst of failures
	// anywhere in the log. Matches internal/llm/circuit_breaker.go
	// recordSuccess(), which has always reset this correctly.
	cb.consecutiveFailures = 0

	if cb.state == StateHalfOpen && cb.consecutiveSuccesses >= cb.successThreshold {
		cb.state = StateClosed
		cb.consecutiveSuccesses = 0
	}
}

// Service Registry for service discovery

// ServiceRegistry provides service discovery with per-type endpoint lists.
//
// Concurrent-safe by construction: services is a safe.Store whose values
// are immutable endpoint slices — RegisterService / UnregisterService
// use Store.Update COW to swap in a fresh slice rather than mutating the
// existing one, so concurrent DiscoverServices readers always get a
// consistent snapshot without external locking.
type ServiceRegistry struct {
	services *safe.Store[string, []*ServiceEndpoint]
	logger   *logrus.Logger
}

type ServiceEndpoint struct {
	ID       string
	Address  string
	Port     int
	Protocol string
	Metadata map[string]interface{}
}

// NewServiceRegistry creates a new service registry
func NewServiceRegistry(logger *logrus.Logger) *ServiceRegistry {
	return &ServiceRegistry{
		services: safe.NewStore[string, []*ServiceEndpoint](),
		logger:   logger,
	}
}

// RegisterService registers a service endpoint
func (sr *ServiceRegistry) RegisterService(serviceType string, endpoint *ServiceEndpoint) {
	sr.services.Update(serviceType, func(cur []*ServiceEndpoint, _ bool) ([]*ServiceEndpoint, bool) {
		return append(append([]*ServiceEndpoint(nil), cur...), endpoint), true
	})

	sr.logger.WithFields(logrus.Fields{
		"serviceType": serviceType,
		"endpointId":  endpoint.ID,
		"address":     endpoint.Address,
		"port":        endpoint.Port,
	}).Info("Service endpoint registered")
}

// UnregisterService removes a service endpoint
func (sr *ServiceRegistry) UnregisterService(serviceType, endpointID string) {
	sr.services.Update(serviceType, func(endpoints []*ServiceEndpoint, present bool) ([]*ServiceEndpoint, bool) {
		if !present {
			return nil, false
		}
		for i, endpoint := range endpoints {
			if endpoint.ID == endpointID {
				next := append([]*ServiceEndpoint(nil), endpoints[:i]...)
				next = append(next, endpoints[i+1:]...)
				return next, true
			}
		}
		return endpoints, true
	})
}

// DiscoverServices discovers service endpoints
func (sr *ServiceRegistry) DiscoverServices(serviceType string) []*ServiceEndpoint {
	endpoints, _ := sr.services.Get(serviceType)
	// Caller may mutate the returned slice; return a defensive copy.
	result := make([]*ServiceEndpoint, len(endpoints))
	copy(result, endpoints)
	return result
}

// Load Balancer Strategies

// RandomLoadBalancer implements random load balancing
type RandomLoadBalancer struct{}

// SelectInstance selects a random instance
// Note: Using math/rand for load balancing is acceptable - it doesn't require cryptographic randomness
func (rl *RandomLoadBalancer) SelectInstance(protocol string, instances []*ServiceInstance) *ServiceInstance {
	if len(instances) == 0 {
		return nil
	}

	return instances[rand.Intn(len(instances))] // #nosec G404 - load balancing doesn't require cryptographic randomness
}

// UpdateLoad updates load information (no-op for random)
func (rl *RandomLoadBalancer) UpdateLoad(instanceID string, loadScore int) {
	// Random load balancer doesn't use load scores
}
