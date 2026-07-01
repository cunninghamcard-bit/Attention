package builtin_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/execenv/local"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
	"github.com/cunninghamcard-bit/Attention/internal/tool/builtin"
)

func TestBuiltinToolPresets(t *testing.T) {
	t.Parallel()

	env := local.New(t.TempDir())
	tests := []struct {
		name string
		defs []extension.ToolDefinition
		want []string
	}{
		{
			name: "coding",
			defs: builtin.NewCodingTools(env, ""),
			want: []string{"read", "bash", "edit", "write"},
		},
		{
			name: "read only",
			defs: builtin.NewReadOnlyTools(env),
			want: []string{"read", "grep", "find", "ls"},
		},
		{
			name: "all",
			defs: builtin.NewAllTools(env, ""),
			want: []string{"read", "bash", "edit", "write", "grep", "find", "ls"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolDefinitionNames(tt.defs)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("tool names = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReadTool(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	if err := env.WriteFile(ctx, "notes.txt", []byte("alpha\nbeta\n")); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}

	result, err := executeTool(ctx, builtin.NewReadTool(env), map[string]any{"path": "notes.txt"}, nil)
	if err != nil {
		t.Fatalf("read Execute returned Go error: %v", err)
	}
	if got := resultText(t, result); got != "alpha\nbeta\n" {
		t.Fatalf("read text = %q, want file content", got)
	}

	missing, err := executeTool(ctx, builtin.NewReadTool(env), map[string]any{"path": "missing.txt"}, nil)
	if err != nil {
		t.Fatalf("read missing returned Go error: %v", err)
	}
	if !isErrorResult(missing) {
		t.Fatalf("read missing details = %#v, want isError result", missing.Details)
	}
	if !strings.Contains(resultText(t, missing), "Could not read file") {
		t.Fatalf("read missing text = %q, want readable error", resultText(t, missing))
	}
}

func toolDefinitionNames(defs []extension.ToolDefinition) []string {
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	return names
}

func TestReadToolReadsImages(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	pngBytes := pngFixture(t, 1, 1)
	if err := env.WriteFile(ctx, "image.png", pngBytes); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}

	result, err := executeTool(ctx, builtin.NewReadTool(env), map[string]any{"path": "image.png"}, nil)
	if err != nil {
		t.Fatalf("read Execute returned Go error: %v", err)
	}
	if len(result.Content) != 2 {
		t.Fatalf("Content len = %d, want guidance text and image", len(result.Content))
	}

	text := result.Content[0]
	if text.Type != ai.ContentText {
		t.Fatalf("Content[0].Type = %q, want text", text.Type)
	}
	if !strings.Contains(text.Text, "Read image file [image/png]") {
		t.Fatalf("image guidance = %q, want read image note", text.Text)
	}

	image := result.Content[1]
	if image.Type != ai.ContentImage {
		t.Fatalf("Content[1].Type = %q, want image", image.Type)
	}
	if image.MimeType != "image/png" {
		t.Fatalf("image MimeType = %q, want image/png", image.MimeType)
	}
	if image.ImageData != base64.StdEncoding.EncodeToString(pngBytes) {
		t.Fatalf("image data = %q, want base64 fixture", image.ImageData)
	}
	if image.Text != "" {
		t.Fatalf("image text = %q, want image block not text content", image.Text)
	}

	if err := env.WriteFile(ctx, "image.bin", pngBytes); err != nil {
		t.Fatalf("WriteFile content-detected fixture: %v", err)
	}

	contentDetected, err := executeTool(ctx, builtin.NewReadTool(env), map[string]any{"path": "image.bin"}, nil)
	if err != nil {
		t.Fatalf("read content-detected image returned Go error: %v", err)
	}
	if len(contentDetected.Content) != 2 {
		t.Fatalf("content-detected Content len = %d, want guidance text and image", len(contentDetected.Content))
	}
	if contentDetected.Content[1].Type != ai.ContentImage {
		t.Fatalf("content-detected Content[1].Type = %q, want image", contentDetected.Content[1].Type)
	}
	if contentDetected.Content[1].MimeType != "image/png" {
		t.Fatalf("content-detected MimeType = %q, want image/png", contentDetected.Content[1].MimeType)
	}

	nonVision, err := executeToolWithContext(
		ctx,
		builtin.NewReadTool(env),
		map[string]any{"path": "image.png"},
		nil,
		extension.ExtensionContext{
			Model: func() ai.Model {
				return ai.Model{
					ID:    "text-only",
					Input: []ai.InputCapability{ai.InputText},
				}
			},
		},
	)
	if err != nil {
		t.Fatalf("read non-vision image returned Go error: %v", err)
	}
	if len(nonVision.Content) != 2 {
		t.Fatalf("non-vision Content len = %d, want guidance text and image", len(nonVision.Content))
	}
	if !strings.Contains(nonVision.Content[0].Text, "Current model does not support images") {
		t.Fatalf("non-vision guidance = %q, want model support note", nonVision.Content[0].Text)
	}
}

func TestReadToolResizesLargeImages(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	pngBytes := pngFixture(t, 2100, 100)
	if err := env.WriteFile(ctx, "wide.png", pngBytes); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}

	result, err := executeTool(ctx, builtin.NewReadTool(env), map[string]any{"path": "wide.png"}, nil)
	if err != nil {
		t.Fatalf("read resized image returned Go error: %v", err)
	}
	if len(result.Content) != 2 {
		t.Fatalf("Content len = %d, want guidance text and image", len(result.Content))
	}

	text := result.Content[0].Text
	if !strings.Contains(text, "Read image file [image/png]") {
		t.Fatalf("image guidance = %q, want resized image mime", text)
	}
	if !strings.Contains(text, "original 2100x100, displayed at 2000x95") {
		t.Fatalf("image guidance = %q, want dimension note", text)
	}
	if !strings.Contains(text, "Multiply coordinates by 1.05") {
		t.Fatalf("image guidance = %q, want coordinate scale note", text)
	}

	imageBlock := result.Content[1]
	if imageBlock.Type != ai.ContentImage {
		t.Fatalf("Content[1].Type = %q, want image", imageBlock.Type)
	}
	decoded, err := base64.StdEncoding.DecodeString(imageBlock.ImageData)
	if err != nil {
		t.Fatalf("Decode resized image data: %v", err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(decoded))
	if err != nil {
		t.Fatalf("Decode resized image config: %v", err)
	}
	if config.Width != 2000 || config.Height != 95 {
		t.Fatalf("resized image dimensions = %dx%d, want 2000x95", config.Width, config.Height)
	}
}

func TestReadToolResolvesPathVariants(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())

	if err := env.WriteFile(ctx, "capture\u2019s.txt", []byte("curly\n")); err != nil {
		t.Fatalf("WriteFile curly fixture: %v", err)
	}
	curly, err := executeTool(ctx, builtin.NewReadTool(env), map[string]any{"path": "capture's.txt"}, nil)
	if err != nil {
		t.Fatalf("read curly variant returned Go error: %v", err)
	}
	if got := resultText(t, curly); got != "curly\n" {
		t.Fatalf("read curly variant = %q, want file content", got)
	}

	if err := env.WriteFile(ctx, "Screen Shot 10\u202fAM.txt", []byte("ampm\n")); err != nil {
		t.Fatalf("WriteFile AM/PM fixture: %v", err)
	}
	ampm, err := executeTool(ctx, builtin.NewReadTool(env), map[string]any{"path": "Screen Shot 10 AM.txt"}, nil)
	if err != nil {
		t.Fatalf("read AM/PM variant returned Go error: %v", err)
	}
	if got := resultText(t, ampm); got != "ampm\n" {
		t.Fatalf("read AM/PM variant = %q, want file content", got)
	}

	if err := env.WriteFile(ctx, "Cafe\u0301.txt", []byte("nfd\n")); err != nil {
		t.Fatalf("WriteFile NFD fixture: %v", err)
	}
	nfd, err := executeTool(ctx, builtin.NewReadTool(env), map[string]any{"path": "Caf\u00e9.txt"}, nil)
	if err != nil {
		t.Fatalf("read NFD variant returned Go error: %v", err)
	}
	if got := resultText(t, nfd); got != "nfd\n" {
		t.Fatalf("read NFD variant = %q, want file content", got)
	}

	if err := env.WriteFile(ctx, "Cafe\u0301\u2019s.txt", []byte("nfd curly\n")); err != nil {
		t.Fatalf("WriteFile NFD curly fixture: %v", err)
	}
	nfdCurly, err := executeTool(ctx, builtin.NewReadTool(env), map[string]any{"path": "Caf\u00e9's.txt"}, nil)
	if err != nil {
		t.Fatalf("read NFD curly variant returned Go error: %v", err)
	}
	if got := resultText(t, nfdCurly); got != "nfd curly\n" {
		t.Fatalf("read NFD curly variant = %q, want file content", got)
	}
}

func pngFixture(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{R: 0x12, G: 0x8a, B: 0xcc, A: 0xff})
		}
	}

	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatalf("Encode PNG fixture: %v", err)
	}
	return buffer.Bytes()
}

