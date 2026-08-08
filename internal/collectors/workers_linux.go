//go:build linux

package collectors

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// listWorkers scans /proc for nginx worker processes.
func listWorkers() []workerStats {
	var out []workerStats
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || !strings.HasPrefix(string(cmdline), "nginx: worker process") {
			continue
		}
		w := workerStats{pid: pid}
		if fds, err := os.ReadDir(filepath.Join("/proc", e.Name(), "fd")); err == nil {
			w.fdsOpen = len(fds)
		}
		w.fdsLimit = readFDLimit(e.Name())
		w.cpuSeconds, w.rssBytes = readStat(e.Name())
		out = append(out, w)
	}
	return out
}

func readFDLimit(pid string) int {
	b, err := os.ReadFile(filepath.Join("/proc", pid, "limits"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "Max open files") {
			f := strings.Fields(line)
			if len(f) >= 4 {
				n, _ := strconv.Atoi(f[3]) // soft limit
				return n
			}
		}
	}
	return 0
}

func readStat(pid string) (cpuSeconds float64, rssBytes float64) {
	b, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return 0, 0
	}
	// Fields after the parenthesized comm; comm can contain spaces.
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0, 0
	}
	f := strings.Fields(s[i+1:])
	// After comm: field 0 = state ... utime is field 11, stime 12, rss 21 (0-indexed here).
	if len(f) > 21 {
		utime, _ := strconv.ParseFloat(f[11], 64)
		stime, _ := strconv.ParseFloat(f[12], 64)
		rssPages, _ := strconv.ParseFloat(f[21], 64)
		return (utime + stime) / 100.0, rssPages * float64(os.Getpagesize())
	}
	return 0, 0
}
