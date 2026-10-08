//go:build !linux

package main

import (
	"log"
	"syscall"
)

// disableCoreDumps keeps cached values out of core files.
func disableCoreDumps() {
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		log.Printf("disable core dumps: setrlimit: %v", err)
	}
}
