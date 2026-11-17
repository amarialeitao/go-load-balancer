package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeServer is a backend whose state is controlled by the test.
type fakeServer struct {
	address string
	alive   bool
}

func (f *fakeServer) Address() string { return f.address }
func (f *fakeServer) IsAlive() bool   { return f.alive }
func (f *fakeServer) Serve(rw http.ResponseWriter, r *http.Request) {
	rw.Write([]byte(f.address))
}

func TestRoundRobinOrder(t *testing.T) {
	a, b, c := &fakeServer{"a", true}, &fakeServer{"b", true}, &fakeServer{"c", true}
	lb := NewLoadBalancer("0", []Server{a, b, c})

	want := []string{"a", "b", "c", "a", "b", "c"}
	for i, expected := range want {
		if got := lb.getNextAvailableServer().Address(); got != expected {
			t.Fatalf("request %d: got %s, want %s", i, got, expected)
		}
	}
}

func TestSkipsServersThatAreDown(t *testing.T) {
	a, b, c := &fakeServer{"a", true}, &fakeServer{"b", false}, &fakeServer{"c", true}
	lb := NewLoadBalancer("0", []Server{a, b, c})

	for i := 0; i < 6; i++ {
		if got := lb.getNextAvailableServer().Address(); got == "b" {
			t.Fatalf("request %d was sent to a server that is down", i)
		}
	}
}

func TestReturns503WhenAllServersAreDown(t *testing.T) {
	lb := NewLoadBalancer("0", []Server{&fakeServer{"a", false}, &fakeServer{"b", false}})

	rec := httptest.NewRecorder()
	lb.serveProxy(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestHealthCheckDetectsBackendGoingDown(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}))
	server := NewSimpleServer(backend.URL)

	if !server.CheckHealth() {
		t.Fatal("a backend answering 200 should be alive")
	}

	backend.Close()
	if server.CheckHealth() {
		t.Fatal("a backend that is not reachable should be down")
	}
	if server.IsAlive() {
		t.Fatal("IsAlive should reflect the last health check")
	}
}

func TestHealthCheckTreats5xxAsDown(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	if NewSimpleServer(backend.URL).CheckHealth() {
		t.Fatal("a backend answering 500 should be down")
	}
}

func TestFailedRequestTakesBackendOutOfRotation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {}))
	server := NewSimpleServer(backend.URL)
	backend.Close()

	rec := httptest.NewRecorder()
	server.Serve(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if server.IsAlive() {
		t.Fatal("a backend that failed a request should be marked as down")
	}
}

func TestPeriodicHealthChecksBringBackendBack(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	server := NewSimpleServer(backend.URL)
	server.SetAlive(false)
	lb := NewLoadBalancer("0", []Server{server})

	stop := make(chan struct{})
	defer close(stop)
	lb.StartHealthChecks(10*time.Millisecond, stop)

	deadline := time.Now().Add(time.Second)
	for !server.IsAlive() {
		if time.Now().After(deadline) {
			t.Fatal("the health check did not mark the recovered backend as alive")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConcurrentSelectionIsBalanced(t *testing.T) {
	servers := []Server{&fakeServer{"a", true}, &fakeServer{"b", true}, &fakeServer{"c", true}}
	lb := NewLoadBalancer("0", servers)

	var mu sync.Mutex
	counts := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			address := lb.getNextAvailableServer().Address()
			mu.Lock()
			counts[address]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	for _, s := range servers {
		if counts[s.Address()] != 100 {
			t.Fatalf("uneven distribution: %v", counts)
		}
	}
}
