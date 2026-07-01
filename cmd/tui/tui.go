// Adapted from github.com/dimetron/pi-go internal/tui
// Package tui implements the interactive terminal UI using Bubble Tea v2.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	tea "charm.land/bubbletea/v2"
)

// model is the Bubble Tea model for the interactive TUI.
type model struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc

	// UI state.
	width  int
	height int

	// Input sub-model.
	inputModel InputModel

	// Chat sub-model (messages, scroll, rendering).
	chatModel ChatModel

	// Status bar sub-model.
	statusModel StatusModel

	// Theme manager.
	themeManager *ThemeManager

	// Agent state.
	running     bool
	agentCh     chan agentMsg      // channel for receiving agent events
	agentCancel context.CancelFunc // cancels the active agent response without quitting the TUI

	// Agent face renderer with mood expressions.
	face *FaceRenderer

	// Matrix rain animation state for sidebar.
	matrix matrixState

	// Loading state. With deferred init cut, the TUI always starts ready, so
	// these stay at their zero values; the render path still reads them.
	loading      bool
	loadingItems map[string]bool // item name -> done?
	loadingTotal int             // planned init item count when known
	loadingDots  int             // animation dots (0-3): ., .., ..., ....

	// Git diff stats (refreshed after tool completions).
	diffAdded   int
	diffRemoved int

	// Branch popup state (shown on status bar click).
	branchPopup *branchPopupState

	// Item picker popup state, reused by builtin commands that need a selector.
	// Modeled on branchPopupState; kind routes Enter to the matching kernel rpc.
	itemPicker *itemPickerState

	// Unified search popup for slash commands and history.
	searchPopup *searchPopupState

	// Legacy selection index for slash commands (used in tests).
	slashCommandSelected int

	// Quit.
	quitting bool

	// Ctrl+C handling: show warning on first press, quit on second.
	ctrlCCount int

	// resizeAt records when the last WindowSizeMsg arrived. Key/paste input
	// is suppressed briefly after resize to let terminal response sequences
	// (OSC color replies, DECRPM, CPR) drain without leaking into the input.
	resizeAt time.Time
}

// branchPopupState manages the git branch list popup.
type branchPopupState struct {
	branches  []string // list of git branches
	selected  int      // currently selected index
	active    string   // the currently active branch
	height    int      // popup height (number of visible items)
	scrollOff int      // scroll offset when more branches than height
}

// pickerKind selects which kernel action an item picker drives on Enter.
type pickerKind string

const (
	pickerFork   pickerKind = "fork"   // /fork — select a user message to fork from
	pickerResume pickerKind = "resume" // /resume — select another session to switch to
	pickerTree   pickerKind = "tree"   // /tree — select an entry to navigate to
	pickerModel  pickerKind = "model"  // /model — select a configured kernel model
)

// itemPickerState manages the generic {id,label} list popup behind /fork,
// /resume, and /tree. It mirrors branchPopupState's selected/height/scrollOff
// navigation contract exactly; kind routes Enter to the matching kernel rpc and
// title labels the popup header.
type itemPickerState struct {
	kind      pickerKind
	title     string       // popup header text
	items     []PickerItem // {id,label} rows
	selected  int          // currently selected index
	height    int          // popup height (number of visible items)
	scrollOff int          // scroll offset when more items than height
}

// newItemPicker opens an item picker over the given rows. The popup height is
// capped at 8 like the branch popup. An empty list is rejected by the caller
// (which shows an honest empty-state notice instead).
func (m *model) newItemPicker(kind pickerKind, title string, items []PickerItem) {
	popupHeight := len(items)
	if popupHeight > 8 {
		popupHeight = 8
	}
	m.itemPicker = &itemPickerState{
		kind:      kind,
		title:     title,
		items:     items,
		selected:  0,
		height:    popupHeight,
		scrollOff: 0,
	}
}

// newBranchPopup creates a new branch popup with the list of git branches.
func (m *model) newBranchPopup() {
	branches := listGitBranches("")
	if len(branches) == 0 {
		return
	}

	active := m.statusModel.GitBranch
	selected := 0
	for i, b := range branches {
		if b == active {
			selected = i
			break
		}
	}

	popupHeight := len(branches)
	if popupHeight > 8 {
		popupHeight = 8
	}

	m.branchPopup = &branchPopupState{
		branches:  branches,
		selected:  selected,
		active:    active,
		height:    popupHeight,
		scrollOff: 0,
	}
}

// listGitBranches returns a list of all local git branches, with the active one first.
func listGitBranches(workDir string) []string {
	cmd := exec.Command("git", "branch")
	if workDir != "" {
		cmd.Dir = workDir
	}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var branches []string
	active := ""
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// Active branch starts with '*'
		if strings.HasPrefix(line, "* ") {
			active = strings.TrimPrefix(line, "* ")
		} else {
			branches = append(branches, strings.TrimSpace(line))
		}
	}

	// Put active branch first
	if active != "" {
		result := []string{active}
		result = append(result, branches...)
		return result
	}
	return branches
}

// searchPopupState is a unified search window for slash commands and history.
// The mode determines what items are shown and how they are selected.
type searchPopupState struct {
	mode         searchMode   // "commands", "history", or "mention"
	entries      []SearchItem // all items (commands or history entries)
	filtered     []SearchItem // filtered by search query
	selected     int          // currently selected index in filtered list
	search       string       // current search query
	height       int          // popup height (number of visible items)
	scrollOff    int          // scroll offset when more entries than height
	mentionStart int          // char index of '@' for mention mode (-1 otherwise)
}

// searchMode determines what the popup displays.
type searchMode string

const (
	searchModeCommands searchMode = "commands"
	searchModeHistory  searchMode = "history"
	searchModeMention  searchMode = "mention"
)

// SearchItem represents an item in the search popup (command or history entry).
type SearchItem struct {
	Text        string // the command or history text
	Description string // for commands: the description
}

// newSearchPopup creates a unified search popup with the given mode.
func (m *model) newSearchPopup(mode searchMode) {
	var items []SearchItem
	var popupHeight int
	mentionStart := -1

	switch mode {
	case searchModeMention:
		// @file completion: find the @prefix at the cursor and list matching
		// files (CompleteMention) under the working dir.
		start, prefix := findMentionAtCursor(m.inputModel.Text, m.inputModel.CursorPos)
		if start < 0 {
			m.searchPopup = nil
			return
		}
		for _, c := range CompleteMention(prefix, m.cwd()).Candidates {
			items = append(items, SearchItem{Text: c.Text, Description: c.Description})
		}
		if len(items) == 0 {
			m.searchPopup = nil
			return
		}
		mentionStart = start
		popupHeight = searchPopupListHeight(len(items))

	case searchModeCommands:
		// Get all slash command candidates.
		allCandidates := m.allSearchCandidates()
		// Filter by current input text if it starts with /.
		inputText := m.inputModel.Text
		filter := ""
		showAll := inputText == "/" // Show all commands when just "/" is typed
		if strings.HasPrefix(inputText, "/") && !showAll {
			filter = strings.ToLower(inputText)
		}
		for _, c := range allCandidates {
			if filter == "" || strings.HasPrefix(strings.ToLower(c.Text), filter) {
				items = append(items, SearchItem{Text: c.Text, Description: c.Description})
			}
		}
		popupHeight = searchPopupListHeight(len(items))

	case searchModeHistory:
		entries := m.inputModel.History
		if len(entries) == 0 {
			return
		}
		// Show oldest first (last item in entries at top).
		items = make([]SearchItem, len(entries))
		for i, e := range entries {
			items[len(entries)-1-i] = SearchItem{Text: e.Text}
		}
		popupHeight = searchPopupListHeight(len(items))
	}

	m.searchPopup = &searchPopupState{
		mode:         mode,
		entries:      items,
		filtered:     items,
		selected:     0,
		search:       "",
		height:       popupHeight,
		scrollOff:    0,
		mentionStart: mentionStart,
	}
}

