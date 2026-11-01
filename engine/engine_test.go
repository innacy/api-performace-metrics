package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"apiperf/curlparse"
)

func TestRunFiresExactlyN(t *testing.T) {
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	ch := Run(context.Background(), curlparse.Target{Method: "GET", URL: ts.URL, Header: http.Header{}}, 50, 5, 5*time.Second)
	var got int
	for s := range ch {
		if s.Error != "" {
			t.Fatalf("unexpected error sample: %q", s.Error)
		}
		if s.Code != 200 {
			t.Fatalf("code = %d", s.Code)
		}
		got++
	}
	if got != 50 {
		t.Fatalf("samples = %d, want 50", got)
	}
	if hits.Load() != 50 {
		t.Fatalf("server hits = %d, want 50", hits.Load())
	}
}

func TestRunCancel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch := Run(ctx, curlparse.Target{Method: "GET", URL: ts.URL, Header: http.Header{}}, 100000, 5, 5*time.Second)
	n := 0
	for range ch {
		n++
		if n == 10 {
			cancel()
		}
	}
	if n >= 100000 {
		t.Fatal("cancel did not stop the attack")
	}
}

func TestProbe(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte("hello"))
	}))
	defer ts.Close()

	p := Probe(curlparse.Target{Method: "GET", URL: ts.URL, Header: http.Header{}}, 2*time.Second)
	if p.Status != 201 || p.Snippet != "hello" || p.Error != "" {
		t.Fatalf("probe = %+v", p)
	}

	p = Probe(curlparse.Target{Method: "GET", URL: "http://127.0.0.1:1", Header: http.Header{}}, time.Second)
	if p.Error == "" {
		t.Fatal("expected connection error")
	}
}

func TestProbeRedirects(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("final"))
	}))
	defer final.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redir.Close()

	p := Probe(curlparse.Target{Method: "GET", URL: redir.URL, Header: http.Header{}, FollowRedirects: false}, 2*time.Second)
	if p.Status != http.StatusFound {
		t.Fatalf("without -L: status = %d, want 302 (curl does not follow redirects by default)", p.Status)
	}

	p = Probe(curlparse.Target{Method: "GET", URL: redir.URL, Header: http.Header{}, FollowRedirects: true}, 2*time.Second)
	if p.Status != 200 || p.Snippet != "final" {
		t.Fatalf("with -L: probe = %+v, want status 200 body \"final\"", p)
	}
}

func TestProbeTiming(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	p := Probe(curlparse.Target{Method: "GET", URL: ts.URL, Header: http.Header{}, Insecure: true}, 2*time.Second)
	if p.Cold == nil || p.Warm == nil {
		t.Fatalf("want cold+warm phases, got %+v", p)
	}
	if p.Cold.Reused || p.Cold.TLSMs <= 0 || p.Cold.ConnectMs <= 0 {
		t.Fatalf("cold should open a new TLS connection: %+v", *p.Cold)
	}
	if !p.Warm.Reused || p.Warm.TLSMs != 0 || p.Warm.ConnectMs != 0 {
		t.Fatalf("warm should reuse the connection: %+v", *p.Warm)
	}
	if p.Cold.WaitMs < 20 || p.Warm.WaitMs < 20 {
		t.Fatalf("server processing should include the 20ms sleep: cold=%v warm=%v", p.Cold.WaitMs, p.Warm.WaitMs)
	}
}

func TestRunRedirects(t *testing.T) {
	var finalHits, redirHits atomic.Int64
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finalHits.Add(1)
		w.WriteHeader(200)
	}))
	defer final.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirHits.Add(1)
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redir.Close()

	noFollow := Run(context.Background(), curlparse.Target{Method: "GET", URL: redir.URL, Header: http.Header{}, FollowRedirects: false}, 5, 2, 5*time.Second)
	for s := range noFollow {
		if s.Code != http.StatusFound {
			t.Fatalf("without -L: code = %d, want 302", s.Code)
		}
	}
	if finalHits.Load() != 0 {
		t.Fatalf("without -L: final server should never be hit, got %d hits", finalHits.Load())
	}

	follow := Run(context.Background(), curlparse.Target{Method: "GET", URL: redir.URL, Header: http.Header{}, FollowRedirects: true}, 5, 2, 5*time.Second)
	for s := range follow {
		if s.Code != 200 {
			t.Fatalf("with -L: code = %d, want 200 (final server response)", s.Code)
		}
	}
	if finalHits.Load() != 5 {
		t.Fatalf("with -L: final server hits = %d, want 5", finalHits.Load())
	}
}

func TestIsPrivateHost(t *testing.T) {
	if !IsPrivateHost("http://127.0.0.1:8080/x") {
		t.Error("loopback should be private")
	}
	if !IsPrivateHost("http://192.168.1.10/x") {
		t.Error("rfc1918 should be private")
	}
	if IsPrivateHost("http://8.8.8.8/x") {
		t.Error("public IP should not be private")
	}
	if !IsPrivateHost("http://localhost:7071/x") {
		t.Error("localhost should resolve to loopback and be private")
	}
	if IsPrivateHost("http://no-such-host-apiperf.invalid/x") {
		t.Error("unresolvable host should not be private")
	}
}
