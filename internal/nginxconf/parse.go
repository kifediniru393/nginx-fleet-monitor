// Package nginxconf parses `nginx -T` output into the intended routing
// topology: vhosts, listeners, upstream targets, and worker capacity settings.
package nginxconf

import (
	"strconv"
	"strings"
)

// Config is the parsed topology of one nginx instance.
type Config struct {
	WorkerProcesses   int // 0 if "auto" or absent
	WorkerConnections int
	Upstreams         map[string][]string // name -> member addrs
	Servers           []Server
}

// Server is one server{} block.
type Server struct {
	Names   []string // server_name values; ["_"] if absent
	Listens []Listen
	Pass    string // proxy_pass/fastcgi_pass/grpc_pass target, "" if none
	File    string
	Line    int
}

// Listen is one listen directive.
type Listen struct {
	Addr    string
	Port    string
	TLS     bool
	Default bool
}

// Backends resolves a server's pass target against the upstream blocks:
// either the members of a named upstream, or the literal address.
func (c *Config) Backends(s Server) []string {
	if s.Pass == "" {
		return nil
	}
	host := passHost(s.Pass)
	if members, ok := c.Upstreams[host]; ok {
		return members
	}
	return []string{host}
}

// passHost strips scheme and path from a pass target: "http://backend/x" -> "backend".
func passHost(pass string) string {
	h := pass
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/"); i >= 0 {
		h = h[:i]
	}
	return h
}

type token struct {
	text string
	file string
	line int
}

// Parse consumes `nginx -T` output (or a plain config with no file markers).
func Parse(text string) *Config {
	cfg := &Config{Upstreams: make(map[string][]string)}
	toks := tokenize(text)

	var stack []string // block names, e.g. [http, server]
	var cur *Server
	var curUpstream string

	i := 0
	directive := func() []token { // consume tokens up to ; { or }
		start := i
		for i < len(toks) && toks[i].text != ";" && toks[i].text != "{" && toks[i].text != "}" {
			i++
		}
		return toks[start:i]
	}

	for i < len(toks) {
		args := directive()
		if i >= len(toks) { // trailing junk
			break
		}
		sep := toks[i].text
		i++
		switch sep {
		case "{":
			name := ""
			if len(args) > 0 {
				name = args[0].text
			}
			stack = append(stack, name)
			switch name {
			case "server":
				// nginx -T dumps included files sequentially, so server{}
				// blocks from conf.d appear at top level of their file, not
				// nested under http{}. Accept any server block outside
				// upstream{} (upstream members are directives, not blocks).
				if !inBlock(stack[:len(stack)-1], "upstream") {
					cfg.Servers = append(cfg.Servers, Server{File: args[0].file, Line: args[0].line})
					cur = &cfg.Servers[len(cfg.Servers)-1]
				}
			case "upstream":
				if len(args) > 1 {
					curUpstream = args[1].text
					cfg.Upstreams[curUpstream] = nil
				}
			}
		case "}":
			if len(stack) > 0 {
				switch stack[len(stack)-1] {
				case "server":
					cur = nil
				case "upstream":
					curUpstream = ""
				}
				stack = stack[:len(stack)-1]
			}
		case ";":
			if len(args) == 0 {
				continue
			}
			name := args[0].text
			rest := args[1:]
			switch {
			case name == "worker_processes" && len(rest) == 1:
				cfg.WorkerProcesses, _ = strconv.Atoi(rest[0].text)
			case name == "worker_connections" && len(rest) == 1:
				cfg.WorkerConnections, _ = strconv.Atoi(rest[0].text)
			case name == "server" && curUpstream != "" && len(rest) > 0:
				cfg.Upstreams[curUpstream] = append(cfg.Upstreams[curUpstream], rest[0].text)
			case cur != nil && name == "server_name":
				for _, r := range rest {
					cur.Names = append(cur.Names, r.text)
				}
			case cur != nil && name == "listen" && len(rest) > 0:
				cur.Listens = append(cur.Listens, parseListen(rest))
			case cur != nil && (name == "proxy_pass" || name == "fastcgi_pass" || name == "grpc_pass") && len(rest) > 0:
				if cur.Pass == "" { // first pass target wins; location-level nuance is Phase 2
					cur.Pass = rest[0].text
				}
			}
		}
	}

	for i := range cfg.Servers {
		if len(cfg.Servers[i].Names) == 0 {
			cfg.Servers[i].Names = []string{"_"}
		}
	}
	return cfg
}

func parseListen(args []token) Listen {
	l := Listen{}
	spec := args[0].text
	if i := strings.LastIndex(spec, ":"); i >= 0 && !strings.Contains(spec[i+1:], "]") {
		l.Addr, l.Port = spec[:i], spec[i+1:]
	} else if _, err := strconv.Atoi(spec); err == nil {
		l.Port = spec
	} else {
		l.Addr = spec
		l.Port = "80"
	}
	for _, a := range args[1:] {
		switch a.text {
		case "ssl":
			l.TLS = true
		case "default_server":
			l.Default = true
		}
	}
	return l
}

func inBlock(stack []string, name string) bool {
	for _, s := range stack {
		if s == name {
			return true
		}
	}
	return false
}

// tokenize splits config text into tokens, tracking the current file from
// `nginx -T`'s "# configuration file /path:" markers and line numbers within it.
func tokenize(text string) []token {
	var toks []token
	file := ""
	lineNo := 0
	for _, line := range strings.Split(text, "\n") {
		lineNo++
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# configuration file ") {
			file = strings.TrimSuffix(strings.TrimPrefix(trimmed, "# configuration file "), ":")
			lineNo = 0
			continue
		}
		if i := strings.Index(trimmed, "#"); i >= 0 {
			trimmed = trimmed[:i]
		}
		for _, f := range splitConfigLine(trimmed) {
			toks = append(toks, token{text: f, file: file, line: lineNo})
		}
	}
	return toks
}

// splitConfigLine splits on whitespace but keeps ; { } as separate tokens.
func splitConfigLine(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch r {
		case ' ', '\t':
			flush()
		case ';', '{', '}':
			flush()
			out = append(out, string(r))
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}