func TestWriteAndEditToolsMutateFiles(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())

	writeResult, err := executeTool(ctx, builtin.NewWriteTool(env), map[string]any{
		"path":    "dir/file.txt",
		"content": "hello world\n",
	}, nil)
	if err != nil {
		t.Fatalf("write Execute returned Go error: %v", err)
	}
	if !strings.Contains(resultText(t, writeResult), "Successfully wrote") {
		t.Fatalf("write text = %q, want success", resultText(t, writeResult))
	}

	editResult, err := executeTool(ctx, builtin.NewEditTool(env), map[string]any{
		"path": "dir/file.txt",
		"edits": []any{
			map[string]any{"oldText": "hello", "newText": "hi"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("edit Execute returned Go error: %v", err)
	}
	if !strings.Contains(resultText(t, editResult), "Successfully replaced 1 block") {
		t.Fatalf("edit text = %q, want success", resultText(t, editResult))
	}

	got, err := env.ReadTextFile(ctx, "dir/file.txt")
	if err != nil {
		t.Fatalf("ReadTextFile after edit: %v", err)
	}
	if got != "hi world\n" {
		t.Fatalf("edited content = %q, want replacement applied", got)
	}
}

func TestEditToolExactEditsStillWork(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	if err := env.WriteFile(ctx, "notes.txt", []byte("alpha\nbeta\n")); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}

	result, err := executeTool(ctx, builtin.NewEditTool(env), map[string]any{
		"path": "notes.txt",
		"edits": []any{
			map[string]any{"oldText": "beta", "newText": "bravo"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("edit Execute returned Go error: %v", err)
	}
	if !strings.Contains(resultText(t, result), "Successfully replaced 1 block") {
		t.Fatalf("edit text = %q, want success", resultText(t, result))
	}

	got, err := env.ReadTextFile(ctx, "notes.txt")
	if err != nil {
		t.Fatalf("ReadTextFile after edit: %v", err)
	}
	if got != "alpha\nbravo\n" {
		t.Fatalf("edited content = %q, want exact replacement applied", got)
	}
}

func TestEditToolReportsDiffInDetails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	if err := env.WriteFile(ctx, "notes.txt", []byte("alpha\nbeta\n")); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}

	result, err := executeTool(ctx, builtin.NewEditTool(env), map[string]any{
		"path": "notes.txt",
		"edits": []any{
			map[string]any{"oldText": "beta", "newText": "bravo"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("edit Execute returned Go error: %v", err)
	}
	if !strings.Contains(resultText(t, result), "Successfully replaced 1 block") {
		t.Fatalf("edit text = %q, want success summary", resultText(t, result))
	}
	details := editDetails(t, result)
	if !strings.Contains(details.Diff, "-2 beta") || !strings.Contains(details.Diff, "+2 bravo") {
		t.Fatalf("edit details diff = %q, want line-numbered diff", details.Diff)
	}
	if !strings.Contains(details.Patch, "@@ -1,2 +1,2 @@") {
		t.Fatalf("edit details patch = %q, want unified hunk header", details.Patch)
	}
	if strings.Contains(details.Patch, "@@\n-alpha") {
		t.Fatalf("edit details patch = %q, want contextual patch instead of whole-file dump", details.Patch)
	}

	got, err := env.ReadTextFile(ctx, "notes.txt")
	if err != nil {
		t.Fatalf("ReadTextFile after edit: %v", err)
	}
	if got != "alpha\nbravo\n" {
		t.Fatalf("edited content = %q, want replacement applied", got)
	}
}

func TestEditToolFuzzyMatchesViaEdits(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	if err := env.WriteFile(ctx, "quote.txt", []byte("const msg = \u201Chello\u201D  \n")); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}

	// The file uses smart quotes and trailing whitespace; the edit targets ASCII
	// quotes with none. Exact match fails, so the unified path falls back to fuzzy
	// matching \u2014 matching pi's single edits[] engine.
	result, err := executeTool(ctx, builtin.NewEditTool(env), map[string]any{
		"path": "quote.txt",
		"edits": []any{
			map[string]any{"oldText": "const msg = \"hello\"\n", "newText": "const msg = \"hi\"\n"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("edit Execute returned Go error: %v", err)
	}
	if !strings.Contains(resultText(t, result), "Successfully replaced 1 block") {
		t.Fatalf("edit text = %q, want success summary", resultText(t, result))
	}

	got, err := env.ReadTextFile(ctx, "quote.txt")
	if err != nil {
		t.Fatalf("ReadTextFile after edit: %v", err)
	}
	if got != "const msg = \"hi\"\n" {
		t.Fatalf("edited content = %q, want fuzzy replacement applied", got)
	}
}

func editDetails(t *testing.T, result tool.Result) struct {
	Diff  string `json:"diff"`
	Patch string `json:"patch"`
} {
	t.Helper()
	encoded, err := json.Marshal(result.Details)
	if err != nil {
		t.Fatalf("marshal edit details: %v", err)
	}
	var details struct {
		Diff  string `json:"diff"`
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal(encoded, &details); err != nil {
		t.Fatalf("unmarshal edit details: %v", err)
	}
	return details
}

func TestLsToolListsDirectoryEntries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	if err := env.CreateDir(ctx, "dir/a", true); err != nil {
		t.Fatalf("CreateDir fixture: %v", err)
	}
	if err := env.WriteFile(ctx, "dir/b.txt", []byte("b")); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}

	result, err := executeTool(ctx, builtin.NewLsTool(env), map[string]any{"path": "dir"}, nil)
	if err != nil {
		t.Fatalf("ls Execute returned Go error: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "a/") || !strings.Contains(text, "b.txt") {
		t.Fatalf("ls text = %q, want directory and file entries", text)
	}
}

func TestBashToolCapturesOutputExitCodeAndStreams(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	var updates int

	result, err := executeTool(
		ctx,
		builtin.NewBashTool(env, ""),
		map[string]any{"command": "printf 'hello\\n'"},
		func(tool.Result) {
			updates++
		},
	)
	if err != nil {
		t.Fatalf("bash Execute returned Go error: %v", err)
	}
	text := resultText(t, result)
	if text != "hello\n" {
		t.Fatalf("bash text = %q, want output as-is", text)
	}
	if strings.Contains(text, "Exit code:") {
		t.Fatalf("bash text = %q, want no success exit-code footer", text)
	}
	if updates == 0 {
		t.Fatal("bash updates = 0, want streaming update")
	}
}

func TestBashToolThrottlesRapidStreamingUpdates(t *testing.T) {
	t.Parallel()

	chunks := make([]string, 20)
	for i := range chunks {
		chunks[i] = "chunk\n"
	}
	env := rapidChunkEnv{chunks: chunks}
	var updates int

	result, err := executeTool(
		context.Background(),
		builtin.NewBashTool(env, ""),
		map[string]any{"command": "rapid chunks"},
		func(tool.Result) {
			updates++
		},
	)
	if err != nil {
		t.Fatalf("bash Execute returned Go error: %v", err)
	}

	if updates < 1 {
		t.Fatalf("bash updates = %d, want at least one streaming update", updates)
	}
	if updates >= len(chunks) {
		t.Fatalf("bash updates = %d, want fewer than %d chunks", updates, len(chunks))
	}
	if got, want := resultText(t, result), strings.Join(chunks, ""); got != want {
		t.Fatalf("bash text = %q, want full output %q", got, want)
	}
}

func TestBashToolReportsNonZeroExitCode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())

	result, err := executeTool(ctx, builtin.NewBashTool(env, ""), map[string]any{
		"command": "printf 'failed\\n'; exit 7",
	}, nil)
	if err != nil {
		t.Fatalf("bash Execute returned Go error: %v", err)
	}
	if !isErrorResult(result) {
		t.Fatalf("bash non-zero details = %#v, want isError result", result.Details)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "failed\n") {
		t.Fatalf("bash non-zero text = %q, want command output", text)
	}
	if !strings.Contains(text, "Command exited with code 7") {
		t.Fatalf("bash non-zero text = %q, want exit status", text)
	}
}

func TestBashToolPrependsCommandPrefixOnOwnLine(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("prefix test uses POSIX shell")
	}

	ctx := context.Background()
	env := local.New(t.TempDir())

	// pi joins the prefix with a newline (tools/bash.ts:289), so a prefix
	// without a trailing separator still executes as its own statement.
	result, err := executeTool(ctx, builtin.NewBashTool(env, "echo PREFIX"), map[string]any{
		"command": "echo BODY",
	}, nil)
	if err != nil {
		t.Fatalf("bash Execute returned Go error: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "PREFIX\nBODY") {
		t.Fatalf("bash prefix text = %q, want PREFIX line before BODY", text)
	}
}

func TestBashToolPrependsPluginBinDirs(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("plugin bin test uses POSIX shell")
	}

	ctx := context.Background()
	env := local.New(t.TempDir())
	binDir := t.TempDir()
	toolPath := filepath.Join(binDir, "plugin-hello")
	if err := os.WriteFile(toolPath, []byte("#!/bin/sh\nprintf 'from-plugin\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(toolPath, 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := executeTool(ctx, builtin.NewBashTool(env, "", binDir), map[string]any{
		"command": "plugin-hello",
	}, nil)
	if err != nil {
		t.Fatalf("bash Execute returned Go error: %v", err)
	}
	if got := resultText(t, result); got != "from-plugin\n" {
		t.Fatalf("bash text = %q, want plugin bin command output", got)
	}
}

func TestBashToolReadsPluginBinDirsFromExtensionContext(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("plugin bin test uses POSIX shell")
	}

	ctx := context.Background()
	env := local.New(t.TempDir())
	binDir := t.TempDir()
	toolPath := filepath.Join(binDir, "plugin-ctx")
	if err := os.WriteFile(toolPath, []byte("#!/bin/sh\nprintf 'from-context\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(toolPath, 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := executeToolWithContext(
		ctx,
		builtin.NewBashTool(env, ""),
		map[string]any{"command": "plugin-ctx"},
		nil,
		extension.ExtensionContext{PluginBinDirs: []string{binDir}},
	)
	if err != nil {
		t.Fatalf("bash Execute returned Go error: %v", err)
	}
	if got := resultText(t, result); got != "from-context\n" {
		t.Fatalf("bash text = %q, want plugin bin command output", got)
	}
}

func TestBashToolTruncatesLongOutput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	command := "i=1; while [ $i -le 2105 ]; do echo line-$i; i=$((i+1)); done"

	result, err := executeTool(ctx, builtin.NewBashTool(env, ""), map[string]any{"command": command}, nil)
	if err != nil {
		t.Fatalf("bash Execute returned Go error: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "line-2105") {
		t.Fatalf("bash long text missing tail: %q", text)
	}
	if !strings.Contains(text, "[Showing lines 106-2105 of 2105. Full output:") {
		t.Fatalf("bash long text = %q, want line-limit truncation notice", text)
	}
}

func TestWriteToolReportsUTF16CodeUnitCount(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	content := "\u00e9\u754c\U0001f642"

	result, err := executeTool(ctx, builtin.NewWriteTool(env), map[string]any{
		"path":    "utf16.txt",
		"content": content,
	}, nil)
	if err != nil {
		t.Fatalf("write Execute returned Go error: %v", err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "Successfully wrote 4 bytes to utf16.txt") {
		t.Fatalf("write text = %q, want UTF-16 code-unit count", text)
	}
	if strings.Contains(text, "Successfully wrote 9 bytes") {
		t.Fatalf("write text = %q, want not byte count", text)
	}

	got, err := env.ReadTextFile(ctx, "utf16.txt")
	if err != nil {
		t.Fatalf("ReadTextFile after write: %v", err)
	}
	if got != content {
		t.Fatalf("written content = %q, want original content", got)
	}
}

func TestRunBashReturnsOutputAndExitCode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())

	result := builtin.RunBash(ctx, env, "printf 'hello\\n'")

	if result.Output != "hello\n" {
		t.Fatalf("RunBash output = %q, want command output", result.Output)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("RunBash exitCode = %v, want 0", result.ExitCode)
	}
	if result.Cancelled || result.Truncated || result.FullOutputPath != "" {
		t.Fatalf("RunBash result = %+v, want ordinary success", result)
	}
}

func TestRunBashReturnsNonZeroExitCode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())

	result := builtin.RunBash(ctx, env, "printf 'failed\\n'; exit 7")

	if result.Output != "failed\n" {
		t.Fatalf("RunBash output = %q, want command output", result.Output)
	}
	if result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("RunBash exitCode = %v, want 7", result.ExitCode)
	}
	if result.Cancelled {
		t.Fatal("RunBash cancelled = true, want false")
	}
}

