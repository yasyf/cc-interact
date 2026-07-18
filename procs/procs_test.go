package procs

import (
	"fmt"
	"os"
	"testing"
)

type fakeProc struct {
	ppid int32
	argv []string
}

func fakeLookup(table map[int32]fakeProc) lookup {
	return func(pid int32) (int32, []string, error) {
		p, ok := table[pid]
		if !ok {
			return 0, nil, fmt.Errorf("no such pid %d", pid)
		}
		return p.ppid, p.argv, nil
	}
}

func TestWalk(t *testing.T) {
	cases := []struct {
		name  string
		table map[int32]fakeProc
		start int32
		want  int32
	}{
		{
			name: "direct claude parent",
			table: map[int32]fakeProc{
				200: {ppid: 1, argv: []string{"/usr/local/bin/claude", "--resume"}},
			},
			start: 200,
			want:  200,
		},
		{
			name: "claude two shells up",
			table: map[int32]fakeProc{
				200: {ppid: 300, argv: []string{"/bin/fish"}},
				300: {ppid: 400, argv: []string{"/bin/zsh", "-c", "cc-tool watch"}},
				400: {ppid: 1, argv: []string{"claude"}},
			},
			start: 200,
			want:  400,
		},
		{
			name: "node shim",
			table: map[int32]fakeProc{
				200: {ppid: 1, argv: []string{"/usr/local/bin/node", "/usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude"}},
			},
			start: 200,
			want:  200,
		},
		{
			name: "bun shim",
			table: map[int32]fakeProc{
				200: {ppid: 1, argv: []string{"/opt/homebrew/bin/bun", "/Users/me/.bun/install/global/node_modules/.bin/claude.js"}},
			},
			start: 200,
			want:  200,
		},
		{
			name: "claude hook path does not match",
			table: map[int32]fakeProc{
				200: {ppid: 300, argv: []string{"/bin/bash", "/Users/me/.claude/hooks/foo.sh"}},
				300: {ppid: 1, argv: []string{"/bin/fish"}},
			},
			start: 200,
			want:  0,
		},
		{
			name: "no claude ancestor",
			table: map[int32]fakeProc{
				200: {ppid: 300, argv: []string{"/bin/fish"}},
				300: {ppid: 1, argv: []string{"/usr/bin/login"}},
			},
			start: 200,
			want:  0,
		},
		{
			name: "lookup error mid-walk",
			table: map[int32]fakeProc{
				200: {ppid: 300, argv: []string{"/bin/fish"}},
				400: {ppid: 1, argv: []string{"claude"}},
			},
			start: 200,
			want:  0,
		},
		{
			name: "ppid cycle",
			table: map[int32]fakeProc{
				200: {ppid: 300, argv: []string{"/bin/fish"}},
				300: {ppid: 200, argv: []string{"/bin/zsh"}},
			},
			start: 200,
			want:  0,
		},
		{
			name: "nested claude nearest wins",
			table: map[int32]fakeProc{
				200: {ppid: 300, argv: []string{"/bin/fish"}},
				300: {ppid: 400, argv: []string{"claude", "--resume"}},
				400: {ppid: 500, argv: []string{"/bin/zsh"}},
				500: {ppid: 1, argv: []string{"claude"}},
			},
			start: 200,
			want:  300,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := walk(tc.start, fakeLookup(tc.table), isClaude); got != tc.want {
				t.Errorf("walk(%d) = %d, want %d", tc.start, got, tc.want)
			}
		})
	}
}

func TestProcLookupRealOS(t *testing.T) {
	ppid, argv, err := procLookup(int32(os.Getpid())) //nolint:gosec // G115: os.Getpid() returns this process's PID, bounded well below int32 max.
	if err != nil {
		t.Fatalf("procLookup(%d): %v", os.Getpid(), err)
	}
	if want := int32(os.Getppid()); ppid != want { //nolint:gosec // G115: os.Getppid() returns the parent PID, bounded well below int32 max.
		t.Errorf("ppid = %d, want %d", ppid, want)
	}
	if len(argv) == 0 {
		t.Error("argv is empty, want test binary argv")
	}
}