// insertMention replaces the @prefix at sp.mentionStart..cursor with the chosen
// file path, e.g. "@io" -> "@internal/io/foo.go ". Cursor lands at the end (the
// common case is mentioning at the end of the line).
func (m *model) insertMention(sp *searchPopupState, path string) {
	runes := []rune(m.inputModel.Text)
	start := sp.mentionStart
	cursor := m.inputModel.CursorPos
	if start < 0 || start > len(runes) || cursor > len(runes) || cursor < start {
		return
	}
	m.inputModel.SetText(string(runes[:start]) + "@" + path + " " + string(runes[cursor:]))
}

func searchPopupListHeight(itemCount int) int {
	if itemCount <= 0 {
		return 0
	}
	height := itemCount
	if height > 25 {
		height = 25
	}
	if height < 3 {
		height = 3
	}
	return height
}

// allSearchCandidates returns all slash command candidates for the search popup,
// built from the kernel command list (the single source of truth).
func (m *model) allSearchCandidates() []CompletionCandidate {
	return allSlashCommandCandidates(m.cfg.Commands)
}

// filterSearch filters items by search query (case-insensitive substring on Text).
func (sp *searchPopupState) filterSearch() {
	if sp.search == "" {
		sp.filtered = sp.entries
		sp.selected = 0
		sp.scrollOff = 0
		return
	}
	q := strings.ToLower(sp.search)
	var filtered []SearchItem
	for _, e := range sp.entries {
		if strings.Contains(strings.ToLower(e.Text), q) || strings.Contains(strings.ToLower(e.Description), q) {
			filtered = append(filtered, e)
		}
	}
	if filtered == nil {
		filtered = sp.entries // show all if no matches
	}
	sp.filtered = filtered
	sp.selected = 0
	sp.scrollOff = 0
}

// Run starts the interactive TUI.
func Run(ctx context.Context, cfg Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	renderer := newMarkdownRenderer(100)

	// Load persistent command history from ~/.pi-go/history.jsonl.
	history := loadHistory()
	if history == nil {
		history = make([]HistoryEntry, 0)
	}

	// Initialize theme manager.
	tm := NewThemeManager()
	if cfg.ThemeName != "" && cfg.ThemeName != "default" {
		_ = tm.SetTheme(cfg.ThemeName) // ignore error, falls back to tokyo-night
	}

	m := model{
		cfg:          cfg,
		ctx:          ctx,
		cancel:       cancel,
		inputModel:   NewInputModel(history, cfg.Commands, cfg.Skills, nil, ""),
		chatModel:    NewChatModel(renderer),
		statusModel:  StatusModel{},
		themeManager: tm,
		face:         NewFaceRenderer(),
	}

	m.statusModel.GitBranch = detectBranch("")

	p := tea.NewProgram(&m, tea.WithContext(ctx))
	_, err := p.Run()
	drainTerminalResponses()
	return err
}

func (m *model) Init() tea.Cmd {
	m.refreshDiffStats()
	return nil
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resizeAt = time.Now()
		m.width = msg.Width
		m.height = msg.Height
		m.applyResize()
		cmd := resizeDrainDoneCmd(m.resizeAt)
		if m.running {
			return m, tea.Batch(cmd, waitForAgent(m.agentCh))
		}
		return m, cmd

	case tea.PasteMsg:
		if !m.running && !m.resizeDraining() && isUserPaste(msg.Content) {
			m.inputModel.InsertText(msg.Content)
		}
		if m.resizeDraining() {
			return m, resizeDrainDoneCmd(m.resizeAt)
		}

	case tea.KeyPressMsg:
		if m.resizeDraining() && isResizeTextFragment(msg) {
			return m, resizeDrainDoneCmd(m.resizeAt)
		}
		return m.handleKey(msg)

	case tea.MouseMsg:
		switch msg := msg.(type) {
		case tea.MouseClickMsg:
			return m.handleMouseClick(msg)
		case tea.MouseWheelMsg:
			return m.handleMouseWheel(msg)
		}
		return m, nil

	case InputSubmitMsg:
		if strings.HasPrefix(msg.Text, "/") {
			return m.handleSlashCommand(msg.Text)
		}
		return m.submitPrompt(msg.Text, msg.Mentions)

	case restartMsg:
		execRestart()
		return m, tea.Quit

	case agentThinkingMsg:
		return m.handleAgentThinking(msg)

	case resetCtrlCCountMsg:
		return m.handleResetCtrlCCount()

	case resizeDrainDoneMsg:
		if msg.resizeAt.Equal(m.resizeAt) {
			m.resizeAt = time.Time{}
		}
		return m, nil

	case loadingTickMsg:
		m.loadingDots = (m.loadingDots + 1) % 4
		return m, tea.Tick(300*time.Millisecond, func(t time.Time) tea.Msg { return loadingTickMsg{} })

	case agentTextMsg:
		return m.handleAgentText(msg)

	case agentToolCallMsg:
		return m.handleAgentToolCall(msg)

	case agentToolResultMsg:
		return m.handleAgentToolResult(msg)

	case agentSubEventMsg:
		return m.handleAgentSubEvent(msg)

	case agentDoneMsg:
		return m.handleAgentDone(msg)

	case matrixTickMsg:
		if m.running {
			m.matrix.tick(m.mainWidth())
			return m, matrixTickCmd()
		}
		return m, nil
	}

	// Keep the agent listener alive for any unhandled message types.
	if m.running {
		return m, waitForAgent(m.agentCh)
	}
	return m, nil
}

// handleMouseClick processes mouse click events.
func (m *model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	return m, nil
}

