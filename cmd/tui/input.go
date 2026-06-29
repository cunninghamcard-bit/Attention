// Adapted from github.com/dimetron/pi-go internal/tui
package main

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// charOffsetToByteOffset converts a UTF-8 character offset within a string
// to a byte offset. Returns 0 if pos is out of bounds.
func charOffsetToByteOffset(s string, charPos int) int {
	if charPos <= 0 {
		return 0
	}
	byteOffset := 0
	for i := 0; i < charPos && byteOffset < len(s); {
		_, size := utf8.DecodeRuneInString(s[byteOffset:])
		if size == 0 {
			break
		}
		byteOffset += size
		i++
	}
	return byteOffset
}

// terminalResponseRe matches common terminal response fragments that leak
// through as text: CSI params (digits, semicolons, question marks) ending
// with a letter, DECRPM ($y), OSC color payloads (rgb:/hex colons+slashes),
// and cursor position reports.
var terminalResponseRe = regexp.MustCompile(
	`\[\d+;\d+[A-Z]` + // CSI CPR like [38;4R
		`|\d+\$[A-Za-z]` + // DECRPM tails like ;2$y
		`|[0-9a-f]{4}/[0-9a-f]{4}/[0-9a-f]{4}` + // hex triplet XXXX/XXXX/XXXX
		`|rgb:` + // OSC color payload
		`|\]\d+;`, // OSC intro like ]11;
)

// InputSubmitMsg is emitted when the user presses Enter with non-empty input.
type InputSubmitMsg struct {
	Text     string
	Mentions []string // file paths referenced via @path
}

// InputModel wraps Bubble Tea's standard textinput component with history
// and slash-command support. All completion/mention state has been removed;
// the textinput library handles cursor movement and editing directly.
type InputModel struct {
	Text       string
	CursorPos  int // character position (not byte offset)
	History    []HistoryEntry
	HistoryIdx int

	// Dependencies (set by root model).
	// Commands is the kernel's command list (get_commands), the SINGLE source of
	// truth for slash-command completion. Skills feeds the sidebar's bare-name
	// Skills section only.
	Commands  []CommandInfo
	Skills    []Skill
	SkillDirs []string
	WorkDir   string

	input textinput.Model
}

// NewInputModel creates an InputModel with initial state. commands is the
// kernel command list used for slash-command completion (the single source).
func NewInputModel(history []HistoryEntry, commands []CommandInfo, skills []Skill, skillDirs []string, workDir string) InputModel {
	im := InputModel{
		History:    history,
		HistoryIdx: -1,
		Commands:   commands,
		Skills:     skills,
		SkillDirs:  skillDirs,
		WorkDir:    workDir,
	}
	im.ensureInput()
	return im
}

// HandleKey processes a key press for the input area.
// Returns a tea.Cmd (InputSubmitMsg on submit, nil otherwise).
func (im *InputModel) HandleKey(msg tea.KeyPressMsg) tea.Cmd {
	im.ensureInput()
	key := msg.Key()

	switch {
	case isLineStartKey(key):
		im.input.CursorStart()
		im.syncFromInput()
		return nil
	case isLineEndKey(key):
		im.input.CursorEnd()
		im.syncFromInput()
		return nil
	}

	switch key.Code {
	case tea.KeyEnter:
		text := strings.TrimSpace(im.input.Value())
		if text == "" {
			return nil
		}
		mentions := extractMentions(text)
		entry := HistoryEntry{Text: text, Mentions: mentions}
		if len(im.History) == 0 || im.History[len(im.History)-1].Text != text {
			im.History = append(im.History, entry)
			appendHistory(entry)
		}
		im.HistoryIdx = -1
		im.setValue("")
		return func() tea.Msg { return InputSubmitMsg{Text: text, Mentions: mentions} }
	case tea.KeyUp:
		im.historyUp()
		return nil
	case tea.KeyDown:
		im.historyDown()
		return nil
	}

	if key.Text != "" && !isUserInput(key.Text) {
		if isUserPaste(key.Text) {
			im.InsertText(key.Text)
		}
		return nil
	}

	var cmd tea.Cmd
	im.input, cmd = im.input.Update(msg)
	im.syncFromInput()
	return cmd
}

// SetWidth sets the visible width of the editable input text, excluding the prompt.
func (im *InputModel) SetWidth(width int) {
	im.ensureInput()
	if width < 0 {
		width = 0
	}
	pos := im.CursorPos
	im.input.SetWidth(width)
	// The textinput viewport only recalculates when the cursor moves outside
	// the current bounds. After a width change (especially from the initial
	// width=0 to a real value), the old viewport covers the full text and the
	// cursor stays within it, so the viewport never narrows. CursorEnd forces
	// a right-edge recalculation with the new width; SetCursor restores the
	// actual position.
	im.input.CursorEnd()
	im.input.SetCursor(pos)
	im.syncFromInput()
}

// View renders the input area.
func (im *InputModel) View(running bool) string {
	im.ensureInput()
	if running {
		prefix := lipgloss.NewStyle().
			Foreground(lipgloss.Color("39")).
			Bold(true).
			Render("> ")
		dim := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
		return prefix + dim.Render("(waiting for response...)")
	}
	return im.input.View()
}

// InsertText inserts pasted or programmatic text at cursor position.
func (im *InputModel) InsertText(text string) {
	im.ensureInput()
	pos := im.CursorPos
	beforeByte := charOffsetToByteOffset(im.Text, im.CursorPos)
	im.setValue(im.Text[:beforeByte] + text + im.Text[beforeByte:])
	im.input.SetCursor(pos + utf8.RuneCountInString(text))
	im.syncFromInput()
}

