package nginxconf

import (
	"os"
	"reflect"
	"testing"
)

func parsed(t *testing.T) *Config {
	t.Helper()
	b, err := os.ReadFile("testdata/nginx-T.txt")
	if err != nil {
		t.Fatal(err)
	}
	return Parse(string(b))
}

func TestWorkerSettings(t *testing.T) {
	c := parsed(t)
	if c.WorkerProcesses != 4 || c.WorkerConnections != 1024 {
		t.Fatalf("workers = %d/%d", c.WorkerProcesses, c.WorkerConnections)
	}
}

func TestUpstreamMembers(t *testing.T) {
	c := parsed(t)
	want := []string{"10.1.1.100:8080", "10.1.1.101:8080"}
	if !reflect.DeepEqual(c.Upstreams["app_backend"], want) {
		t.Fatalf("members = %v", c.Upstreams["app_backend"])
	}
}

func TestServers(t *testing.T) {
	c := parsed(t)
	if len(c.Servers) != 3 {
		t.Fatalf("got %d servers", len(c.Servers))
	}

	a := c.Servers[0]
	if !reflect.DeepEqual(a.Names, []string{"a.example.com", "www.a.example.com"}) {
		t.Fatalf("names = %v", a.Names)
	}
	if len(a.Listens) != 2 || !a.Listens[0].TLS || !a.Listens[0].Default || a.Listens[0].Port != "443" {
		t.Fatalf("listens = %+v", a.Listens)
	}
	if got := c.Backends(a); !reflect.DeepEqual(got, []string{"10.1.1.100:8080", "10.1.1.101:8080"}) {
		t.Fatalf("backends = %v", got)
	}
	if a.File != "/etc/nginx/conf.d/site-a.conf" {
		t.Fatalf("file = %q", a.File)
	}

	b := c.Servers[1]
	if b.Listens[0].Addr != "10.0.0.5" || b.Listens[0].Port != "443" {
		t.Fatalf("listen = %+v", b.Listens[0])
	}
	// Literal proxy_pass: scheme and path stripped, not resolved via upstreams.
	if got := c.Backends(b); !reflect.DeepEqual(got, []string{"10.1.1.100:9090"}) {
		t.Fatalf("backends = %v", got)
	}

	legacy := c.Servers[2]
	if !reflect.DeepEqual(legacy.Names, []string{"_"}) || legacy.Pass != "" {
		t.Fatalf("legacy = %+v", legacy)
	}
	if got := c.Backends(legacy); got != nil {
		t.Fatalf("legacy backends = %v", got)
	}
}

func TestBackendsVariablePassIsDynamic(t *testing.T) {
	cfg := Parse(`
http {
    server {
        server_name dyn.example.com;
        listen 443 ssl;
        proxy_pass https://$lyftloyaltywallet;
    }
}`)
	if len(cfg.Servers) != 1 {
		t.Fatalf("servers = %d", len(cfg.Servers))
	}
	if got := cfg.Backends(cfg.Servers[0]); got != nil {
		t.Fatalf("variable pass target leaked as backend: %v", got)
	}
}