// handleMouseWheel processes mouse wheel events for scrolling the chat viewport.
func (m *model) handleMouseWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	mouse := msg.Mouse()
	switch mouse.Button {
	case tea.MouseWheelUp:
		m.chatModel.ScrollUp(3, m.height)
	case tea.MouseWheelDown:
		m.chatModel.ScrollDown(3)
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()

	// Handle branch popup.
	if m.branchPopup != nil {
		switch key.Code {
		case tea.KeyEsc:
			m.branchPopup = nil
			return m, nil
		case tea.KeyEnter:
			return m.handleBranchSelect()
		case tea.KeyUp:
			if m.branchPopup.selected > 0 {
				m.branchPopup.selected--
				if m.branchPopup.selected < m.branchPopup.scrollOff {
					m.branchPopup.scrollOff--
				}
			}
			return m, nil
		case tea.KeyDown:
			if m.branchPopup.selected < len(m.branchPopup.branches)-1 {
				m.branchPopup.selected++
				if m.branchPopup.selected >= m.branchPopup.scrollOff+m.branchPopup.height {
					m.branchPopup.scrollOff++
				}
			}
			return m, nil
		default:
			// Any other key dismisses the popup
			m.branchPopup = nil
			return m, nil
		}
	}

	// Handle item picker popup. Mirrors branchPopup.
	if m.itemPicker != nil {
		switch key.Code {
		case tea.KeyEsc:
			m.itemPicker = nil
			return m, nil
		case tea.KeyEnter:
			return m.handleItemPickerSelect()
		case tea.KeyUp:
			if m.itemPicker.selected > 0 {
				m.itemPicker.selected--
				if m.itemPicker.selected < m.itemPicker.scrollOff {
					m.itemPicker.scrollOff--
				}
			}
			return m, nil
		case tea.KeyDown:
			if m.itemPicker.selected < len(m.itemPicker.items)-1 {
				m.itemPicker.selected++
				if m.itemPicker.selected >= m.itemPicker.scrollOff+m.itemPicker.height {
					m.itemPicker.scrollOff++
				}
			}
			return m, nil
		default:
			// Any other key dismisses the popup
			m.itemPicker = nil
			return m, nil
		}
	}

	// Esc / Ctrl+C: dismiss completion, cancel agent, or quit.
	switch {
	case key.Code == tea.KeyEsc:
		if m.searchPopup != nil {
			m.searchPopup = nil
			return m, nil
		}
		if m.running {
			m.cancelAgent()
			return m, nil
		}
		return m, nil

	case key.Code == 'c' && key.Mod == tea.ModCtrl:
		if m.running {
			m.cancelAgent()
			m.ctrlCCount++
			m.chatModel.AppendWarning("\nCtrl+C again to quit (or wait 2s)...")
			return m, resetCtrlCCount(m)
		}
		m.ctrlCCount++
		if m.ctrlCCount >= 2 {
			m.quitting = true
			return m, tea.Quit
		}
		// First press: show warning and reset count after 2 seconds
		m.chatModel.AppendWarning("\nCtrl+C again to quit (or wait 2s)...")
		return m, resetCtrlCCount(m)

	case key.Code == tea.KeyF12:
		return m, nil
	}

	if m.running || m.loading {
		return m, nil
	}

	// Ctrl+O: toggle compact/expanded tool output.
	if key.Code == 'o' && key.Mod == tea.ModCtrl {
		m.chatModel.ToolDisplay.CompactTools = !m.chatModel.ToolDisplay.CompactTools
		return m, nil
	}

	// Ctrl+B toggles the branch popup only when the prompt is empty. The
	// standard text input uses Ctrl+B as backward cursor movement, and some
	// terminals emit it for left/back navigation.
	if key.Code == 'b' && key.Mod == tea.ModCtrl && m.inputModel.Text == "" {
		if m.statusModel.GitBranch != "" {
			if m.branchPopup == nil {
				m.newBranchPopup()
			} else {
				m.branchPopup = nil
			}
		}
		return m, nil
	}

	// Handle unified search popup keys (slash commands or history).
	if m.handleSearchPopupKey(key) {
		return m, nil
	}

	// Arrow up on empty input: show history search popup.
	if key.Code == tea.KeyUp && m.inputModel.Text == "" && m.searchPopup == nil {
		if len(m.inputModel.History) > 0 {
			m.newSearchPopup(searchModeHistory)
			return m, nil
		}
		// If no history, fall through to input model for inline history nav.
	}

	// Scroll keys stay in root model.
	switch key.Code {
	case tea.KeyPgUp:
		m.chatModel.ScrollUp(5, m.height)
		return m, nil

	case tea.KeyPgDown:
		m.chatModel.ScrollDown(5)
		return m, nil
	}

	// Slash command: show commands popup when input starts with /.
	if m.shouldShowSlashCommandPopup() {
		if m.searchPopup == nil || m.searchPopup.mode != searchModeCommands {
			m.newSearchPopup(searchModeCommands)
		}
		// Immediately handle Tab/Up/Down to navigate the popup.
		if key.Code == tea.KeyTab || key.Code == tea.KeyUp || key.Code == tea.KeyDown {
			if m.handleSearchPopupKey(key) {
				return m, nil
			}
		}
	}

	// Delegate all other keys to InputModel.
	prevText := m.inputModel.Text
	cmd := m.inputModel.HandleKey(msg)
	// Keep the commands popup in sync with the input line (single source of
	// truth): rebuild it from the new input text on every change, and close it
	// when nothing matches so Enter submits the line instead of accepting a
	// stale item.
	if m.inputModel.Text != prevText {
		wasCommands := m.searchPopup != nil && m.searchPopup.mode == searchModeCommands
		wasMention := m.searchPopup != nil && m.searchPopup.mode == searchModeMention
		atStart, _ := findMentionAtCursor(m.inputModel.Text, m.inputModel.CursorPos)
		switch {
		case m.shouldShowSlashCommandPopup():
			if wasCommands || m.searchPopup == nil {
				m.newSearchPopup(searchModeCommands)
				if m.searchPopup != nil && len(m.searchPopup.filtered) == 0 {
					m.searchPopup = nil
				}
			}
		case atStart >= 0 && (wasMention || m.searchPopup == nil):
			// An @mention is at the cursor: open/rebuild the file picker
			// (newSearchPopup clears it if there are no matching files).
			m.newSearchPopup(searchModeMention)
		case wasCommands || wasMention:
			m.searchPopup = nil
		}
	}
	return m, cmd
}

func (m *model) handleSearchPopupKey(key tea.Key) bool {
	if m.searchPopup == nil {
		return false
	}

	sp := m.searchPopup

	switch key.Code {
	case tea.KeyUp:
		if sp.selected > 0 {
			sp.selected--
		} else if len(sp.filtered) > 1 {
			// Wrap to last item on Up from first.
			sp.selected = len(sp.filtered) - 1
		}
		sp.scrollOff = max(0, sp.selected-sp.height+1)
		return true
	case tea.KeyDown:
		if sp.selected < len(sp.filtered)-1 {
			sp.selected++
		} else if len(sp.filtered) > 1 {
			// Wrap to first item on Down from last.
			sp.selected = 0
		}
		if sp.selected >= sp.scrollOff+sp.height {
			sp.scrollOff = sp.selected - sp.height + 1
		}
		return true
	case tea.KeyTab:
		// Tab COMPLETES the selected item into the input box, then closes the
		// popup. The input line is the single source of truth.
		if len(sp.filtered) > 0 && sp.selected < len(sp.filtered) {
			item := sp.filtered[sp.selected]
			switch sp.mode {
			case searchModeHistory:
				m.inputModel.SetText(item.Text)
				m.inputModel.HistoryIdx = -1
			case searchModeMention:
				m.insertMention(sp, item.Text)
			default:
				m.inputModel.SetText(item.Text + " ")
			}
			m.searchPopup = nil
		}
		return true
	case tea.KeyEnter:
		if sp.mode == searchModeHistory {
			// History: Enter inserts the entry into the input (does not submit).
			if len(sp.filtered) > 0 && sp.selected < len(sp.filtered) {
				m.inputModel.SetText(sp.filtered[sp.selected].Text)
				m.inputModel.HistoryIdx = -1
			}
			m.searchPopup = nil
			return true
		}
		if sp.mode == searchModeMention {
			// Mention: Enter accepts the highlighted file into the @prefix.
			if len(sp.filtered) > 0 && sp.selected < len(sp.filtered) {
				m.insertMention(sp, sp.filtered[sp.selected].Text)
			}
			m.searchPopup = nil
			return true
		}
		// Commands: Enter SUBMITS the typed line. Let the key flow to the input
		// model (-> InputSubmitMsg -> handleSlashCommand). Do NOT accept a
		// highlighted item here — that was the "/zzzbogus picks /clone" bug.
		return false
	case tea.KeyEsc:
		m.searchPopup = nil
		return true
	case tea.KeyBackspace:
		if sp.mode == searchModeHistory {
			if len(sp.search) > 0 {
				sp.search = sp.search[:len(sp.search)-1]
				sp.filterSearch()
			} else {
				m.searchPopup = nil
			}
			return true
		}
		// Commands: backspace edits the INPUT; the input-change handler
		// re-filters the popup from the new input text.
		return false
	default:
		if sp.mode == searchModeHistory {
			// History keeps its own type-to-search field.
			if key.Text != "" && len(key.Text) == 1 && key.Mod == 0 {
				sp.search += key.Text
				sp.filterSearch()
				return true
			}
			return false
		}
		// Commands: printable chars flow to the input box (single source); the
		// input-change handler rebuilds + re-filters the popup from the text.
		return false
	}
}

