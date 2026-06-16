package vcs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

func parseFiles(patch string) ([]FileChange, error) {
	parsed, _, err := gitdiff.Parse(strings.NewReader(patch))
	if err != nil {
		return nil, fmt.Errorf("parse patch: %w", err)
	}
	out := make([]FileChange, 0, len(parsed))
	for _, f := range parsed {
		var fc FileChange
		switch {
		case f.IsNew:
			fc = FileChange{Path: f.NewName, Status: "A"}
		case f.IsDelete:
			fc = FileChange{Path: f.OldName, Status: "D"}
		case f.IsRename:
			fc = FileChange{Path: f.NewName, OldPath: f.OldName, Status: "R"}
		case f.IsCopy:
			fc = FileChange{Path: f.NewName, OldPath: f.OldName, Status: "C"}
		default:
			fc = FileChange{Path: f.NewName, Status: "M"}
		}
		fc.Fingerprint = fingerprint(f, fc.Status)
		out = append(out, fc)
	}
	return out, nil
}

// fingerprint hashes a deterministic serialization of one parsed file's diff:
// status, old/new names, old/new modes, and every fragment header + line. It is
// derived purely from the snapshot patch, so it is identical for the git and jj
// backends and stable across no-op recaptures.
func fingerprint(f *gitdiff.File, status string) string {
	h := sha256.New()
	field := func(s string) {
		io.WriteString(h, s)
		h.Write([]byte{0})
	}
	field(status)
	field(f.OldName)
	field(f.NewName)
	fmt.Fprintf(h, "%o\x00%o\x00", f.OldMode, f.NewMode)
	for _, frag := range f.TextFragments {
		fmt.Fprintf(h, "@%d,%d,%d,%d\x00", frag.OldPosition, frag.OldLines, frag.NewPosition, frag.NewLines)
		for _, line := range frag.Lines {
			io.WriteString(h, line.Op.String())
			field(line.Line)
		}
	}
	if f.IsBinary {
		// Binary patches carry no fragments; the index-line OIDs are the content.
		field(f.OldOIDPrefix)
		field(f.NewOIDPrefix)
	}
	return hex.EncodeToString(h.Sum(nil))
}