// Clear resets the input text and cursor.
func (im *InputModel) Clear() {
	im.ensureInput()
	im.setValue("")
}

// SetText replaces the input text and moves the cursor to the end.
func (im *InputModel) SetText(text string) {
	im.ensureInput()
	im.setValue(text)
	im.input.CursorEnd()
	im.syncFromInput()
}

// AllCommandNames returns a sorted list of all slash command names from the
// kernel command list (the single source of truth), in their verbatim slash
// form (e.g. "/compact", "/skill:review").
func (im *InputModel) AllCommandNames() []string {
	seen := make(map[string]bool)
	var cmds []string
	for _, c := range im.Commands {
		name := "/" + c.Name
		if !seen[name] {
			seen[name] = true
			cmds = append(cmds, name)
		}
	}
	sort.Strings(cmds)
	return cmds
}

// Cursor returns the real Bubble Tea cursor for the input's current position.
func (im *InputModel) Cursor() *tea.Cursor {
	im.ensureInput()
	cursor := im.input.Cursor()
	if cursor == nil {
		return nil
	}
	promptWidth := lipgloss.Width(im.input.Prompt)
	beforeCursor := im.Text[:charOffsetToByteOffset(im.Text, im.CursorPos)]
	cursor.X = promptWidth + lipgloss.Width(beforeCursor)
	if width := im.input.Width(); width > 0 && cursor.X > promptWidth+width {
		cursor.X = promptWidth + width
	}
	return cursor
}

func (im *InputModel) ensureInput() {
	// Check if the textinput has been initialized by examining the prompt.
	// Using KeyMap.CharacterForward.Keys() == nil as a heuristic is fragile
	// because it depends on the zero value of key.Binding.Keys().
	if im.input.Prompt == "" {
		im.input = textinput.New()
		promptStyle := lipgloss.NewStyle().
			Foreground(lipgloss.Color("39")).
			Bold(true)
		styles := im.input.Styles()
		styles.Focused.Prompt = promptStyle
		styles.Blurred.Prompt = promptStyle
		styles.Cursor.Color = lipgloss.Color("39")
		styles.Cursor.Shape = tea.CursorBar
		im.input.SetStyles(styles)
		im.input.Prompt = "> "
		im.input.SetVirtualCursor(false)
		im.input.SetWidth(0)
		_ = im.input.Focus()
	}
	if im.input.Value() != im.Text {
		im.input.SetValue(im.Text)
	}
	im.input.SetCursor(im.CursorPos)
	im.syncFromInput()
}

func (im *InputModel) setValue(text string) {
	im.input.SetValue(text)
	im.syncFromInput()
}

func (im *InputModel) syncFromInput() {
	im.Text = im.input.Value()
	im.CursorPos = im.input.Position()
}

func (im *InputModel) historyUp() {
	if len(im.History) == 0 {
		return
	}
	if im.HistoryIdx < 0 {
		im.HistoryIdx = len(im.History) - 1
	} else if im.HistoryIdx > 0 {
		im.HistoryIdx--
	}
	im.restoreHistoryEntry(im.HistoryIdx)
}

func (im *InputModel) historyDown() {
	if im.HistoryIdx < 0 {
		return
	}
	im.HistoryIdx++
	if im.HistoryIdx >= len(im.History) {
		im.HistoryIdx = -1
		im.setValue("")
		return
	}
	im.restoreHistoryEntry(im.HistoryIdx)
}

func (im *InputModel) restoreHistoryEntry(idx int) {
	if idx < 0 || idx >= len(im.History) {
		return
	}
	im.setValue(im.History[idx].Text)
	im.input.CursorEnd()
	im.syncFromInput()
}

func isLineStartKey(key tea.Key) bool {
	return key.Code == tea.KeyHome ||
		(key.Code == 'a' && key.Mod == tea.ModCtrl) ||
		key.Code == 0x01
}

func isLineEndKey(key tea.Key) bool {
	return key.Code == tea.KeyEnd ||
		(key.Code == 'e' && key.Mod == tea.ModCtrl) ||
		key.Code == 0x05
}

// isUserInput returns true if the string represents genuine user keyboard input.
// Real keyboard input via KeyPressMsg is always a single rune. Multi-character
// text values are terminal response fragments (CSI, OSC, DECRPM) that leaked
// through Bubble Tea's parser when escape sequences get split at arbitrary
// byte boundaries during resize or color queries.
func isUserInput(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	// Real keystrokes produce exactly one rune. Multi-char text in a
	// KeyPressMsg is always terminal response garbage. Actual multi-char
	// input (paste) arrives via PasteMsg which is filtered separately.
	// NB: Some keyboard layouts or IMEs may produce multi-rune key events
	// (e.g. CJK IME composition, decomposed Unicode). These are still valid
	// user input even though they have >1 rune.
	if utf8.RuneCountInString(s) > 1 && !isCJKInput(s) {
		return false
	}
	return true
}

// isCJKInput returns true if the string appears to be CJK text from an IME.
// CJK IMEs may produce multi-rune key events during composition; these are
// valid user input and should not be treated as terminal response garbage.
func isCJKInput(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.Is(unicode.Han, r) && !unicode.Is(unicode.Hiragana, r) &&
			!unicode.Is(unicode.Katakana, r) && !unicode.Is(unicode.Hangul, r) {
			return false
		}
	}
	return true
}

// isUserPaste returns true if a PasteMsg contains real pasted text rather than
// terminal response sequences that were misidentified as bracketed paste.
func isUserPaste(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return !terminalResponseRe.MatchString(s)
}
