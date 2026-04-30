package github

import (
	"strconv"
	"strings"
)

// DiffLines parses a unified diff and returns a map of file path → set of commentable line numbers.
// These are the "new" side line numbers (right side) that GitHub will accept for review comments.
func DiffLines(diff string) map[string]map[int]bool {
	result := make(map[string]map[int]bool)
	var currentFile string
	var lineNum int

	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git") {
			// Extract file path from "diff --git a/path b/path"
			parts := strings.SplitN(line, " b/", 2)
			if len(parts) == 2 {
				currentFile = parts[1]
				result[currentFile] = make(map[int]bool)
			}
			continue
		}

		if strings.HasPrefix(line, "@@") && currentFile != "" {
			// Parse hunk header: @@ -old,count +new,count @@
			lineNum = parseHunkNewStart(line)
			continue
		}

		if currentFile == "" || lineNum == 0 {
			continue
		}

		if strings.HasPrefix(line, "-") {
			// Deleted line — not commentable on the new side, don't increment
			continue
		}

		if strings.HasPrefix(line, "+") || !strings.HasPrefix(line, "\\") {
			// Added line or context line — commentable
			result[currentFile][lineNum] = true
			lineNum++
		}
	}

	return result
}

// DiffLinesFromPatches builds the same file → commentable-line-set map as
// DiffLines, but from the per-file patch strings returned by GetPRFiles.
// Used as a fallback when the full PR diff is too large (HTTP 406).
func DiffLinesFromPatches(files []PRFile) map[string]map[int]bool {
	result := make(map[string]map[int]bool)
	for _, f := range files {
		result[f.Filename] = parsePatchLines(f.Patch)
	}
	return result
}

// parsePatchLines extracts commentable new-side line numbers from a single
// file's patch string (as returned by GitHub's PR files API).
func parsePatchLines(patch string) map[int]bool {
	result := make(map[int]bool)
	var lineNum int
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "@@") {
			lineNum = parseHunkNewStart(line)
			continue
		}
		if lineNum == 0 {
			continue
		}
		if strings.HasPrefix(line, "-") {
			continue
		}
		if strings.HasPrefix(line, "+") || !strings.HasPrefix(line, "\\") {
			result[lineNum] = true
			lineNum++
		}
	}
	return result
}

// ValidateCommentLine checks if a file+line is commentable in the given diff.
// Returns an error message if not, empty string if valid.
func ValidateCommentLine(diffLines map[string]map[int]bool, path string, line int, startLine *int) string {
	fileLines, ok := diffLines[path]
	if !ok {
		return "file '" + path + "' is not in the diff"
	}

	if !fileLines[line] {
		return "line " + strconv.Itoa(line) + " in '" + path + "' is not part of the diff"
	}

	if startLine != nil && !fileLines[*startLine] {
		return "start_line " + strconv.Itoa(*startLine) + " in '" + path + "' is not part of the diff"
	}

	return ""
}

func parseHunkNewStart(hunkHeader string) int {
	// @@ -old,count +new,count @@ optional section
	plusIdx := strings.Index(hunkHeader, "+")
	if plusIdx < 0 {
		return 0
	}
	rest := hunkHeader[plusIdx+1:]
	commaIdx := strings.Index(rest, ",")
	spaceIdx := strings.Index(rest, " ")

	end := len(rest)
	if commaIdx > 0 && commaIdx < end {
		end = commaIdx
	}
	if spaceIdx > 0 && spaceIdx < end {
		end = spaceIdx
	}

	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}