func (m *model) View() tea.View {
	if m.quitting {
		return tea.NewView("Goodbye!\n")
	}

	if m.width == 0 {
		// Show matrix-style startup text before the first terminal size arrives.
		matrixLine := renderStartupMatrixLine(m.loadingDots, m.cfg.AppVersion, m.loadingItems, m.loadingTotal)
		if m.loadingItems != nil {
			var lines []string
			lines = append(lines, matrixLine)
			for _, item := range sortedKeys(m.loadingItems) {
				done := m.loadingItems[item]
				mark := " "
				if done {
					mark = "✓"
				}
				lines = append(lines, "  "+mark+" "+item)
			}
			return tea.NewView(strings.Join(lines, "\n") + "\n")
		}
		return tea.NewView(matrixLine + "\n")
	}

	// Layout: sidebar on the right, chat+status+input on the left.
	mainWidth := m.mainWidth()
	if m.statusModel.Width != mainWidth || m.chatModel.Width != mainWidth {
		m.applyResize()
		mainWidth = m.mainWidth()
	}
	sidebarWidth := m.width - mainWidth
	showSidebar := sidebarWidth > 0

	// Render components.
	m.inputModel.SetWidth(max(0, mainWidth-2))
	messagesView := m.chatModel.RenderMessages(m.running)
	statusBar := m.statusModel.Render(m.statusRenderInput())
	inputArea := m.inputModel.View(m.running || m.loading)
	var inputCursor *tea.Cursor
	if !m.running && !m.loading {
		inputCursor = m.inputModel.Cursor()
	}

	// Calculate available height for messages.
	availableHeight := m.messageViewportHeight()

	// Truncate messages to fit viewport.
	msgLines := strings.Split(messagesView, "\n")
	totalLines := len(msgLines)

	startLine := totalLines - availableHeight - m.chatModel.Scroll
	if startLine < 0 {
		startLine = 0
	}
	endLine := startLine + availableHeight
	if endLine > totalLines {
		endLine = totalLines
	}

	visibleMessages := strings.Join(msgLines[startLine:endLine], "\n")

	// Pad to fill available space, leaving 1 blank line between messages and status bar.
	visibleLineCount := strings.Count(visibleMessages, "\n") + 1
	for visibleLineCount < availableHeight-1 {
		visibleMessages += "\n"
		visibleLineCount++
	}
	visibleMessages = m.overlaySearchPopup(visibleMessages, mainWidth)

	// Note: width constraint is handled by glamour's WithWordWrap(contentWidth) in chatModel.UpdateRenderer.
	// lipgloss.Width() counts raw bytes including invisible ANSI codes, causing wrapping issues.

	// Render matrix rain as full-width top bar (when active).
	matrixBar := m.matrix.render()

	// Horizontal rule for separating sections.
	hrStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#585b70")) // Catppuccin Mocha surface2
	hr := hrStyle.Render(strings.Repeat("─", mainWidth))

	var b strings.Builder
	if matrixBar != "" {
		b.WriteString(hr)
		b.WriteString("\n")
		b.WriteString(matrixBar)
		b.WriteString("\n")
		b.WriteString(hr)
		b.WriteString("\n")
	} else {
		b.WriteString("\n") // blank line at top when no matrix
	}
	b.WriteString(visibleMessages)

	// Render branch popup if open.
	if m.branchPopup != nil {
		popupView := m.renderBranchPopup()
		b.WriteString(popupView)
		b.WriteString("\n")
	}

	// Render item picker popup if open.
	if m.itemPicker != nil {
		popupView := m.renderItemPicker()
		b.WriteString(popupView)
		b.WriteString("\n")
	}

	b.WriteString(hr)
	b.WriteString("\n")
	b.WriteString(statusBar)
	b.WriteString("\n")
	b.WriteString(hr)
	b.WriteString("\n")
	inputCursorY := strings.Count(b.String(), "\n")
	b.WriteString(inputArea)
	b.WriteString("\n")
	b.WriteString(hr)

	leftPanel := b.String()

	var final string
	if showSidebar {
		hostName, _ := os.Hostname()
		sidebarInput := SidebarRenderInput{
			Width:        sidebarWidth,
			Height:       m.height,
			Mascot:       m.mascot(),
			ProviderName: m.providerDisplayName(),
			ModelName:    m.cfg.ModelName,
			GitBranch:    m.statusModel.GitBranch,
			DiffAdded:    m.diffAdded,
			DiffRemoved:  m.diffRemoved,
			Running:      m.running,
			TokenTracker: m.cfg.TokenTracker,
			AppVersion:   m.cfg.AppVersion,
			HostName:     hostName,
			FolderName:   sidebarFolderName(m.cwd()),
			Messages:     m.chatModel.Messages,
			ActiveTool:   m.statusModel.ActiveTool,
			LoadingItems: m.loadingItems,
			MatrixLines:  "",
			StatusLine:   "",
			Skills:       m.cfg.Skills,
		}
		sidebar := RenderSidebar(sidebarInput)
		final = lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, sidebar)
	} else {
		final = leftPanel
	}

	v := tea.NewView(final)
	if inputCursor != nil {
		inputCursor.Y += inputCursorY
		v.Cursor = inputCursor
	}
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// drainTerminalResponses discards any pending terminal response sequences
// (e.g. cursor position reports, DECRQM replies) that may arrive after the
// TUI exits. Without this, late responses leak into the shell prompt as garbage
// like "[14;1R[?2026;2$y".
func drainTerminalResponses() {
	f := os.Stdin
	// Switch stdin to non-blocking so we can read without waiting.
	if err := setNonBlock(f); err != nil {
		return
	}
	defer setBlock(f) //nolint:errcheck

	buf := make([]byte, 256)
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		n, _ := f.Read(buf)
		if n == 0 {
			break
		}
	}
}

const startupProgressBarWidth = 8
const startupProductName = "Attention"

func renderStartupMatrixLine(phase int, appVersion string, loadingItems map[string]bool, loadingTotal int) string {
	versionSuffix := ""
	if appVersion != "" {
		versionSuffix = " " + appVersion
	}
	progress := renderStartupProgress(loadingItems, loadingTotal)
	detail := renderStartupDetail(loadingItems)
	width := 48
	if width < 1 || len(matrixRunes) == 0 {
		return "Loading " + startupProductName + versionSuffix + progress + detail + " .."
	}
	bright := lipgloss.NewStyle().Foreground(lipgloss.Color("#94e2d5")).Bold(true)
	mid := lipgloss.NewStyle().Foreground(lipgloss.Color("#89b4fa"))
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("#45475a"))
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("#cba6f7")).Bold(true)

	dotCount := 2 + phase%3
	wave := phase % (2 * (width - 1))
	if wave >= width {
		wave = 2*(width-1) - wave
	}

	var b strings.Builder
	b.WriteString(accent.Render("Loading " + startupProductName + versionSuffix + progress + detail + strings.Repeat(".", dotCount)))
	b.WriteString(" ")
	for i := 0; i < width; i++ {
		r := matrixRunes[(i+phase*7)%len(matrixRunes)]
		delta := i - wave
		if delta < 0 {
			delta = -delta
		}
		switch {
		case delta == 0:
			b.WriteString(bright.Render(string(r)))
		case delta <= 2:
			b.WriteString(mid.Render(string(r)))
		default:
			b.WriteString(dim.Render(string(r)))
		}
	}
	return b.String()
}

func renderStartupProgress(loadingItems map[string]bool, loadingTotal int) string {
	if loadingItems == nil {
		return ""
	}

	done := 0
	for _, itemDone := range loadingItems {
		if itemDone {
			done++
		}
	}

	total := loadingTotal
	if total < len(loadingItems) {
		total = len(loadingItems)
	}
	if total < 1 {
		return fmt.Sprintf(" [%s 0%%]", strings.Repeat("░", startupProgressBarWidth))
	}

	pct := done * 100 / total
	if pct > 100 {
		pct = 100
	}

	filled := done * startupProgressBarWidth / total
	if done > 0 && filled == 0 {
		filled = 1
	}
	if filled > startupProgressBarWidth {
		filled = startupProgressBarWidth
	}

	bar := strings.Repeat("█", filled) + strings.Repeat("░", startupProgressBarWidth-filled)
	return fmt.Sprintf(" [%s %d%% %d/%d]", bar, pct, done, total)
}

