package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ToolFilePath returns the file an edit tool's raw input names: file_path for
// Edit and Write, notebook_path for NotebookEdit, or "" when it names neither.
func ToolFilePath(input json.RawMessage) string {
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	_ = json.Unmarshal(input, &in)
	if in.FilePath != "" {
		return in.FilePath
	}
	return in.NotebookPath
}

// EditDir returns the nearest existing directory above the file an edit tool's
// raw input names, or "" when it names no file.
func EditDir(input json.RawMessage) string {
	path := ToolFilePath(input)
	if path == "" {
		return ""
	}
	dir := filepath.Dir(path)
	for {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}
