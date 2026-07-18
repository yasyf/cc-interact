// Package procs resolves a consumer's window identity. A process spawned
// inside a Claude Code window (hook child, MCP channel server, CLI) descends
// from that claude process; the nearest claude-matching ancestor pid is the
// window id, and 0 marks a process running outside any window.
package procs

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/shirou/gopsutil/v4/process"
)

const maxHops = 32

type lookup func(pid int32) (ppid int32, argv []string, err error)

var claudePID = sync.OnceValue(func() int {
	return FindAncestor(os.Getppid(), isClaude)
})

// ClaudePID returns the pid of the nearest claude ancestor of this process,
// resolved once per process (ancestry is frozen at spawn). 0 means not inside
// a Claude window — a defined state, not an error.
func ClaudePID() int { return claudePID() }

// LiveClaude reports whether pid exists and its argv still matches claude,
// defeating pid recycling across reboots.
func LiveClaude(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := process.NewProcess(int32(pid)) //nolint:gosec // G115: OS PIDs are bounded well below int32 max (Linux PID_MAX_LIMIT 2^22, macOS 99999) and pid is already guarded > 0 above.
	if err != nil {
		return false
	}
	argv, err := p.CmdlineSlice()
	if err != nil {
		return false
	}
	return isClaude(argv)
}

// FindAncestor walks the parent chain starting at pid (inclusive) and returns
// the first pid whose argv satisfies match; 0 when nothing matches.
func FindAncestor(pid int, match func(argv []string) bool) int {
	return int(walk(int32(pid), procLookup, match)) //nolint:gosec // G115: OS PIDs are bounded well below int32 max (Linux PID_MAX_LIMIT 2^22, macOS 99999).
}

func walk(pid int32, lk lookup, match func(argv []string) bool) int32 {
	seen := map[int32]bool{}
	for hops := 0; hops < maxHops && pid > 1 && !seen[pid]; hops++ {
		seen[pid] = true
		ppid, argv, err := lk(pid)
		if err != nil {
			return 0
		}
		if match(argv) {
			return pid
		}
		pid = ppid
	}
	return 0
}

func procLookup(pid int32) (int32, []string, error) {
	p, err := process.NewProcess(pid)
	if err != nil {
		return 0, nil, fmt.Errorf("open pid %d: %w", pid, err)
	}
	ppid, err := p.Ppid()
	if err != nil {
		return 0, nil, fmt.Errorf("ppid of pid %d: %w", pid, err)
	}
	argv, err := p.CmdlineSlice()
	if err != nil {
		return 0, nil, fmt.Errorf("argv of pid %d: %w", pid, err)
	}
	return ppid, argv, nil
}

func isClaude(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	switch filepath.Base(argv[0]) {
	case "claude":
		return true
	case "node", "bun":
		if len(argv) > 1 {
			script := filepath.Base(argv[1])
			return script == "claude" || script == "claude.js"
		}
	}
	return false
}