func TestRunBashTruncatesAndPersistsLongOutput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	command := "i=1; while [ $i -le 2105 ]; do echo line-$i; i=$((i+1)); done"

	result := builtin.RunBash(ctx, env, command)

	if !result.Truncated {
		t.Fatalf("RunBash truncated = false, want true: %+v", result)
	}
	if result.FullOutputPath == "" {
		t.Fatalf("RunBash fullOutputPath is empty: %+v", result)
	}
	if !strings.Contains(result.Output, "line-2105") {
		t.Fatalf("RunBash long output missing tail: %q", result.Output)
	}
	if !strings.Contains(result.Output, "[Showing lines 106-2105 of 2105. Full output:") {
		t.Fatalf("RunBash long output = %q, want truncation notice", result.Output)
	}

	full, err := env.ReadTextFile(ctx, result.FullOutputPath)
	if err != nil {
		t.Fatalf("ReadTextFile full output: %v", err)
	}
	if !strings.Contains(full, "line-1\n") || !strings.Contains(full, "line-2105\n") {
		t.Fatalf("full output file missing expected lines")
	}
}

func TestGrepToolFindsMatches(t *testing.T) {
	requireToolBinary(t, "rg")
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	if err := env.WriteFile(ctx, "a.txt", []byte("alpha\nneedle's here\n")); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}
	if err := env.WriteFile(ctx, "b.txt", []byte("beta\n")); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}

	result, err := executeTool(ctx, builtin.NewGrepTool(env), map[string]any{
		"pattern": "needle's",
		"literal": true,
	}, nil)
	if err != nil {
		t.Fatalf("grep Execute returned Go error: %v", err)
	}
	if got := resultText(t, result); !strings.Contains(got, "a.txt:2: needle's here") {
		t.Fatalf("grep text = %q, want file match", got)
	}
	details := resultDetailsMap(t, result)
	matches, ok := details["matches"].([]any)
	if !ok || len(matches) != 1 {
		t.Fatalf("grep matches details = %#v, want one match", details["matches"])
	}
	match, ok := matches[0].(map[string]any)
	if !ok {
		t.Fatalf("grep match detail = %#v, want object", matches[0])
	}
	if match["path"] != "a.txt" || match["line"] != float64(2) || match["text"] != "needle's here" {
		t.Fatalf("grep match detail = %#v, want a.txt line 2", match)
	}
}