func renderStartupDetail(loadingItems map[string]bool) string {
	if loadingItems == nil {
		return ""
	}
	if len(loadingItems) == 0 {
		return " starting init pipeline"
	}

	var pending []string
	for _, item := range sortedKeys(loadingItems) {
		if !loadingItems[item] {
			pending = append(pending, item)
		}
	}
	if len(pending) == 0 {
		return " finalizing init"
	}
	return " working: " + strings.Join(pending, ", ")
}

func (m *model) applyResize() {
	mainWidth := m.mainWidth()
	m.statusModel.Width = mainWidth
	if m.chatModel.Width != mainWidth {
		m.chatModel.UpdateRenderer(mainWidth)
	}
	m.clampScroll()
	// Pre-render or reflow matrix bar so width changes are visible immediately.
	if !m.matrix.active {
		m.matrix.feed("pi-go", mainWidth)
	} else {
		m.matrix.tick(mainWidth)
	}
	// Matrix height can affect the message viewport, so clamp again after it updates.
	m.clampScroll()
}

func (m *model) clampScroll() {
	maxScroll := m.chatModel.MaxScroll(m.messageViewportHeight())
	if m.chatModel.Scroll > maxScroll {
		m.chatModel.Scroll = maxScroll
	}
	if m.chatModel.Scroll < 0 {
		m.chatModel.Scroll = 0
	}
}

func (m *model) messageViewportHeight() int {
	mainWidth := m.mainWidth()
	if m.statusModel.Width != mainWidth {
		m.statusModel.Width = mainWidth
	}
	statusBar := m.statusModel.Render(m.statusRenderInput())
	inputArea := m.inputModel.View(m.running || m.loading)
	statusLines := strings.Count(statusBar, "\n") + 1
	inputLines := strings.Count(inputArea, "\n") + 1
	availableHeight := m.height - statusLines - inputLines - 4
	if m.matrix.render() != "" {
		availableHeight -= 2
	}
	if m.branchPopup != nil {
		availableHeight -= m.branchPopup.height + 6
	}
	if m.itemPicker != nil {
		availableHeight -= m.itemPicker.height + 6
	}
	if availableHeight < 1 {
		return 1
	}
	return availableHeight
}

// resizeDraining returns true for a short window after a terminal resize,
// during which key and paste input is suppressed to let terminal response
// sequences (OSC color replies, DECRPM, cursor position reports) drain.
func (m *model) resizeDraining() bool {
	return !m.resizeAt.IsZero() && time.Since(m.resizeAt) < 150*time.Millisecond
}

type resizeDrainDoneMsg struct {
	resizeAt time.Time
}

func resizeDrainDoneCmd(resizeAt time.Time) tea.Cmd {
	if resizeAt.IsZero() {
		return nil
	}
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
		return resizeDrainDoneMsg{resizeAt: resizeAt}
	})
}

func isResizeTextFragment(msg tea.KeyPressMsg) bool {
	return msg.Key().Text != ""
}

// cwd returns the working directory used for git/diff helpers. WorkDir was cut
// from Config in Phase 1, so this falls back to the process working directory.
func (m *model) cwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

// mainWidth returns the width of the main panel (excluding sidebar).
func (m *model) mainWidth() int {
	if m.width <= 0 {
		return 1
	}
	if m.width > 80 {
		w := m.width - SidebarWidth
		if w > 0 {
			return w
		}
	}
	return m.width
}

func (m *model) eyes() string {
	if m.face != nil {
		return m.face.Eyes()
	}
	return MoodIdle.Eyes()
}

func (m *model) mascot() string {
	if m.face != nil {
		return m.face.Mascot()
	}
	return MoodIdle.Mascot()
}

// refreshDiffStats updates the git diff line counts.
func (m *model) refreshDiffStats() {
	cwd := m.cwd()
	if cwd == "" {
		return
	}
	cmd := exec.Command("git", "diff", "--numstat", "HEAD")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return
	}
	var added, removed int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var a, r int
		if _, err := fmt.Sscanf(line, "%d\t%d\t", &a, &r); err == nil {
			added += a
			removed += r
		}
	}
	added += countUntrackedLines(cwd)
	m.diffAdded = added
	m.diffRemoved = removed
}

// countUntrackedLines counts total lines across untracked files.
func countUntrackedLines(cwd string) int {
	cmd := exec.Command("git", "ls-files", "--others", "--exclude-standard")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	total := 0
	for _, file := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if file == "" {
			continue
		}
		wc := exec.Command("wc", "-l", file)
		wc.Dir = cwd
		wcOut, err := wc.Output()
		if err != nil {
			continue
		}
		var lines int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(wcOut)), "%d", &lines); err == nil {
			total += lines
		}
	}
	return total
}

// statusRenderInput builds the StatusRenderInput from the current model state.
func (m *model) statusRenderInput() StatusRenderInput {
	var rc *runCycleInfo
	hostName, _ := os.Hostname()
	return StatusRenderInput{
		ProviderName: m.providerDisplayName(),
		ModelName:    m.cfg.ModelName,
		Running:      m.running,
		Eyes:         m.eyes(),
		Messages:     m.chatModel.Messages,
		TokenTracker: m.cfg.TokenTracker,
		DiffAdded:    m.diffAdded,
		DiffRemoved:  m.diffRemoved,
		RunCycle:     rc,
		FolderName:   sidebarFolderName(m.cwd()),
		HostName:     hostName,
		LoadingItems: m.loadingItems,
	}
}

// providerDisplayName returns the provider label shown in the status bar
// and sidebar.
func (m *model) providerDisplayName() string {
	return m.cfg.ProviderName
}

// detectBranch returns the current git branch name, or empty string.
func detectBranch(workDir string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	if workDir != "" {
		cmd.Dir = workDir
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// handleBranchSelect switches to the selected branch.
func (m *model) handleBranchSelect() (tea.Model, tea.Cmd) {
	if m.branchPopup == nil || len(m.branchPopup.branches) == 0 {
		m.branchPopup = nil
		return m, nil
	}

	selectedBranch := m.branchPopup.branches[m.branchPopup.selected]

	// Don't switch if already on this branch
	if selectedBranch == m.branchPopup.active {
		m.branchPopup = nil
		return m, nil
	}

	cwd := m.cwd()

	// Run git checkout in the background
	cmd := exec.Command("git", "checkout", selectedBranch)
	if cwd != "" {
		cmd.Dir = cwd
	}

	err := cmd.Run()
	if err != nil {
		m.chatModel.AppendWarning(fmt.Sprintf("Failed to switch branch: %v", err))
	} else {
		m.statusModel.GitBranch = selectedBranch
		m.refreshDiffStats()
	}

	m.branchPopup = nil
	return m, nil
}

// handleItemPickerSelect runs the kernel action for the selected picker item.
// It routes by the picker's kind to the matching rpcAgent method, reports the
// outcome as a chat notice, and refreshes diff stats. For /fork, the forked
// prompt text is placed back in the input box like pi did so the user can
// re-run or edit it.
func (m *model) handleItemPickerSelect() (tea.Model, tea.Cmd) {
	picker := m.itemPicker
	if picker == nil || len(picker.items) == 0 {
		m.itemPicker = nil
		return m, nil
	}
	item := picker.items[picker.selected]
	m.itemPicker = nil

	agent, ok := m.cfg.Agent.(*rpcAgent)
	if !ok {
		m.chatModel.AppendWarning("Picker is not available (no kernel backend)")
		return m, nil
	}

	switch picker.kind {
	case pickerFork:
		text, cancelled, err := agent.Fork(item.ID)
		if err != nil {
			m.chatModel.AppendWarning(fmt.Sprintf("/fork failed: %v", err))
			return m, nil
		}
		if cancelled {
			m.chatModel.AppendNotice("Fork cancelled.")
			return m, nil
		}
		// The kernel replaced the session at the fork point. Put the forked prompt
		// text in the input box so the user can re-run or edit it, like pi did.
		if text != "" {
			m.inputModel.SetText(text)
		}
		m.chatModel.AppendNotice("Forked the session. The selected prompt is in the input box.")
		m.refreshDiffStats()
		return m, nil

	case pickerResume:
		if err := agent.SwitchSession(item.ID); err != nil {
			m.chatModel.AppendWarning(fmt.Sprintf("/resume failed: %v", err))
			return m, nil
		}
		m.chatModel.AppendNotice(fmt.Sprintf("Switched to session %s.", item.Label))
		m.refreshDiffStats()
		return m, nil

	case pickerTree:
		if err := agent.NavigateTree(item.ID); err != nil {
			m.chatModel.AppendWarning(fmt.Sprintf("/tree failed: %v", err))
			return m, nil
		}
		m.chatModel.AppendNotice(fmt.Sprintf("Navigated to: %s", item.Label))
		m.refreshDiffStats()
		return m, nil

	case pickerModel:
		provider, id, ok := parseModelPickerID(item.ID)
		if !ok {
			m.chatModel.AppendWarning(fmt.Sprintf("/model failed: invalid model selection %q", item.Label))
			return m, nil
		}
		notice, id, provider, err := agent.SetModel(provider, id)
		if err != nil {
			m.chatModel.AppendWarning(fmt.Sprintf("/model failed: %v", err))
			return m, nil
		}
		m.cfg.ModelName = id
		m.cfg.ProviderName = provider
		m.chatModel.AppendNotice(notice)
		return m, nil

	default:
		m.chatModel.AppendWarning(fmt.Sprintf("Unknown picker action %q", picker.kind))
		return m, nil
	}
}

// resetCtrlCCount is a tea.Cmd that resets the Ctrl+C counter after a delay.
func resetCtrlCCount(m *model) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(2 * time.Second)
		return resetCtrlCCountMsg{}
	}
}

