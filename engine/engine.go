package engine

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"

	"apiperf/curlparse"
	"apiperf/metrics"
)

type ProbeResult struct {
	Status    int     `json:"status"`
	LatencyMs int64   `json:"latencyMs"`
	Bytes     int     `json:"bytes"`
	Snippet   string  `json:"snippet"` // first 512 bytes of body
	Error     string  `json:"error,omitempty"`
	Proto     string  `json:"proto,omitempty"`
	Cold      *Phases `json:"cold,omitempty"` // first request, new connection
	Warm      *Phases `json:"warm,omitempty"` // repeat on the same connection; nil if it failed
}

// Run fires exactly n requests at fixed concurrency c (closed loop: rate
// unlimited, workers capped). Emits one metrics.Sample per request on the
// returned channel; closes it when done or when ctx is cancelled.
// OffsetMs is measured from attack start.
// On ctx cancellation the channel still closes and remaining in-flight
// results are discarded; callers may stop reading after cancel.
func Run(ctx context.Context, t curlparse.Target, n, c int, timeout time.Duration) <-chan metrics.Sample {
	out := make(chan metrics.Sample, 1024)
	if n < 1 {
		close(out)
		return out
	}
	if c < 1 {
		c = 1
	}

	vt := vegeta.Target{Method: t.Method, URL: t.URL, Body: t.Body, Header: t.Header}
	var issued int64
	targeter := func(tgt *vegeta.Target) error {
		if atomic.AddInt64(&issued, 1) > int64(n) {
			return vegeta.ErrNoTargets
		}
		*tgt = vt
		return nil
	}

	// Own the transport so its idle connections can be closed when the run
	// finishes, instead of leaking sockets for the process lifetime.
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: c,
	}
	if t.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	opts := []func(*vegeta.Attacker){
		vegeta.Client(&http.Client{Transport: tr, Timeout: timeout}),
		vegeta.Workers(uint64(c)),
		vegeta.MaxWorkers(uint64(c)),
		vegeta.MaxBody(0), // apiperf never reads response bodies
	}
	// Redirects must be set after Client: it sets CheckRedirect on the
	// attacker's http.Client, which vegeta.Client() would otherwise replace.
	if t.FollowRedirects {
		opts = append(opts, vegeta.Redirects(vegeta.DefaultRedirects))
	} else {
		opts = append(opts, vegeta.Redirects(vegeta.NoFollow))
	}
	attacker := vegeta.NewAttacker(opts...)

	go func() {
		defer close(out)
		defer tr.CloseIdleConnections()
		start := time.Now()
		results := attacker.Attack(targeter, vegeta.Rate{Freq: 0, Per: 0}, 0, "apiperf")
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				attacker.Stop()
			case <-done:
			}
		}()
		defer close(done)

		sent := 0
		cancelled := false
		for res := range results {
			if res.Error == vegeta.ErrNoTargets.Error() {
				continue
			}
			// vegeta sets res.Error to the status text (e.g. "503 Service
			// Unavailable") for any non-2xx response. That's not a transport
			// error, so don't propagate it as Sample.Error — only genuine
			// transport failures (no status code) count as errors here.
			errStr := ""
			if res.Code == 0 {
				errStr = res.Error
			}
			s := metrics.Sample{
				OffsetMs:  res.Timestamp.Sub(start).Milliseconds(),
				LatencyUs: res.Latency.Microseconds(),
				Code:      int(res.Code),
				Error:     errStr,
				Timeout:   errStr != "" && (strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline")),
			}
			sent++
			if sent >= n {
				attacker.Stop()
			}
			if cancelled {
				continue
			}
			select {
			case out <- s:
			case <-ctx.Done():
				cancelled = true
			}
		}
	}()
	return out
}

// Phases breaks one request's latency into its network steps (ms).
// Steps skipped on a reused connection (DNS, connect, TLS) stay 0.
type Phases struct {
	DNSMs     float64 `json:"dnsMs"`
	ConnectMs float64 `json:"connectMs"`
	TLSMs     float64 `json:"tlsMs"`
	SendMs    float64 `json:"sendMs"`
	WaitMs    float64 `json:"waitMs"` // server processing: request written → first byte
	RecvMs    float64 `json:"recvMs"`
	Reused    bool    `json:"reused"`
}

// Probe sends one real request on a new connection ("cold"), then repeats
// it on the same connection ("warm"), timing each step of both.
func Probe(t curlparse.Target, timeout time.Duration) ProbeResult {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	defer tr.CloseIdleConnections()
	if t.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &http.Client{Transport: tr, Timeout: timeout}
	if !t.FollowRedirects {
		// Match curl's default: don't follow redirects unless -L was given.
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	start := time.Now()
	res, body, cold, err := timedDo(client, t)
	if err != nil {
		return ProbeResult{Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()}
	}
	snip := body
	if len(snip) > 512 {
		snip = snip[:512]
	}
	p := ProbeResult{
		Status: res.StatusCode, LatencyMs: time.Since(start).Milliseconds(),
		Bytes: len(body), Snippet: string(snip), Proto: res.Proto, Cold: &cold,
	}
	if _, _, warm, err := timedDo(client, t); err == nil {
		p.Warm = &warm
	}
	return p
}

// timedDo performs one request with an httptrace hooked in. It returns the
// first 64KB of the body; the rest is drained so the connection can be reused.
// ponytail: with -L, phases cover every hop combined (curl's -w does the same).
func timedDo(client *http.Client, t curlparse.Target) (*http.Response, []byte, Phases, error) {
	var p Phases
	var dnsStart, connStart, tlsStart, gotConn, wrote, firstByte time.Time
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { p.DNSMs += ms(time.Since(dnsStart)) },
		ConnectStart:         func(string, string) { connStart = time.Now() },
		ConnectDone:          func(string, string, error) { p.ConnectMs += ms(time.Since(connStart)) },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { p.TLSMs += ms(time.Since(tlsStart)) },
		GotConn:              func(i httptrace.GotConnInfo) { gotConn, p.Reused = time.Now(), i.Reused },
		WroteRequest:         func(httptrace.WroteRequestInfo) { wrote = time.Now(); p.SendMs += ms(wrote.Sub(gotConn)) },
		GotFirstResponseByte: func() { firstByte = time.Now(); p.WaitMs += ms(firstByte.Sub(wrote)) },
	}
	req, err := http.NewRequest(t.Method, t.URL, bytes.NewReader(t.Body))
	if err != nil {
		return nil, nil, p, err
	}
	req.Header = t.Header.Clone()
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, p, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	io.Copy(io.Discard, io.LimitReader(res.Body, 10<<20))
	p.RecvMs = ms(time.Since(firstByte))
	return res, body, p, nil
}

// IsPrivateHost reports whether every resolved IP of the URL's host is
// loopback/private/link-local. Unresolvable counts as NOT private.
func IsPrivateHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
			return false
		}
	}
	return true
}
