package vcs

import (
	"encoding/json"
	"testing"
)

func TestParseFiles(t *testing.T) {
	t.Run("empty patch marshals to an empty array", func(t *testing.T) {
		files, err := parseFiles("")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if files == nil {
			t.Fatal("files is a nil slice; it must marshal to [] not null")
		}
		b, err := json.Marshal(files)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(b) != "[]" {
			t.Fatalf("marshal = %s, want []", b)
		}
	})

	t.Run("one added file", func(t *testing.T) {
		patch := "diff --git a/a.txt b/a.txt\nnew file mode 100644\n--- /dev/null\n+++ b/a.txt\n@@ -0,0 +1,1 @@\n+hi\n"
		files, err := parseFiles(patch)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(files) != 1 || files[0].Path != "a.txt" || files[0].Status != "A" || files[0].Fingerprint == "" {
			t.Fatalf("files = %+v, want a.txt added with a fingerprint", files)
		}
	})
}