// msgResetCtrlCCount resets the Ctrl+C counter.
type resetCtrlCCountMsg struct{}

// loadingTickMsg advances the loading dots animation.
type loadingTickMsg struct{}

func (m *model) handleResetCtrlCCount() (tea.Model, tea.Cmd) {
	m.ctrlCCount = 0
	return m, nil
}

// handleSlashCommand dispatches a submitted "/..." line against the kernel
// command list (m.cfg.Commands — the single source of truth). It parses
// "/<name> <args>", looks <name> up VERBATIM, and routes by the command's source:
//
//   - builtin: invoked via the matching rpc action (new/compact/clone/reload/
//     session/name). model/fork/tree/resume open an interactive item picker
//     (modeled on the branch popup) that drives the corresponding rpc on select.
//   - skill / prompt: submitted as a prompt with the FULL verbatim "/..." text
//     so the kernel expands and runs it.
//   - plugin: invoked through the kernel command dispatcher over rpc.
//   - not found: the standard "Unknown command" warning.
func (m *model) handleSlashCommand(text string) (tea.Model, tea.Cmd) {
	m.inputModel.Clear()
	m.searchPopup = nil

	trimmed := strings.TrimSpace(text)
	// Parse "/<name> <args>" — name is the first whitespace-delimited token
	// (sans leading slash), args is the remainder.
	body := strings.TrimPrefix(trimmed, "/")
	name := body
	args := ""
	if i := strings.IndexAny(body, " \t"); i >= 0 {
		name = body[:i]
		args = strings.TrimSpace(body[i+1:])
	}

	// Look the command up VERBATIM in the kernel command list.
	var found *CommandInfo
	for i := range m.cfg.Commands {
		if m.cfg.Commands[i].Name == name {
			found = &m.cfg.Commands[i]
			break
		}
	}
	if found == nil {
		m.chatModel.AppendWarning(fmt.Sprintf("Unknown command: %s", trimmed))
		return m, nil
	}

	switch found.Source {
	case "skill", "prompt":
		// The kernel expands and runs these. Submit the FULL verbatim "/..." text
		// (e.g. "/skill:review") so ExpandSkillCommand / prompt expansion fires.
		return m.submitPrompt(trimmed, nil)
	case "plugin":
		return m.dispatchPluginCommand(name, args)
	case "builtin":
		return m.dispatchBuiltin(name, args)
	default:
		// Unknown source: be honest rather than guess.
		m.chatModel.AppendWarning(fmt.Sprintf("/%s has an unsupported command source %q", name, found.Source))
		return m, nil
	}
}

func skillsFromCommands(commands []CommandInfo) []Skill {
	skills := make([]Skill, 0)
	for _, c := range commands {
		if c.Source != "skill" {
			continue
		}
		skills = append(skills, Skill{
			Name:        strings.TrimPrefix(c.Name, "skill:"),
			Description: c.Description,
			Source:      "user",
		})
	}
	return skills
}

func (m *model) dispatchPluginCommand(name, args string) (tea.Model, tea.Cmd) {
	if m.cfg.Agent == nil {
		m.chatModel.AppendWarning(fmt.Sprintf("/%s is not available (no kernel backend)", name))
		return m, nil
	}
	result, err := m.cfg.Agent.DispatchCommand(name, args)
	if err != nil {
		m.chatModel.AppendWarning(fmt.Sprintf("/%s failed: %v", name, err))
		return m, nil
	}
	for _, notification := range result.Notifications {
		if notification.Message == "" {
			continue
		}
		switch notification.Level {
		case "warning", "error":
			m.chatModel.AppendWarning(notification.Message)
		default:
			m.chatModel.AppendNotice(notification.Message)
		}
	}
	return m, nil
}

// dispatchBuiltin runs a kernel builtin slash command by its verbatim name.
// Actions needing an interactive selector (model/fork/tree/resume) open an item
// picker that drives the corresponding rpc when the user selects a row.
func (m *model) dispatchBuiltin(name, args string) (tea.Model, tea.Cmd) {
	if name == "reload" {
		if m.cfg.Agent == nil {
			m.chatModel.AppendWarning(fmt.Sprintf("/%s is not available (no kernel backend)", name))
			return m, nil
		}
		notice, err := m.cfg.Agent.Reload()
		if err != nil {
			m.chatModel.AppendWarning(fmt.Sprintf("/%s failed: %v", name, err))
			return m, nil
		}
		if commands := m.cfg.Agent.FetchCommands(); commands != nil {
			m.cfg.Commands = commands
			m.inputModel.Commands = commands
			skills := skillsFromCommands(commands)
			m.cfg.Skills = skills
			m.inputModel.Skills = skills
		} else {
			m.chatModel.AppendWarning("Reloaded, but command list refresh failed.")
		}
		m.chatModel.AppendNotice(notice)
		return m, nil
	}

	agent, ok := m.cfg.Agent.(*rpcAgent)
	if !ok {
		m.chatModel.AppendWarning(fmt.Sprintf("/%s is not available (no kernel backend)", name))
		return m, nil
	}

	var (
		notice string
		err    error
	)
	switch name {
	case "new":
		notice, err = agent.NewSession()
	case "compact":
		notice, err = agent.Compact()
	case "clone":
		notice, err = agent.Clone()
	case "model":
		return m.openModelPicker(agent)
	case "session":
		notice, err = agent.SessionStats()
	case "name":
		if args == "" {
			m.chatModel.AppendWarning("Usage: /name <session name>")
			return m, nil
		}
		notice, err = agent.SetSessionName(args)
	case "fork":
		return m.openForkPicker(agent)
	case "resume":
		return m.openResumePicker(agent)
	case "tree":
		return m.openTreePicker(agent)
	default:
		m.chatModel.AppendWarning(fmt.Sprintf("/%s is not supported in the viewer yet", name))
		return m, nil
	}

	if err != nil {
		m.chatModel.AppendWarning(fmt.Sprintf("/%s failed: %v", name, err))
		return m, nil
	}
	m.chatModel.AppendNotice(notice)
	return m, nil
}

