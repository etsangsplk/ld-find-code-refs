package search

import (
	"strings"
	"sync"

	"github.com/launchdarkly/ld-find-code-refs/internal/helpers"
	"github.com/launchdarkly/ld-find-code-refs/internal/ld"
)

const (
	// These are defensive limits intended to prevent corner cases stemming from
	// large repos, false positives, etc. The goal is a) to prevent the program
	// from taking a very long time to run and b) to prevent the program from
	// PUTing a massive json payload. These limits will likely be tweaked over
	// time. The LaunchDarkly backend will also apply limits.
	maxFileCount     = 10000 // Maximum number of files containing code references
	maxHunkCount     = 25000 // Maximum number of total code references
	maxLineCharCount = 500   // Maximum number of characters per line
)

// Truncate lines to prevent sending over massive hunks, e.g. a minified file.
// NOTE: We may end up truncating a valid flag key reference. We accept this risk
//       and will handle hunks missing flag key references on the frontend.
func truncateLine(line string) string {
	// len(line) returns number of bytes, not num. characters, but it's a close enough
	// approximation for our purposes
	if len(line) <= maxLineCharCount {
		return line
	}
	// convert to rune slice so that we don't truncate multibyte unicode characters
	runes := []rune(line)
	return string(runes[0:maxLineCharCount]) + "…"
}

func matchDelimiters(match string, flagKey string, delimiters string) bool {
	for _, left := range delimiters {
		for _, right := range delimiters {
			if strings.Contains(match, string(left)+flagKey+string(right)) {
				return true
			}
		}
	}
	return false
}

type file struct {
	path  string
	lines []string
}

func (f file) linesIfMatch(projKey, flagKey, line string, aliases []string, matchLineNum, ctxLines int, delimiters string) *ld.HunkRep {
	matchedFlag := false
	aliasMatches := []string{}

	// Match flag keys with delimiters
	if matchDelimiters(line, flagKey, delimiters) {
		matchedFlag = true
	}

	// Match all aliases for the flag key
	for _, alias := range aliases {
		if strings.Contains(line, alias) {
			aliasMatches = append(aliasMatches, alias)
		}
	}

	if !matchedFlag && len(aliasMatches) == 0 {
		return nil
	}

	startingLineNum := matchLineNum
	var context []string
	if ctxLines >= 0 {
		startingLineNum -= ctxLines
		if startingLineNum < 0 {
			startingLineNum = 0
		}
		endingLineNum := matchLineNum + ctxLines + 1
		if endingLineNum >= len(f.lines) {
			context = f.lines[startingLineNum:]
		} else {
			context = f.lines[startingLineNum:endingLineNum]
		}
	}
	for i, line := range context {
		context[i] = truncateLine(line)
	}

	ret := ld.HunkRep{
		ProjKey:            projKey,
		FlagKey:            flagKey,
		StartingLineNumber: startingLineNum + 1,
		Lines:              strings.Join(context, "\n"),
		Aliases:            []string{}}
	for _, alias := range aliasMatches {
		ret.Aliases = []string{alias}
	}

	return &ret
}

func (f file) toHunks(projKey string, aliases map[string][]string, ctxLines int, delimiters string) *ld.ReferenceHunksRep {
	hunks := []ld.HunkRep{}
	for flagKey, flagAliases := range aliases {
		hunks = append(hunks, f.aggregateHunksForFlag(projKey, flagKey, flagAliases, ctxLines, delimiters)...)
	}
	if len(hunks) == 0 {
		return nil
	}
	return &ld.ReferenceHunksRep{Path: f.path, Hunks: hunks}
}

// aggregateHunksForFlag finds all references in a file, and combines matches into hunks if their context lines overlap
func (f file) aggregateHunksForFlag(projKey, flagKey string, flagAliases []string, ctxLines int, delimiters string) []ld.HunkRep {
	hunksForFlag := []ld.HunkRep{}
	for i, line := range f.lines {
		match := f.linesIfMatch(projKey, flagKey, line, flagAliases, i, ctxLines, delimiters)
		if match != nil {
			lastHunkIdx := len(hunksForFlag) - 1
			// If the previous hunk overlaps or is adjacent to the current hunk, merge them together
			if lastHunkIdx >= 0 && hunksForFlag[lastHunkIdx].Overlap(*match) >= 0 {
				hunksForFlag = append(hunksForFlag[:lastHunkIdx], mergeHunks(hunksForFlag[lastHunkIdx], *match, ctxLines)...)
			} else {
				hunksForFlag = append(hunksForFlag, *match)
			}
		}
	}
	return hunksForFlag
}

// mergeHunks combines the lines and aliases of two hunks together for a given file
// if the hunks do not overlap, returns each hunk separately
// assumes the startingLineNumber of a is less than b and there is some overlap between the two
func mergeHunks(a, b ld.HunkRep, ctxLines int) []ld.HunkRep {
	if a.StartingLineNumber > b.StartingLineNumber {
		a, b = b, a
	}

	aLines := strings.Split(a.Lines, "\n")
	bLines := strings.Split(b.Lines, "\n")
	overlap := a.Overlap(b)
	// no overlap
	if ctxLines < 0 || overlap < 0 {
		return []ld.HunkRep{a, b}
	} else if overlap >= len(bLines) {
		// subset hunk
		return []ld.HunkRep{a}
	}

	combinedLines := append(aLines, bLines[overlap:]...)
	return []ld.HunkRep{
		{
			StartingLineNumber: a.StartingLineNumber,
			Lines:              strings.Join(combinedLines, "\n"),
			ProjKey:            a.ProjKey,
			FlagKey:            a.FlagKey,
			Aliases:            helpers.Dedupe(append(a.Aliases, b.Aliases...)),
		},
	}
}

// processFiles starts goroutines to process files individually. When all files have completed processing, the references channel is closed to signal completion.
func processFiles(files chan file, references chan ld.ReferenceHunksRep, projKey string, aliases map[string][]string, ctxLines int, delimiters string) {
	w := new(sync.WaitGroup)
	for file := range files {
		file := file
		w.Add(1)
		go func() {
			reference := file.toHunks(projKey, aliases, ctxLines, delimiters)
			if reference != nil {
				references <- *reference
			}
			w.Done()
		}()
	}
	w.Wait()
	close(references)
}

func SearchForRefs(projKey, workspace string, searchTerms []string, aliases map[string][]string, ctxLines int, delimiters string) ([]ld.ReferenceHunksRep, error) {
	files := make(chan file)
	references := make(chan ld.ReferenceHunksRep)

	// Start workers to process files asynchronously as they are written to the files channel
	go processFiles(files, references, projKey, aliases, ctxLines, delimiters)

	// Blocks until all files have been read, but not necessarily processed
	readFiles(files, workspace)

	ret := []ld.ReferenceHunksRep{}
	totalHunks := 0
	for reference := range references {
		ret = append(ret, reference)

		// Reached maximum number of files with code references
		if len(ret) >= maxFileCount {
			return ret, nil
		}
		totalHunks += len(reference.Hunks)
		// Reached maximum number of hunks across all files
		if totalHunks > maxHunkCount {
			return ret, nil
		}
	}
	return ret, nil
}