func TestGrepToolUsesRipgrepHiddenAndIgnoreSemantics(t *testing.T) {
	requireToolBinary(t, "rg")
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	// rg (like pi's grep, which does not pass --no-require-git) applies .gitignore
	// only inside a git repository; the .git/HEAD marker makes rg treat this dir
	// as a repo root so the .gitignore fixture takes effect.
	for path, content := range map[string]string{
		".git/HEAD":   "ref: refs/heads/main\n",
		".gitignore":  "ignored.txt\n",
		".hidden.txt": "needle\n",
		"ignored.txt": "needle\n",
	} {
		if err := env.WriteFile(ctx, path, []byte(content)); err != nil {
			t.Fatalf("WriteFile fixture %s: %v", path, err)
		}
	}

	result, err := executeTool(ctx, builtin.NewGrepTool(env), map[string]any{
		"pattern": "needle",
		"literal": true,
	}, nil)
	if err != nil {
		t.Fatalf("grep Execute returned Go error: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, ".hidden.txt:1: needle") {
		t.Fatalf("grep text = %q, want hidden-file match", text)
	}
	if strings.Contains(text, "ignored.txt") {
		t.Fatalf("grep text = %q, want .gitignore-respected output", text)
	}
}

func TestGrepToolFormatsContextLines(t *testing.T) {
	requireToolBinary(t, "rg")
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	if err := env.WriteFile(ctx, "a.txt", []byte("before\nneedle\n after\n")); err != nil {
		t.Fatalf("WriteFile fixture: %v", err)
	}

	result, err := executeTool(ctx, builtin.NewGrepTool(env), map[string]any{
		"pattern": "needle",
		"literal": true,
		"context": 1,
	}, nil)
	if err != nil {
		t.Fatalf("grep Execute returned Go error: %v", err)
	}
	if got := resultLines(t, result); !stringSlicesEqual(got, []string{
		"a.txt-1- before",
		"a.txt:2: needle",
		"a.txt-3-  after",
	}) {
		t.Fatalf("grep context lines = %#v, want formatted context block", got)
	}
}

func TestGrepToolReportsMissingRipgrep(t *testing.T) {
	t.Setenv(config.EnvAgentDir, t.TempDir())
	t.Setenv("ALONG_OFFLINE", "1")

	ctx := context.Background()
	env := commandNotFoundEnv{tool: "rg", root: t.TempDir()}
	result, err := executeTool(ctx, builtin.NewGrepTool(env), map[string]any{
		"pattern": "needle",
	}, nil)
	if err != nil {
		t.Fatalf("grep missing rg returned Go error: %v", err)
	}
	if !isErrorResult(result) {
		t.Fatalf("grep missing rg details = %#v, want isError result", result.Details)
	}
	if !strings.Contains(resultText(t, result), "ripgrep (rg) is not available") {
		t.Fatalf("grep missing rg text = %q, want clear rg error", resultText(t, result))
	}
}

func TestFindToolMatchesGlobPatterns(t *testing.T) {
	requireToolBinary(t, "fd")
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	findTool := builtin.NewFindTool(env)
	for _, fixture := range []string{
		".hidden.go",
		"main.go",
		"sub/readme.md",
		"sub/deep/notes.md",
		"sub/deep/fixture.txt",
		".git/ignored.md",
		"node_modules/ignored.md",
		".cache/ignored.md",
		"sub/node_modules/pkg.txt",
	} {
		if err := env.WriteFile(ctx, fixture, []byte("x")); err != nil {
			t.Fatalf("WriteFile fixture %s: %v", fixture, err)
		}
	}
	if err := env.WriteFile(ctx, ".gitignore", []byte(".git/\nnode_modules/\n.cache/\n")); err != nil {
		t.Fatalf("WriteFile .gitignore fixture: %v", err)
	}

	tests := []struct {
		name    string
		pattern string
		path    string
		want    []string
	}{
		{
			name:    "basename go glob includes hidden",
			pattern: "*.go",
			want:    []string{".hidden.go", "main.go"},
		},
		{
			name:    "nested markdown glob",
			pattern: "**/*.md",
			want:    []string{"sub/deep/notes.md", "sub/readme.md"},
		},
		{
			name:    "subtree glob",
			pattern: "sub/**/*",
			want: []string{
				"sub/deep/",
				"sub/deep/fixture.txt",
				"sub/deep/notes.md",
				"sub/readme.md",
			},
		},
		{
			name:    "relative to search path",
			pattern: "**/*.md",
			path:    "sub",
			want:    []string{"deep/notes.md", "readme.md"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := map[string]any{"pattern": tt.pattern}
			if tt.path != "" {
				args["path"] = tt.path
			}
			result, err := executeTool(ctx, findTool, args, nil)
			if err != nil {
				t.Fatalf("find Execute returned Go error: %v", err)
			}
			if got := resultLines(t, result); !stringSlicesEqual(got, tt.want) {
				t.Fatalf("find lines = %#v, want %#v", got, tt.want)
			}
			if result.Details != nil {
				t.Fatalf("find details = %#v, want nil", result.Details)
			}
		})
	}
}

func TestFindToolRespectsLimitAndReportsDetails(t *testing.T) {
	requireToolBinary(t, "fd")
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := env.WriteFile(ctx, name, []byte("x")); err != nil {
			t.Fatalf("WriteFile fixture %s: %v", name, err)
		}
	}

	result, err := executeTool(ctx, builtin.NewFindTool(env), map[string]any{
		"pattern": "*.txt",
		"limit":   2,
	}, nil)
	if err != nil {
		t.Fatalf("find Execute returned Go error: %v", err)
	}
	got := resultLinesBeforeNotice(t, result)
	if len(got) != 2 {
		t.Fatalf("find limited lines = %#v, want two files", got)
	}
	for _, line := range got {
		if line != "a.txt" && line != "b.txt" && line != "c.txt" {
			t.Fatalf("find limited lines = %#v, want fixture files only", got)
		}
	}
	details := resultDetailsMap(t, result)
	if details["resultLimitReached"] != float64(2) {
		t.Fatalf("find details = %#v, want resultLimitReached 2", details)
	}
	if !strings.Contains(resultText(t, result), "2 results limit reached") {
		t.Fatalf("find text = %q, want result limit notice", resultText(t, result))
	}
}