// openForkPicker fetches the forkable user messages and opens the fork picker.
// An empty list (nothing to fork) shows an honest notice instead.
func (m *model) openForkPicker(agent *rpcAgent) (tea.Model, tea.Cmd) {
	items, err := agent.ForkMessages()
	if err != nil {
		m.chatModel.AppendWarning(fmt.Sprintf("/fork failed: %v", err))
		return m, nil
	}
	if len(items) == 0 {
		m.chatModel.AppendNotice("Nothing to fork — no user messages yet.")
		return m, nil
	}
	m.newItemPicker(pickerFork, "Fork from message (Enter to fork, Esc to close)", items)
	return m, nil
}

// openResumePicker scans the current session's directory for other *.jsonl
// session files and opens the resume picker. The current session file is
// skipped. An empty list (no other sessions) shows an honest notice instead.
func (m *model) openResumePicker(agent *rpcAgent) (tea.Model, tea.Cmd) {
	current, err := agent.SessionFile()
	if err != nil {
		m.chatModel.AppendWarning(fmt.Sprintf("/resume failed: %v", err))
		return m, nil
	}
	if current == "" {
		m.chatModel.AppendNotice("No session file on disk — cannot list sessions.")
		return m, nil
	}
	items := listSessionFiles(current)
	if len(items) == 0 {
		m.chatModel.AppendNotice("No other sessions to resume.")
		return m, nil
	}
	m.newItemPicker(pickerResume, "Resume session (Enter to switch, Esc to close)", items)
	return m, nil
}

// openTreePicker fetches the session's navigable entries and opens the tree
// picker. An empty list shows an honest notice instead.
func (m *model) openTreePicker(agent *rpcAgent) (tea.Model, tea.Cmd) {
	items, err := agent.Entries()
	if err != nil {
		m.chatModel.AppendWarning(fmt.Sprintf("/tree failed: %v", err))
		return m, nil
	}
	if len(items) == 0 {
		m.chatModel.AppendNotice("Nothing to navigate — the session has no entries yet.")
		return m, nil
	}
	m.newItemPicker(pickerTree, "Navigate tree (Enter to jump, Esc to close)", items)
	return m, nil
}

// openModelPicker fetches configured models from the kernel and opens the model
// picker. An empty list means the kernel has no switchable model inventory.
func (m *model) openModelPicker(agent *rpcAgent) (tea.Model, tea.Cmd) {
	items, err := agent.AvailableModels()
	if err != nil {
		m.chatModel.AppendWarning(fmt.Sprintf("/model failed: %v", err))
		return m, nil
	}
	if len(items) == 0 {
		m.chatModel.AppendNotice("No models are available from the kernel.")
		return m, nil
	}
	m.newItemPicker(pickerModel, "Switch model (Enter to switch, Esc to close)", items)
	return m, nil
}

const modelPickerSeparator = "\t"

func modelPickerItems(models []wireModel) []PickerItem {
	items := make([]PickerItem, 0, len(models))
	for _, model := range models {
		if model.ID == "" || model.Provider == "" {
			continue
		}
		items = append(items, PickerItem{
			ID:    model.Provider + modelPickerSeparator + model.ID,
			Label: modelPickerLabel(model),
		})
	}
	return items
}

func parseModelPickerID(value string) (provider, id string, ok bool) {
	provider, id, ok = strings.Cut(value, modelPickerSeparator)
	return provider, id, ok && provider != "" && id != ""
}

func modelPickerLabel(model wireModel) string {
	name := model.Name
	if name == "" {
		name = model.ID
	}
	return fmt.Sprintf("%s (%s)", name, model.Provider)
}

func modelSwitchNotice(model wireModel) string {
	return fmt.Sprintf("Switched model to %s (%s).", modelPickerName(model), model.Provider)
}

func modelPickerName(model wireModel) string {
	if model.Name != "" {
		return model.Name
	}
	return model.ID
}

// listSessionFiles scans the directory of currentFile for sibling *.jsonl
// session files, skipping currentFile itself. Each item's ID is the absolute
// path (sent to switch_session) and Label is the filename.
func listSessionFiles(currentFile string) []PickerItem {
	dir := filepath.Dir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	currentBase := filepath.Base(currentFile)
	var items []PickerItem
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") || name == currentBase {
			continue
		}
		items = append(items, PickerItem{
			ID:    filepath.Join(dir, name),
			Label: strings.TrimSuffix(name, ".jsonl"),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Label < items[j].Label })
	return items
}

func (m *model) shouldShowSlashCommandPopup() bool {
	if m.running || m.loading {
		return false
	}
	text := m.inputModel.Text
	return strings.HasPrefix(text, "/") && !strings.ContainsAny(text, " \t\n\r")
}

func (m *model) overlaySearchPopup(messages string, mainWidth int) string {
	popup := m.renderSearchPopup(max(0, mainWidth-4))
	if popup == "" {
		return messages
	}

	lines := strings.Split(messages, "\n")
	viewportHeight := len(lines)

	popupLines := strings.Split(popup, "\n")
	if viewportHeight > 0 && len(popupLines) > viewportHeight {
		popupLines = popupLines[:viewportHeight]
	}
	if len(popupLines) == 0 {
		return strings.Join(lines, "\n")
	}

	popupWidth := maxLineWidth(popupLines)
	left := 0
	if mainWidth > popupWidth {
		left = (mainWidth - popupWidth) / 2
	}
	start := 0
	if viewportHeight > len(popupLines) {
		start = viewportHeight - len(popupLines) - 1
	}
	if start < 0 {
		start = 0
	}

	for i, line := range popupLines {
		idx := start + i
		if idx >= len(lines) {
			break
		}
		lines[idx] = overlayPopupLine(line, left, mainWidth)
	}
	return strings.Join(lines, "\n")
}

func maxLineWidth(lines []string) int {
	width := 0
	for _, line := range lines {
		width = max(width, lipgloss.Width(line))
	}
	return width
}

func overlayPopupLine(line string, left int, totalWidth int) string {
	if totalWidth <= 0 {
		return line
	}
	if left < 0 {
		left = 0
	}
	prefix := strings.Repeat(" ", left)
	rendered := prefix + line
	if width := lipgloss.Width(rendered); width < totalWidth {
		rendered += strings.Repeat(" ", totalWidth-width)
	}
	return rendered
}

