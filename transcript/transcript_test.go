package transcript

import (
	"path/filepath"
	"testing"
)

func TestSubagentsDir(t *testing.T) {
	tests := []struct {
		name              string
		sessionTranscript string
		want              string
	}{
		{
			name:              "session transcript",
			sessionTranscript: filepath.Join("projects", "slug", "39bd1158.jsonl"),
			want:              filepath.Join("projects", "slug", "39bd1158", "subagents"),
		},
		{
			name:              "absolute session transcript",
			sessionTranscript: filepath.Join(string(filepath.Separator), "home", "me", ".claude", "projects", "slug", "session.jsonl"),
			want:              filepath.Join(string(filepath.Separator), "home", "me", ".claude", "projects", "slug", "session", "subagents"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SubagentsDir(tt.sessionTranscript); got != tt.want {
				t.Fatalf("SubagentsDir() = %q, want %q", got, tt.want)
			}
		})
	}
}
