// Package sysinfo reads the machine resources the scheduler guards on.
package sysinfo

import (
	"os"
	"strconv"
	"strings"
)

// MemAvailableMB reads MemAvailable from /proc/meminfo.
func MemAvailableMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemAvailable:"); ok {
			kb, _ := strconv.Atoi(strings.Fields(rest)[0])
			return kb / 1024
		}
	}
	return 0
}

// Load1 reads the 1-minute load average.
func Load1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.Fields(string(b))[0], 64)
	return f
}

// MemPressure reads "some avg10" from /proc/pressure/memory (0 if absent).
func MemPressure() float64 {
	b, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return 0
	}
	for _, field := range strings.Fields(strings.SplitN(string(b), "\n", 2)[0]) {
		if v, ok := strings.CutPrefix(field, "avg10="); ok {
			f, _ := strconv.ParseFloat(v, 64)
			return f
		}
	}
	return 0
}
