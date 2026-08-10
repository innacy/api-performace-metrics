package engine

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"

	"apiperf/curlparse"
	"apiperf/metrics"
)

type ProbeResult struct {
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latencyMs"`
	Bytes     int    `json:"bytes"`
	Snippet   string `json:"snippet"` // first 512 bytes of body
	Error     string `json:"error,omitempty"`
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

// Probe sends ONE real request (plain net/http) and reports what happened.
func Probe(t curlparse.Target, timeout time.Duration) ProbeResult {
	client := &http.Client{Timeout: timeout}
	if t.Insecure {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	if !t.FollowRedirects {
		// Match curl's default: don't follow redirects unless -L was given.
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	req, err := http.NewRequest(t.Method, t.URL, strings.NewReader(string(t.Body)))
	if err != nil {
		return ProbeResult{Error: err.Error()}
	}
	req.Header = t.Header.Clone()
	start := time.Now()
	res, err := client.Do(req)
	if err != nil {
		return ProbeResult{Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()}
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	snip := body
	if len(snip) > 512 {
		snip = snip[:512]
	}
	return ProbeResult{
		Status: res.StatusCode, LatencyMs: time.Since(start).Milliseconds(),
		Bytes: len(body), Snippet: string(snip),
	}
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
