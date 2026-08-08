package nginxconf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromDiskResolvesIncludes(t *testing.T) {
	dir := t.TempDir()
	confd := filepath.Join(dir, "conf.d")
	os.Mkdir(confd, 0o755)
	os.WriteFile(filepath.Join(dir, "nginx.conf"), []byte(`
worker_processes 2;
events { worker_connections 512; }
http {
    include conf.d/*.conf;
}
`), 0o644)
	os.WriteFile(filepath.Join(confd, "site.conf"), []byte(`
server {
    listen 443 ssl;
    server_name x.example.com;
    location / { proxy_pass http://10.0.0.9:8080; }
}
`), 0o644)

	text, err := LoadFromDisk(filepath.Join(dir, "nginx.conf"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Parse(text)
	if cfg.WorkerProcesses != 2 || cfg.WorkerConnections != 512 {
		t.Fatalf("workers = %d/%d", cfg.WorkerProcesses, cfg.WorkerConnections)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Names[0] != "x.example.com" {
		t.Fatalf("servers = %+v", cfg.Servers)
	}
	if cfg.Servers[0].File != filepath.Join(confd, "site.conf") {
		t.Fatalf("file attribution = %q", cfg.Servers[0].File)
	}
}

func TestLoadFromDiskIncludeCycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.conf")
	os.WriteFile(path, []byte("include "+path+";\nworker_processes 1;\n"), 0o644)
	text, err := LoadFromDisk(path)
	if err != nil {
		t.Fatal(err)
	}
	if Parse(text).WorkerProcesses != 1 {
		t.Fatal("cycle guard broke parsing")
	}
}