func TestFindToolFallsBackToFDFind(t *testing.T) {
	t.Setenv(config.EnvAgentDir, t.TempDir())

	ctx := context.Background()
	base := local.New(t.TempDir())
	env := &scriptedSearchToolEnv{
		ExecutionEnv: base,
		searchOutput: filepath.Join(base.Cwd(), "main.go") + "\n",
	}

	result, err := executeTool(ctx, builtin.NewFindTool(env), map[string]any{
		"pattern": "*.go",
	}, nil)
	if err != nil {
		t.Fatalf("find fdfind fallback returned Go error: %v", err)
	}
	if got := resultLines(t, result); !stringSlicesEqual(got, []string{"main.go"}) {
		t.Fatalf("find fdfind fallback lines = %#v, want main.go", got)
	}
	if len(env.commands) != 3 {
		t.Fatalf("find fdfind fallback commands = %#v, want fd lookup, fdfind lookup, execution", env.commands)
	}
	if env.commands[0] != "command -v fd" {
		t.Fatalf("first command = %q, want fd lookup", env.commands[0])
	}
	if env.commands[1] != "command -v fdfind" {
		t.Fatalf("second command = %q, want fdfind lookup", env.commands[1])
	}
	if !strings.Contains(env.commands[2], "/usr/bin/fdfind") {
		t.Fatalf("third command = %q, want resolved fdfind execution", env.commands[2])
	}
}

