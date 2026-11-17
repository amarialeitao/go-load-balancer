package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// Server is what the load balancer needs from a backend.
type Server interface {
	Address() string
	IsAlive() bool
	Serve(rw http.ResponseWriter, r *http.Request)
}

// HealthChecker is implemented by backends that can check their own health.
type HealthChecker interface {
	CheckHealth() bool
}

// SimpleServer is a backend reached through a reverse proxy to a single URL.
type SimpleServer struct {
	address string
	proxy   *httputil.ReverseProxy
	client  *http.Client

	mu    sync.RWMutex
	alive bool
}

func NewSimpleServer(address string) *SimpleServer {
	serverURL, err := url.Parse(address)
	if err != nil {
		panic(fmt.Sprintf("Error parsing server URL %s: %v", address, err))
	}

	s := &SimpleServer{
		address: address,
		proxy:   httputil.NewSingleHostReverseProxy(serverURL),
		client:  &http.Client{Timeout: 2 * time.Second},
		alive:   true,
	}

	// Passive health check: if a request to this backend fails, take it out of
	// the rotation right away instead of waiting for the next periodic check.
	s.proxy.ErrorHandler = func(rw http.ResponseWriter, r *http.Request, err error) {
		log.Printf("Backend %s failed: %v", s.address, err)
		s.SetAlive(false)
		http.Error(rw, "Bad gateway", http.StatusBadGateway)
	}

	return s
}

func (s *SimpleServer) Address() string {
	return s.address
}

func (s *SimpleServer) IsAlive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.alive
}

func (s *SimpleServer) SetAlive(alive bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alive = alive
}

// CheckHealth sends a GET request to the backend and updates its state.
// Any response below 500 means the server is up; network errors, timeouts
// and 5xx responses mean it is down.
func (s *SimpleServer) CheckHealth() bool {
	resp, err := s.client.Get(s.address)
	alive := err == nil && resp.StatusCode < http.StatusInternalServerError
	if resp != nil {
		resp.Body.Close()
	}
	s.SetAlive(alive)
	return alive
}

func (s *SimpleServer) Serve(rw http.ResponseWriter, r *http.Request) {
	s.proxy.ServeHTTP(rw, r)
}

// LoadBalancer spreads requests across its servers in round-robin order,
// skipping servers that are not alive.
type LoadBalancer struct {
	port            string
	roundRobinCount int
	servers         []Server
	mu              sync.Mutex
}

func NewLoadBalancer(port string, servers []Server) *LoadBalancer {
	return &LoadBalancer{
		port:            port,
		roundRobinCount: 0,
		servers:         servers,
	}
}

// getNextAvailableServer returns the next alive server in round-robin order,
// or nil when every server is down.
func (lb *LoadBalancer) getNextAvailableServer() Server {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	for i := 0; i < len(lb.servers); i++ {
		server := lb.servers[lb.roundRobinCount%len(lb.servers)]
		lb.roundRobinCount++
		if server.IsAlive() {
			return server
		}
	}

	return nil
}

func (lb *LoadBalancer) serveProxy(rw http.ResponseWriter, r *http.Request) {
	targetServer := lb.getNextAvailableServer()
	if targetServer == nil {
		http.Error(rw, "No available servers", http.StatusServiceUnavailable)
		return
	}

	log.Printf("Forwarding request to address %s", targetServer.Address())
	targetServer.Serve(rw, r)
}

// checkHealth runs one health check on every server that supports it and
// logs each change of state.
func (lb *LoadBalancer) checkHealth() {
	for _, server := range lb.servers {
		checker, ok := server.(HealthChecker)
		if !ok {
			continue
		}
		wasAlive := server.IsAlive()
		isAlive := checker.CheckHealth()
		if wasAlive != isAlive {
			state := "DOWN"
			if isAlive {
				state = "UP"
			}
			log.Printf("Health check: %s is %s", server.Address(), state)
		}
	}
}

// StartHealthChecks checks every server once, then again at each interval,
// until stop is closed.
func (lb *LoadBalancer) StartHealthChecks(interval time.Duration, stop <-chan struct{}) {
	lb.checkHealth()
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				lb.checkHealth()
			case <-stop:
				return
			}
		}
	}()
}

func main() {
	servers := []Server{
		NewSimpleServer("https://www.facebook.com"),
		NewSimpleServer("http://www.bing.com"),
		NewSimpleServer("https://www.google.com"),
	}
	lb := NewLoadBalancer("8000", servers)
	lb.StartHealthChecks(10*time.Second, make(chan struct{}))

	http.HandleFunc("/", lb.serveProxy)

	log.Printf("Serving requests at 'localhost:%v'", lb.port)
	log.Fatal(http.ListenAndServe(":"+lb.port, nil))
}
