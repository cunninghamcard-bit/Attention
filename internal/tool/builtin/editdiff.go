// editdiff.go ports pi's content-based edit matching and display diff
// behavior from packages/coding-agent/src/core/tools/edit-diff.ts:11-454.
package builtin

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// FuzzyMatch is the result of matching old text against file content.
type FuzzyMatch struct {
	Found                 bool
	Index                 int
	MatchLength           int
	UsedFuzzy             bool
	ContentForReplacement string
}

// Edit describes one content replacement.
type Edit struct {
	OldText string
	NewText string
}

// AppliedEditResult contains the content diff should compare after edits apply.
type AppliedEditResult struct {
	BaseContent string
	NewContent  string
}

type matchedEdit struct {
	editIndex   int
	matchIndex  int
	matchLength int
	newText     string
}

// DetectLineEnding returns the first observed newline style, matching pi:11-18.
func DetectLineEnding(s string) string {
	crlf := strings.Index(s, "\r\n")
	lf := strings.Index(s, "\n")
	if lf == -1 {
		return "\n"
	}
	if crlf == -1 {
		return "\n"
	}
	if crlf < lf {
		return "\r\n"
	}
	return "\n"
}

// NormalizeToLF converts CRLF and bare CR line endings to LF, matching pi:19-21.
func NormalizeToLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// RestoreLineEndings restores LF content to the original newline style.
func RestoreLineEndings(s string, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}

