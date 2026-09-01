package main

import "syscall"

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetTickCount64 = kernel32.NewProc("GetTickCount64")
)

func uptimeSeconds() int64 {
	ms, _, _ := procGetTickCount64.Call()
	return int64(ms) / 1000
}
