package review

import (
	"strconv"
	"strings"

	"github.com/jR4dh3y/samik-bot/internal/gh"
)

// DiffIndex knows which (path, side, line) triples are part of the PR diff,
// so inline comments are only posted where GitHub can pin them.
type DiffIndex struct {
	files map[string]*fileLines
}

type fileLines struct {
	right map[int64]bool // new-file line numbers
	left  map[int64]bool // old-file line numbers
}

// NewDiffIndex builds the index from GitHub's PR files API patches.
func NewDiffIndex(files []gh.File) *DiffIndex {
	idx := &DiffIndex{files: map[string]*fileLines{}}
	for _, f := range files {
		filename, ok := normalizePath(f.Filename)
		if !ok || f.Patch == "" {
			continue
		}
		fl := idx.file(filename)
		parsePatch(f.Patch, fl)
	}
	return idx
}

func (d *DiffIndex) file(path string) *fileLines {
	if fl, ok := d.files[path]; ok {
		return fl
	}
	fl := &fileLines{right: map[int64]bool{}, left: map[int64]bool{}}
	d.files[path] = fl
	return fl
}

// InDiff reports whether a line of path is part of the diff on the given
// side ("RIGHT" for new lines, "LEFT" for old lines).
func (d *DiffIndex) InDiff(path, side string, line int64) bool {
	if line < 1 {
		return false
	}
	path, ok := normalizePath(path)
	if !ok {
		return false
	}
	side, ok = normalizeSide(side)
	if !ok {
		return false
	}
	fl, ok := d.files[path]
	if !ok {
		return false
	}
	if side == "LEFT" {
		return fl.left[line]
	}
	return fl.right[line]
}

// parsePatch walks a unified diff and records present line numbers per side.
func parsePatch(patch string, fl *fileLines) {
	var oldLine, newLine int64
	inHunk := false
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			old, new, ok := parseHunk(line)
			if !ok {
				inHunk = false
				continue
			}
			oldLine, newLine = old, new
			inHunk = true
		case !inHunk, strings.HasPrefix(line, "\\"):
			// skip headers and "\ No newline" markers
		case strings.HasPrefix(line, "+"):
			fl.right[newLine] = true
			newLine++
		case strings.HasPrefix(line, "-"):
			fl.left[oldLine] = true
			oldLine++
		default: // context line
			fl.right[newLine] = true
			fl.left[oldLine] = true
			newLine++
			oldLine++
		}
	}
}

// parseHunk reads "@@ -12,7 +13,9 @@" and returns the start lines.
func parseHunk(header string) (old, new int64, ok bool) {
	parts := strings.Split(header, "@@")
	if len(parts) < 2 {
		return 0, 0, false
	}
	fields := strings.Fields(parts[1])
	if len(fields) < 2 {
		return 0, 0, false
	}
	old, ok = parseHunkSide(fields[0])
	if !ok {
		return 0, 0, false
	}
	new, ok = parseHunkSide(fields[1])
	if !ok {
		return 0, 0, false
	}
	return old, new, true
}

// parseHunkSide reads one of "-12,7" / "+13" and returns its start line.
func parseHunkSide(s string) (int64, bool) {
	s = strings.TrimLeft(s, "-+")
	start, _, ok := strings.Cut(s, ",")
	if !ok {
		start = s
	}
	n, err := strconv.ParseInt(start, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