// NormalizeForFuzzyMatch applies Unicode NFKC normalization then pi's fuzzy
// normalization: trailing whitespace per line, smart quotes, dashes, and spaces.
func NormalizeForFuzzyMatch(s string) string {
	s = norm.NFKC.String(s)

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	s = strings.Join(lines, "\n")

	var builder strings.Builder
	builder.Grow(len(s))
	for _, r := range s {
		switch {
		case isSmartSingleQuote(r):
			builder.WriteByte('\'')
		case isSmartDoubleQuote(r):
			builder.WriteByte('"')
		case isUnicodeDash(r):
			builder.WriteByte('-')
		case isSpecialSpace(r):
			builder.WriteByte(' ')
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// FuzzyFindText tries an exact match first, then a fuzzy-normalized match.
func FuzzyFindText(content string, oldText string) FuzzyMatch {
	exactIndex := strings.Index(content, oldText)
	if exactIndex != -1 {
		return FuzzyMatch{
			Found:                 true,
			Index:                 exactIndex,
			MatchLength:           len(oldText),
			UsedFuzzy:             false,
			ContentForReplacement: content,
		}
	}

	fuzzyContent := NormalizeForFuzzyMatch(content)
	fuzzyOldText := NormalizeForFuzzyMatch(oldText)
	fuzzyIndex := strings.Index(fuzzyContent, fuzzyOldText)
	if fuzzyIndex == -1 {
		return FuzzyMatch{
			Found:                 false,
			Index:                 -1,
			MatchLength:           0,
			UsedFuzzy:             false,
			ContentForReplacement: content,
		}
	}

	return FuzzyMatch{
		Found:                 true,
		Index:                 fuzzyIndex,
		MatchLength:           len(fuzzyOldText),
		UsedFuzzy:             true,
		ContentForReplacement: fuzzyContent,
	}
}

// StripBOM removes an initial UTF-8 BOM and returns it separately.
func StripBOM(s string) (bom, text string) {
	if after, ok := strings.CutPrefix(s, "\uFEFF"); ok {
		return "\uFEFF", after
	}
	return "", s
}

// ApplyEdits applies one or more content edits to LF-normalized content.
func ApplyEdits(normalizedContent string, edits []Edit, path string) (AppliedEditResult, error) {
	normalizedEdits := make([]Edit, 0, len(edits))
	for i, edit := range edits {
		oldText := NormalizeToLF(edit.OldText)
		if oldText == "" {
			return AppliedEditResult{}, emptyOldTextError(path, i, len(edits))
		}
		normalizedEdits = append(normalizedEdits, Edit{
			OldText: oldText,
			NewText: NormalizeToLF(edit.NewText),
		})
	}

	initialMatches := make([]FuzzyMatch, 0, len(normalizedEdits))
	for _, edit := range normalizedEdits {
		initialMatches = append(initialMatches, FuzzyFindText(normalizedContent, edit.OldText))
	}

	baseContent := normalizedContent
	for _, match := range initialMatches {
		if match.UsedFuzzy {
			baseContent = NormalizeForFuzzyMatch(normalizedContent)
			break
		}
	}

	matches := make([]matchedEdit, 0, len(normalizedEdits))
	for i, edit := range normalizedEdits {
		match := FuzzyFindText(baseContent, edit.OldText)
		if !match.Found {
			return AppliedEditResult{}, notFoundError(path, i, len(normalizedEdits))
		}

		occurrences := countOccurrences(baseContent, edit.OldText)
		if occurrences > 1 {
			return AppliedEditResult{}, duplicateError(path, i, len(normalizedEdits), occurrences)
		}

		matches = append(matches, matchedEdit{
			editIndex:   i,
			matchIndex:  match.Index,
			matchLength: match.MatchLength,
			newText:     edit.NewText,
		})
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].matchIndex < matches[j].matchIndex
	})
	for i := 1; i < len(matches); i++ {
		previous := matches[i-1]
		current := matches[i]
		if previous.matchIndex+previous.matchLength > current.matchIndex {
			return AppliedEditResult{}, fmt.Errorf(
				"edits[%d] and edits[%d] overlap in %s. Merge them into one edit or target disjoint regions.",
				previous.editIndex,
				current.editIndex,
				path,
			)
		}
	}

	newContent := baseContent
	for i := len(matches) - 1; i >= 0; i-- {
		edit := matches[i]
		newContent = newContent[:edit.matchIndex] +
			edit.newText +
			newContent[edit.matchIndex+edit.matchLength:]
	}
	if newContent == baseContent {
		return AppliedEditResult{}, noChangeError(path, len(normalizedEdits))
	}

	return AppliedEditResult{
		BaseContent: baseContent,
		NewContent:  newContent,
	}, nil
}

// DiffString returns a line-numbered display diff and the first changed line.
func DiffString(oldContent string, newContent string, contextLines int) (string, *int) {
	if contextLines < 0 {
		contextLines = 0
	}

	oldLines := comparableLines(oldContent)
	newLines := comparableLines(newContent)
	items := lineDiffItems(oldLines, newLines)
	groups := diffGroups(items)
	lineWidth := len(fmt.Sprint(max(len(strings.Split(oldContent, "\n")), len(strings.Split(newContent, "\n")))))

	output := []string{}
	var firstChangedLine *int
	for i, group := range groups {
		if group.change {
			for _, item := range group.items {
				if firstChangedLine == nil {
					line := item.newLine
					firstChangedLine = &line
				}
				output = append(output, formatDiffItem(item, lineWidth))
			}
			continue
		}

		hasLeadingChange := i > 0 && groups[i-1].change
		hasTrailingChange := i < len(groups)-1 && groups[i+1].change
		output = append(output, contextItems(group.items, hasLeadingChange, hasTrailingChange, contextLines, lineWidth)...)
	}

	return strings.Join(output, "\n"), firstChangedLine
}

// UnifiedPatch returns a standard unified patch, matching pi's
// generateUnifiedPatch wrapper around Diff.createTwoFilesPatch
// (.agents/references/pi/packages/coding-agent/src/core/tools/edit-diff.ts:262-268).
func UnifiedPatch(path string, oldContent string, newContent string, contextLines int) string {
	if contextLines < 0 {
		contextLines = 0
	}

	oldLines := comparableLines(oldContent)
	newLines := comparableLines(newContent)
	items := lineDiffItems(oldLines, newLines)

	var builder strings.Builder
	builder.WriteString("--- ")
	builder.WriteString(path)
	builder.WriteString("\n+++ ")
	builder.WriteString(path)
	builder.WriteString("\n")

	for _, hunk := range unifiedPatchHunks(items, contextLines) {
		oldStart, oldCount, newStart, newCount := unifiedPatchLineRange(hunk)
		fmt.Fprintf(&builder, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, item := range hunk {
			builder.WriteByte(unifiedPatchLinePrefix(item.kind))
			builder.WriteString(item.text)
			builder.WriteByte('\n')
		}
	}

	// jsdiff emits "\ No newline at end of file" markers for EOF newline
	// mismatches; this port intentionally skips them for now.
	return builder.String()
}

func isSmartSingleQuote(r rune) bool {
	switch r {
	case '\u2018', '\u2019', '\u201A', '\u201B':
		return true
	default:
		return false
	}
}

func isSmartDoubleQuote(r rune) bool {
	switch r {
	case '\u201C', '\u201D', '\u201E', '\u201F':
		return true
	default:
		return false
	}
}

func isUnicodeDash(r rune) bool {
	switch r {
	case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
		return true
	default:
		return false
	}
}

func isSpecialSpace(r rune) bool {
	return r == '\u00A0' ||
		(r >= '\u2002' && r <= '\u200A') ||
		r == '\u202F' ||
		r == '\u205F' ||
		r == '\u3000'
}

func countOccurrences(content string, oldText string) int {
	fuzzyContent := NormalizeForFuzzyMatch(content)
	fuzzyOldText := NormalizeForFuzzyMatch(oldText)
	return strings.Count(fuzzyContent, fuzzyOldText)
}

func notFoundError(path string, editIndex int, totalEdits int) error {
	if totalEdits == 1 {
		return fmt.Errorf(
			"Could not find the exact text in %s. The old text must match exactly including all whitespace and newlines.",
			path,
		)
	}
	return fmt.Errorf(
		"Could not find edits[%d] in %s. The oldText must match exactly including all whitespace and newlines.",
		editIndex,
		path,
	)
}

func duplicateError(path string, editIndex int, totalEdits int, occurrences int) error {
	if totalEdits == 1 {
		return fmt.Errorf(
			"Found %d occurrences of the text in %s. The text must be unique. Please provide more context to make it unique.",
			occurrences,
			path,
		)
	}
	return fmt.Errorf(
		"Found %d occurrences of edits[%d] in %s. Each oldText must be unique. Please provide more context to make it unique.",
		occurrences,
		editIndex,
		path,
	)
}

func emptyOldTextError(path string, editIndex int, totalEdits int) error {
	if totalEdits == 1 {
		return fmt.Errorf("oldText must not be empty in %s.", path)
	}
	return fmt.Errorf("edits[%d].oldText must not be empty in %s.", editIndex, path)
}

func noChangeError(path string, totalEdits int) error {
	if totalEdits == 1 {
		return fmt.Errorf(
			"No changes made to %s. The replacement produced identical content. "+
				"This might indicate an issue with special characters or the text not existing as expected.",
			path,
		)
	}
	return fmt.Errorf("No changes made to %s. The replacements produced identical content.", path)
}

type diffKind int

const (
	diffEqual diffKind = iota
	diffRemoved
	diffAdded
)

type diffItem struct {
	kind    diffKind
	text    string
	oldLine int
	newLine int
}

type diffGroup struct {
	change bool
	items  []diffItem
}

type diffRange struct {
	start int
	end   int
}

func comparableLines(s string) []string {
	if s == "" {
		return []string{}
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func lineDiffItems(oldLines []string, newLines []string) []diffItem {
	lcs := lcsLengths(oldLines, newLines)
	items := []diffItem{}
	oldLine := 1
	newLine := 1
	i := 0
	j := 0
	for i < len(oldLines) || j < len(newLines) {
		switch {
		case i < len(oldLines) && j < len(newLines) && oldLines[i] == newLines[j]:
			items = append(items, diffItem{
				kind:    diffEqual,
				text:    oldLines[i],
				oldLine: oldLine,
				newLine: newLine,
			})
			i++
			j++
			oldLine++
			newLine++
		case i < len(oldLines) && (j >= len(newLines) || lcs[i+1][j] >= lcs[i][j+1]):
			items = append(items, diffItem{
				kind:    diffRemoved,
				text:    oldLines[i],
				oldLine: oldLine,
				newLine: newLine,
			})
			i++
			oldLine++
		default:
			items = append(items, diffItem{
				kind:    diffAdded,
				text:    newLines[j],
				oldLine: oldLine,
				newLine: newLine,
			})
			j++
			newLine++
		}
	}
	return items
}

func lcsLengths(oldLines []string, newLines []string) [][]int {
	lcs := make([][]int, len(oldLines)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(newLines)+1)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
				continue
			}
			lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
		}
	}
	return lcs
}

func diffGroups(items []diffItem) []diffGroup {
	groups := []diffGroup{}
	for _, item := range items {
		change := item.kind != diffEqual
		last := len(groups) - 1
		if last >= 0 && groups[last].change == change {
			groups[last].items = append(groups[last].items, item)
			continue
		}
		groups = append(groups, diffGroup{
			change: change,
			items:  []diffItem{item},
		})
	}
	return groups
}

func unifiedPatchHunks(items []diffItem, contextLines int) [][]diffItem {
	groups := diffGroups(items)
	ranges := []diffRange{}
	offset := 0
	for _, group := range groups {
		groupStart := offset
		groupEnd := offset + len(group.items)
		offset = groupEnd

		if !group.change {
			continue
		}

		next := diffRange{
			start: max(0, groupStart-contextLines),
			end:   min(len(items), groupEnd+contextLines),
		}
		last := len(ranges) - 1
		if last >= 0 && next.start <= ranges[last].end {
			ranges[last].end = max(ranges[last].end, next.end)
			continue
		}
		ranges = append(ranges, next)
	}

	hunks := make([][]diffItem, 0, len(ranges))
	for _, r := range ranges {
		hunks = append(hunks, items[r.start:r.end])
	}
	return hunks
}

func unifiedPatchLineRange(items []diffItem) (int, int, int, int) {
	oldStart := 0
	newStart := 0
	oldCount := 0
	newCount := 0
	for _, item := range items {
		if item.kind != diffAdded {
			if oldStart == 0 {
				oldStart = item.oldLine
			}
			oldCount++
		}
		if item.kind != diffRemoved {
			if newStart == 0 {
				newStart = item.newLine
			}
			newCount++
		}
	}

	if oldStart == 0 && len(items) > 0 {
		oldStart = items[0].oldLine - 1
	}
	if newStart == 0 && len(items) > 0 {
		newStart = items[0].newLine - 1
	}

	return oldStart, oldCount, newStart, newCount
}

func unifiedPatchLinePrefix(kind diffKind) byte {
	switch kind {
	case diffAdded:
		return '+'
	case diffRemoved:
		return '-'
	default:
		return ' '
	}
}

func contextItems(
	items []diffItem,
	hasLeadingChange bool,
	hasTrailingChange bool,
	contextLines int,
	lineWidth int,
) []string {
	switch {
	case hasLeadingChange && hasTrailingChange:
		if len(items) <= contextLines*2 {
			return formatDiffItems(items, lineWidth)
		}
		output := formatDiffItems(items[:contextLines], lineWidth)
		output = append(output, separatorLine(lineWidth))
		output = append(output, formatDiffItems(items[len(items)-contextLines:], lineWidth)...)
		return output
	case hasLeadingChange:
		shown := min(len(items), contextLines)
		output := formatDiffItems(items[:shown], lineWidth)
		if len(items)-shown > 0 {
			output = append(output, separatorLine(lineWidth))
		}
		return output
	case hasTrailingChange:
		skipped := max(0, len(items)-contextLines)
		output := []string{}
		if skipped > 0 {
			output = append(output, separatorLine(lineWidth))
		}
		output = append(output, formatDiffItems(items[skipped:], lineWidth)...)
		return output
	default:
		return []string{}
	}
}

func formatDiffItems(items []diffItem, lineWidth int) []string {
	output := make([]string, 0, len(items))
	for _, item := range items {
		output = append(output, formatDiffItem(item, lineWidth))
	}
	return output
}

func formatDiffItem(item diffItem, lineWidth int) string {
	switch item.kind {
	case diffAdded:
		return fmt.Sprintf("+%*d %s", lineWidth, item.newLine, item.text)
	case diffRemoved:
		return fmt.Sprintf("-%*d %s", lineWidth, item.oldLine, item.text)
	default:
		return fmt.Sprintf(" %*d %s", lineWidth, item.oldLine, item.text)
	}
}

func separatorLine(lineWidth int) string {
	return fmt.Sprintf(" %*s ...", lineWidth, "")
}
