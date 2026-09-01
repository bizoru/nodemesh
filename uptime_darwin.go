package main

import (
	"regexp"
	"strconv"
	"time"
)

var bootRe = regexp.MustCompile(`sec = (\d+)`)

func uptimeSeconds() int64 {
	out := run("sysctl", "-n", "kern.boottime") // { sec = 1755..., usec = ... } ...
	if m := bootRe.FindStringSubmatch(out); m != nil {
		if sec, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return time.Now().Unix() - sec
		}
	}
	return 0
}