func TestFindToolReportsMissingFD(t *testing.T) {
	t.Setenv(config.EnvAgentDir, t.TempDir())
	t.Setenv("ALONG_OFFLINE", "1")

	ctx := context.Background()
	env := commandNotFoundEnv{tool: "fd", root: t.TempDir()}
	result, err := executeTool(ctx, builtin.NewFindTool(env), map[string]any{
		"pattern": "*.go",
	}, nil)
	if err != nil {
		t.Fatalf("find missing fd returned Go error: %v", err)
	}
	if !isErrorResult(result) {
		t.Fatalf("find missing fd details = %#v, want isError result", result.Details)
	}
	if !strings.Contains(resultText(t, result), "fd is not available") {
		t.Fatalf("find missing fd text = %q, want clear fd error", resultText(t, result))
	}
}

func TestFindToolMissingPathReturnsErrorResult(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())

	result, err := executeTool(ctx, builtin.NewFindTool(env), map[string]any{
		"pattern": "*.go",
		"path":    "missing",
	}, nil)
	if err != nil {
		t.Fatalf("find missing path returned Go error: %v", err)
	}
	if !isErrorResult(result) {
		t.Fatalf("find missing details = %#v, want isError result", result.Details)
	}
	if !strings.Contains(resultText(t, result), "Path not found:") {
		t.Fatalf("find missing text = %q, want path-not-found error", resultText(t, result))
	}
}

