package main

import (
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Pod is a live listening TCP service on this node (not chained — realtime).
type Pod struct {
	Port    int    `json:"port"`
	Process string `json:"process"`
}

var (
	lsofRe    = regexp.MustCompile(`(?m)^(\S+)\s+\d+\s+\S+\s+\d+u\s+IPv\d\s+\S+\s+\S+\s+TCP\s+(\S+):(\d+)\s+\(LISTEN\)`)
	ssRe      = regexp.MustCompile(`(?m)^LISTEN\s+\S+\s+\S+\s+(\S+):(\d+)\s+\S+\s+users:\(\("([^"]+)"`)
	netstatRe = regexp.MustCompile(`(?m)^\s*TCP\s+(\S+):(\d+)\s+\S+\s+LISTENING\s+(\d+)`)
)

func listPods() []Pod {
	seen := map[int]string{}
	add := func(port int, proc string) {
		if port <= 0 || port > 65535 {
			return
		}
		if _, ok := seen[port]; !ok || seen[port] == "" {
			seen[port] = proc
		}
	}
	switch runtime.GOOS {
	case "darwin":
		for _, m := range lsofRe.FindAllStringSubmatch(run("lsof", "-iTCP", "-sTCP:LISTEN", "-P", "-n"), -1) {
			p, _ := strconv.Atoi(m[3])
			add(p, m[1])
		}
	case "linux":
		for _, m := range ssRe.FindAllStringSubmatch(run("ss", "-tlnp"), -1) {
			p, _ := strconv.Atoi(m[2])
			add(p, m[3])
		}
	case "windows":
		pidNames := map[string]string{}
		for _, line := range strings.Split(run("tasklist", "/fo", "csv", "/nh"), "\n") {
			f := strings.Split(line, "\",\"")
			if len(f) >= 2 {
				pidNames[strings.Trim(f[1], "\"")] = strings.Trim(f[0], "\"")
			}
		}
		for _, m := range netstatRe.FindAllStringSubmatch(run("netstat", "-ano", "-p", "TCP"), -1) {
			p, _ := strconv.Atoi(m[2])
			add(p, strings.TrimSuffix(pidNames[m[3]], ".exe"))
		}
	}
	out := make([]Pod, 0, len(seen))
	for port, proc := range seen {
		out = append(out, Pod{Port: port, Process: proc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func portsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, listPods())
}
