package curlparse

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name, in                string
		method, url, body       string
		header                  map[string]string
		insecure                bool
		followRedirects         bool
		errContains             string
	}{
		{name: "bare get", in: "curl http://localhost:8080/api/health",
			method: "GET", url: "http://localhost:8080/api/health"},
		{name: "post json", in: `curl -X POST 'http://h/api' -H 'Content-Type: application/json' -d '{"a":1}'`,
			method: "POST", url: "http://h/api", body: `{"a":1}`,
			header: map[string]string{"Content-Type": "application/json"}},
		{name: "data implies post", in: `curl http://h/x -d 'k=v'`,
			method: "POST", url: "http://h/x", body: "k=v"},
		{name: "basic auth", in: `curl -u alice:s3cret http://h/x`,
			method: "GET", url: "http://h/x",
			header: map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))}},
		{name: "insecure", in: `curl -k https://h/x`, method: "GET", url: "https://h/x", insecure: true},
		{name: "location short", in: `curl -L http://h/x`, method: "GET", url: "http://h/x", followRedirects: true},
		{name: "location long", in: `curl --location http://h/x`, method: "GET", url: "http://h/x", followRedirects: true},
		{name: "no location by default", in: `curl http://h/x`, method: "GET", url: "http://h/x", followRedirects: false},
		{name: "backslash-newline line continuations", in: "curl --location 'http://h/x' \\\n--header 'X-API-Key: secret' \\\n--header 'Content-Type: application/json' \\\n--data '[\n  {\n    \"name\": \"tester\"\n  }\n]'",
			method: "POST", url: "http://h/x", body: "[\n  {\n    \"name\": \"tester\"\n  }\n]",
			header: map[string]string{"X-API-Key": "secret", "Content-Type": "application/json"}, followRedirects: true},
		{name: "literal backslash-newline inside quoted data survives", in: "curl http://h/x -d 'line1\\\nline2'",
			method: "POST", url: "http://h/x", body: "line1\\\nline2"},
		{name: "no curl prefix", in: `http://h/x`, method: "GET", url: "http://h/x"},
		{name: "scheme defaults to http", in: `curl localhost:8099/x`,
			method: "GET", url: "http://localhost:8099/x"},
		{name: "unsupported flag", in: `curl --compressed http://h/x`, errContains: "--compressed"},
		{name: "missing url", in: `curl -X POST`, errContains: "URL"},
		{name: "bad quoting", in: `curl 'http://h/x`, errContains: ""}, // any error acceptable
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.in)
			if c.errContains != "" || c.name == "bad quoting" {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), c.errContains) {
					t.Fatalf("error %q does not contain %q", err, c.errContains)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Method != c.method || got.URL != c.url || string(got.Body) != c.body || got.Insecure != c.insecure || got.FollowRedirects != c.followRedirects {
				t.Fatalf("got %+v", got)
			}
			for k, v := range c.header {
				if got.Header.Get(k) != v {
					t.Fatalf("header %s = %q, want %q", k, got.Header.Get(k), v)
				}
			}
		})
	}
}