func TestToolArgumentValidationReturnsToolErrorResult(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := local.New(t.TempDir())
	tests := []struct {
		name string
		def  extension.ToolDefinition
		args map[string]any
	}{
		{
			name: "read missing path",
			def:  builtin.NewReadTool(env),
			args: map[string]any{},
		},
		{
			name: "write invalid content",
			def:  builtin.NewWriteTool(env),
			args: map[string]any{"path": "x.txt", "content": 12},
		},
		{
			name: "edit missing edits",
			def:  builtin.NewEditTool(env),
			args: map[string]any{"path": "x.txt"},
		},
		{
			name: "bash invalid command",
			def:  builtin.NewBashTool(env, ""),
			args: map[string]any{"command": 12},
		},
		{
			name: "ls invalid path",
			def:  builtin.NewLsTool(env),
			args: map[string]any{"path": 12},
		},
		{
			name: "grep invalid pattern",
			def:  builtin.NewGrepTool(env),
			args: map[string]any{"pattern": 12},
		},
		{
			name: "find missing pattern",
			def:  builtin.NewFindTool(env),
			args: map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := executeTool(ctx, tt.def, tt.args, nil)
			if err != nil {
				t.Fatalf("Execute returned Go error: %v", err)
			}
			if !isErrorResult(result) {
				t.Fatalf("Details = %#v, want isError result", result.Details)
			}
			if resultText(t, result) == "" {
				t.Fatal("error result text is empty")
			}
		})
	}
}