func (m *model) renderSearchPopup(width int) string {
	if m.searchPopup == nil {
		return ""
	}
	if width < 24 {
		width = 24
	}

	sp := m.searchPopup
	bg := lipgloss.Color("236")

	// Get colors based on mode.
	popupStyle := lipgloss.NewStyle().Background(bg)
	headerStyle := lipgloss.NewStyle().Background(bg).Bold(true)
	searchStyle := lipgloss.NewStyle().Background(bg)
	itemStyle := lipgloss.NewStyle().Background(bg)
	selectedItemStyle := lipgloss.NewStyle().Background(lipgloss.Color("15"))

	var header string

	switch sp.mode {
	case searchModeCommands:
		border := lipgloss.Color("33") // cyan for commands
		popupStyle = popupStyle.
			Foreground(lipgloss.Color("252")).
			Border(lipgloss.RoundedBorder(), true, true, true, true).
			BorderForeground(border).
			Width(width)
		headerStyle = headerStyle.Foreground(lipgloss.Color("252")).Width(width)
		searchStyle = searchStyle.Foreground(lipgloss.Color("245"))
		itemStyle = itemStyle.Foreground(lipgloss.Color("81")) // teal
		selectedItemStyle = selectedItemStyle.Background(lipgloss.Color("33"))
		header = "Commands"
	case searchModeHistory:
		border := lipgloss.Color("208") // orange for history
		popupStyle = popupStyle.
			Foreground(lipgloss.Color("252")).
			Border(lipgloss.RoundedBorder(), true, true, true, true).
			BorderForeground(border).
			Width(width)
		headerStyle = headerStyle.Foreground(lipgloss.Color("252")).Width(width)
		searchStyle = searchStyle.Foreground(lipgloss.Color("245"))
		itemStyle = itemStyle.Foreground(lipgloss.Color("208")) // orange
		selectedItemStyle = selectedItemStyle.Background(lipgloss.Color("208"))
		header = "History"
	}

	var b strings.Builder

	// Header with count.
	if len(sp.filtered) > 0 {
		header = fmt.Sprintf("%s (%d)", header, len(sp.filtered))
	}
	b.WriteString(headerStyle.Render(header))

	// Search prompt line.
	if sp.search != "" {
		b.WriteString("\n")
		searchLine := fmt.Sprintf("  Search: %s", sp.search)
		b.WriteString(searchStyle.Width(width).Render(clipRunes(searchLine, width)))
	} else {
		b.WriteString("\n")
		b.WriteString(searchStyle.Width(width).Render("  Search... (type to filter)"))
	}

	if len(sp.filtered) == 0 {
		b.WriteString("\n")
		if sp.mode == searchModeCommands {
			b.WriteString(searchStyle.Width(width).Render("  No matching commands"))
		} else {
			b.WriteString(searchStyle.Width(width).Render("  No matching history"))
		}
		return popupStyle.Render(b.String())
	}

	// Item list.
	for i := 0; i < sp.height && i < len(sp.filtered); i++ {
		idx := sp.scrollOff + i
		item := sp.filtered[idx]
		prefix := "  "
		currentItemStyle := itemStyle
		if idx == sp.selected {
			// Always highlight the selected item.
			prefix = "> "
			currentItemStyle = selectedItemStyle
		}

		line := prefix + item.Text
		// Add description for commands.
		if item.Description != "" && sp.mode == searchModeCommands {
			desc := clipRunes(item.Description, width*50/100)
			if desc != "" {
				line += "  " + desc
			}
		}

		b.WriteString("\n")
		b.WriteString(currentItemStyle.Width(width).Render(clipRunes(line, width)))
	}

	return popupStyle.Render(b.String())
}

// allSlashCommandCandidates builds the full candidate list from the kernel
// command list (the single source of truth), presenting each command name in
// its verbatim slash form (e.g. "/compact", "/skill:review").
func allSlashCommandCandidates(commands []CommandInfo) []CompletionCandidate {
	seen := make(map[string]bool)
	candidates := make([]CompletionCandidate, 0, len(commands))
	for _, c := range commands {
		text := "/" + c.Name
		if seen[text] {
			continue
		}
		seen[text] = true
		candidates = append(candidates, CompletionCandidate{
			Text:        text,
			Description: c.Description,
			Type:        CompletionTypeCommand,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return strings.ToLower(candidates[i].Text) < strings.ToLower(candidates[j].Text)
	})
	return candidates
}

func clipRunes(s string, width int) string {
	if width <= 0 {
		return ""
	}
	// Measure by DISPLAY WIDTH (CJK/East-Asian glyphs occupy 2 columns), not rune
	// count, so wide-character labels don't overflow their column budget.
	if runewidth.StringWidth(s) <= width {
		return s
	}
	if width <= 3 {
		return runewidth.Truncate(s, width, "")
	}
	return runewidth.Truncate(s, width, "...")
}

// renderBranchPopup renders the branch list popup.
func (m *model) renderBranchPopup() string {
	if m.branchPopup == nil {
		return ""
	}

	popup := m.branchPopup
	bg := lipgloss.Color("236")
	border := lipgloss.Color("240")
	selected := lipgloss.Color("33")
	activeFg := lipgloss.Color("35")
	dimFg := lipgloss.Color("243")

	style := lipgloss.NewStyle().
		Background(bg).
		Foreground(lipgloss.Color("252")).
		Border(lipgloss.ThickBorder(), true, true, true, true).
		BorderForeground(border).
		Width(m.width - 10)

	// Calculate popup position (centered horizontally, near the bottom)
	popupWidth := m.width - 10

	var b strings.Builder
	b.WriteString("\n")

	// Header
	header := lipgloss.NewStyle().
		Background(bg).
		Foreground(lipgloss.Color("252")).
		Bold(true).
		Width(popupWidth).
		Align(lipgloss.Center).
		Render("Git Branches (Enter to switch, Esc to close)")
	b.WriteString(header)
	b.WriteString("\n")

	// Render visible branches
	branches := popup.branches
	height := popup.height
	scrollOff := popup.scrollOff

	if len(branches) > height {
		branches = branches[scrollOff : scrollOff+height]
	}

	for i, branch := range branches {
		actualIndex := i + scrollOff
		isSelected := actualIndex == popup.selected
		isActive := branch == popup.active

		var line string
		if isActive {
			line = fmt.Sprintf("  ● %s (current)", branch)
		} else {
			line = fmt.Sprintf("    %s", branch)
		}

		if isSelected {
			line = "> " + line[2:] // Replace leading spaces with ">"
		}

		var lineStyle lipgloss.Style
		switch {
		case isSelected:
			lineStyle = lipgloss.NewStyle().Background(selected).Foreground(lipgloss.Color("15"))
		case isActive:
			lineStyle = lipgloss.NewStyle().Background(bg).Foreground(activeFg)
		default:
			lineStyle = lipgloss.NewStyle().Background(bg).Foreground(dimFg)
		}

		b.WriteString(lineStyle.Width(popupWidth).Render(line))
		b.WriteString("\n")
	}

	// Show scroll indicator if needed
	if len(popup.branches) > popup.height {
		scrollStyle := lipgloss.NewStyle().Background(bg).Foreground(dimFg)
		b.WriteString(scrollStyle.Render("  ↑↓ scroll"))
	}

	return style.Render(b.String())
}

// renderItemPicker renders the generic {id,label} list popup behind /fork,
// /resume, and /tree. It mirrors renderBranchPopup's layout (thick border,
// centered header, selected-row highlight, scroll indicator).
func (m *model) renderItemPicker() string {
	if m.itemPicker == nil {
		return ""
	}

	popup := m.itemPicker
	bg := lipgloss.Color("236")
	border := lipgloss.Color("240")
	selected := lipgloss.Color("33")
	dimFg := lipgloss.Color("243")

	style := lipgloss.NewStyle().
		Background(bg).
		Foreground(lipgloss.Color("252")).
		Border(lipgloss.ThickBorder(), true, true, true, true).
		BorderForeground(border).
		Width(m.width - 10)

	popupWidth := m.width - 10

	var b strings.Builder
	b.WriteString("\n")

	// Header.
	header := lipgloss.NewStyle().
		Background(bg).
		Foreground(lipgloss.Color("252")).
		Bold(true).
		Width(popupWidth).
		Align(lipgloss.Center).
		Render(popup.title)
	b.WriteString(header)
	b.WriteString("\n")

	// Render visible items.
	items := popup.items
	height := popup.height
	scrollOff := popup.scrollOff

	if len(items) > height {
		items = items[scrollOff : scrollOff+height]
	}

	for i, item := range items {
		actualIndex := i + scrollOff
		isSelected := actualIndex == popup.selected

		label := item.Label
		if popupWidth > 4 {
			label = clipRunes(label, popupWidth-4)
		}

		var line string
		if isSelected {
			line = "> " + label
		} else {
			line = "  " + label
		}

		var lineStyle lipgloss.Style
		if isSelected {
			lineStyle = lipgloss.NewStyle().Background(selected).Foreground(lipgloss.Color("15"))
		} else {
			lineStyle = lipgloss.NewStyle().Background(bg).Foreground(dimFg)
		}

		b.WriteString(lineStyle.Width(popupWidth).Render(line))
		b.WriteString("\n")
	}

	// Show scroll indicator if needed.
	if len(popup.items) > popup.height {
		scrollStyle := lipgloss.NewStyle().Background(bg).Foreground(dimFg)
		b.WriteString(scrollStyle.Render("  ↑↓ scroll"))
	}

	return style.Render(b.String())
}
