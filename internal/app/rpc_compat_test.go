package app

// MVP 判据 4 的机器证明：pi RPC wire 协议在新引擎上逐字节工作。
// 管道驱动 rpc.ServeIO(facade)：发 pi 命令帧 → 断言 pi 事件帧序列与响应帧。

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	rpcmode "github.com/cunninghamcard-bit/Attention/internal/mode/rpc"
	"github.com/cunninghamcard-bit/Attention/internal/session"
)

func TestComposePiRPCCompat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cwd := t.TempDir()
	comp, err := Compose(ctx, ComposeOptions{
		DataDir:    t.TempDir(),
		CWD:        cwd,
		Model:      ai.Model{ID: "test-model", Provider: "test-provider", ContextWindow: 128_000},
		FakeStream: helloWorldStream(),
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer comp.Stop()

	sess, err := comp.Repo.Create(ctx, session.JsonlSessionCreateOptions{CWD: cwd})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	facade := comp.NewSessionFacade(sess.GetMetadata().ID)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	serveDone := make(chan error, 1)
	go func() { serveDone <- rpcmode.ServeIO(ctx, facade, stdinR, stdoutW) }()

	if _, err := io.WriteString(stdinW, `{"id":"p1","type":"prompt","message":"hi"}`+"\n"); err != nil {
		t.Fatalf("write command: %v", err)
	}

	// 收线：响应帧 + 事件帧混流，读到 agent_end 为止。
	type frame struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Command string `json:"command"`
		Success bool   `json:"success"`
	}
	var (
		gotResponse bool
		eventSeq    []string
	)
	scanner := bufio.NewScanner(stdoutR)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lines := make(chan string, 64)
	go func() {
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

collect:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stdout closed early; events=%v", eventSeq)
			}
			var f frame
			if err := json.Unmarshal([]byte(line), &f); err != nil {
				t.Fatalf("bad wire line %q: %v", line, err)
			}
			if f.Type == "response" {
				if f.ID != "p1" || f.Command != "prompt" || !f.Success {
					t.Fatalf("prompt response: %s", line)
				}
				gotResponse = true
				continue
			}
			eventSeq = append(eventSeq, f.Type)
			if f.Type == "agent_end" {
				break collect
			}
		case <-ctx.Done():
			t.Fatalf("timeout; events=%v response=%v", eventSeq, gotResponse)
		}
	}

	if !gotResponse {
		t.Fatal("no prompt response frame (pi rpc ack semantics)")
	}
	// pi wire 事件序列（黄金路径，fake 流两 delta）：
	assertSubsequence(t, eventSeq, []string{
		"agent_start", "turn_start",
		"message_start", "message_end", // user
		"message_start", "message_update", "message_end", // assistant（≥1 update）
		"turn_end", "agent_end",
	})

	_ = stdinW.Close() // 协议口关流 = 会话收尾
	select {
	case <-serveDone:
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not exit after stdin close")
	}
}

// assertSubsequence 断言 want 是 got 的有序子序列（got 可有多余 update 帧）。
func assertSubsequence(t *testing.T, got, want []string) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("wire sequence mismatch:\n got=%v\nwant subsequence=%v (matched %d)", got, want, i)
	}
}