func executeTool(
	ctx context.Context,
	def extension.ToolDefinition,
	args map[string]any,
	onUpdate tool.UpdateCallback,
) (tool.Result, error) {
	return executeToolWithContext(ctx, def, args, onUpdate, extension.ExtensionContext{})
}

func executeToolWithContext(
	ctx context.Context,
	def extension.ToolDefinition,
	args map[string]any,
	onUpdate tool.UpdateCallback,
	extCtx extension.ExtensionContext,
) (tool.Result, error) {
	if def.PrepareArgs != nil {
		args = def.PrepareArgs(args)
	}
	return def.Execute(ctx, extension.ToolCall{ID: "call-1", Args: args}, onUpdate, extCtx)
}

func resultText(t *testing.T, result tool.Result) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("Content len = %d, want 1", len(result.Content))
	}
	return result.Content[0].Text
}

func resultLines(t *testing.T, result tool.Result) []string {
	t.Helper()
	text := strings.TrimSpace(resultText(t, result))
	if text == "" {
		return []string{}
	}
	return strings.Split(text, "\n")
}

func resultLinesBeforeNotice(t *testing.T, result tool.Result) []string {
	t.Helper()
	text, _, _ := strings.Cut(resultText(t, result), "\n\n[")
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{}
	}
	return strings.Split(text, "\n")
}

func resultDetailsMap(t *testing.T, result tool.Result) map[string]any {
	t.Helper()
	data, err := json.Marshal(result.Details)
	if err != nil {
		t.Fatalf("Marshal details: %v", err)
	}
	details := map[string]any{}
	if err := json.Unmarshal(data, &details); err != nil {
		t.Fatalf("Unmarshal details: %v", err)
	}
	return details
}

func requireToolBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := osexec.LookPath(name); err != nil {
		t.Skipf("%s binary is not available: %v", name, err)
	}
}

type commandNotFoundEnv struct {
	execenv.ExecutionEnv
	tool string
	root string
}

type rapidChunkEnv struct {
	execenv.ExecutionEnv
	chunks []string
}

type scriptedSearchToolEnv struct {
	execenv.ExecutionEnv
	commands     []string
	searchOutput string
}

func (e rapidChunkEnv) Exec(
	_ context.Context,
	_ string,
	opts execenv.ExecOptions,
) (execenv.ExecResult, error) {
	// Mirror the real env: a caller sink owns retention (ExecResult left empty),
	// otherwise output is buffered into ExecResult. OnStdout always streams.
	var stdout strings.Builder
	for _, chunk := range e.chunks {
		if opts.Stdout != nil {
			_, _ = opts.Stdout.Write([]byte(chunk))
		} else {
			stdout.WriteString(chunk)
		}
		if opts.OnStdout != nil {
			opts.OnStdout(chunk)
		}
	}
	return execenv.ExecResult{
		Stdout:   stdout.String(),
		ExitCode: 0,
	}, nil
}

func (e *scriptedSearchToolEnv) Exec(
	_ context.Context,
	command string,
	_ execenv.ExecOptions,
) (execenv.ExecResult, error) {
	e.commands = append(e.commands, command)
	switch {
	case command == "command -v fd":
		return execenv.ExecResult{ExitCode: 1}, nil
	case command == "command -v fdfind":
		return execenv.ExecResult{Stdout: "/usr/bin/fdfind\n", ExitCode: 0}, nil
	case strings.Contains(command, "/usr/bin/fdfind"):
		return execenv.ExecResult{Stdout: e.searchOutput, ExitCode: 0}, nil
	default:
		return execenv.ExecResult{Stderr: "unexpected command: " + command, ExitCode: 2}, nil
	}
}

func (e commandNotFoundEnv) Cwd() string {
	return e.root
}

func (e commandNotFoundEnv) AbsolutePath(_ context.Context, path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(e.root, path), nil
}

func (e commandNotFoundEnv) FileInfo(ctx context.Context, path string) (execenv.FileInfo, error) {
	absolutePath, err := e.AbsolutePath(ctx, path)
	if err != nil {
		return execenv.FileInfo{}, err
	}
	return execenv.FileInfo{
		Name:  filepath.Base(absolutePath),
		Path:  absolutePath,
		IsDir: true,
	}, nil
}

func (e commandNotFoundEnv) Exec(
	context.Context,
	string,
	execenv.ExecOptions,
) (execenv.ExecResult, error) {
	return execenv.ExecResult{
		Stderr:   e.tool + ": command not found",
		ExitCode: 127,
	}, nil
}

func stringSlicesEqual(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isErrorResult(result tool.Result) bool {
	details, ok := result.Details.(map[string]any)
	if !ok {
		return false
	}
	isError, ok := details["isError"].(bool)
	return ok && isError
}
