// Package host 是 JS host 的引擎侧：spawn node 子进程跑 host.js（bootstrap），
// stdio JSON-line 帧通信，崩溃退避重启 + activate 重放（plugin-system §10）。
//
// 设计纪律：
//   - 每帧一行 JSON，双向同一 Frame 结构（T 判别）；请求/应答帧带 CorrID。
//   - host→engine 的 registered 帧是该 activate 的注册全集——引擎按插件
//     原子提交，不存在半挂载状态（spec §4）。
//   - 注册必须幂等：host 重启后引擎按 engine→sessions→environments 顺序
//     原样重放 activate，收敛到同一注册集。
//   - host 死 = 其工具快速失败 errorClass "ext_host_down"，不打挂 run。
package host

import "encoding/json"

// FrameType 是帧判别词汇。增量纪律：只许加，不许改义。
type FrameType string

const (
	// 引擎 → host
	FrameActivate        FrameType = "activate"         // 挂载一个插件模块（按 owner 实例化）
	FrameDispose         FrameType = "dispose"          // 卸载（禁用/会话卸载/环境销毁）
	FrameToolExecute     FrameType = "tool.execute"     // 执行 host 侧工具（T7 接线）
	FrameHookDispatch    FrameType = "hook.dispatch"    // 决策挂点往返（T8 接线）
	FrameCommandDispatch FrameType = "command.dispatch" // ext.command 路由（T6 接线）
	FrameUIResolved      FrameType = "ui.resolved"      // 唤醒挂起的 ui promise（T9 接线）
	FramePing            FrameType = "ping"             // 心跳

	// host → 引擎
	FrameRegistered  FrameType = "registered"   // activate 的注册全集（原子提交）
	FrameDisposed    FrameType = "disposed"     // dispose 完成
	FrameEventsEmit  FrameType = "events.emit"  // 插件事实 → ext.<id>.<name> 信封
	FrameUIRequest   FrameType = "ui.request"   // 插件问人（T9 接线）
	FrameToolResult  FrameType = "tool.result"  // tool.execute 应答
	FrameHookResult  FrameType = "hook.result"  // hook.dispatch 应答
	FrameCommandDone FrameType = "command.done" // command.dispatch 应答
	FrameLog         FrameType = "log"          // 插件日志透传（level+msg）
	FramePong        FrameType = "pong"
	FrameFatal       FrameType = "fatal" // host 自报不可恢复错误后退出
)

// Frame 是线上唯一结构。T 判别；CorrID 关联请求/应答（请求方铸造）；
// 其余字段按 T 取用，无关字段为空。
type Frame struct {
	T      FrameType `json:"t"`
	CorrID string    `json:"corrId,omitempty"`

	// activate / dispose / 路由定位
	PluginID  string `json:"pluginId,omitempty"`
	Owner     string `json:"owner,omitempty"` // session | engine | environment
	SessionID string `json:"sessionId,omitempty"`
	EnvID     string `json:"envId,omitempty"`

	// activate
	ModulePath string          `json:"modulePath,omitempty"` // 插件产物绝对路径
	CtxSeed    json.RawMessage `json:"ctxSeed,omitempty"`    // owner 作用域种子（会话元信息等）

	// registered（原子提交全集）
	Tools    []ToolReg    `json:"tools,omitempty"`
	Hooks    []HookReg    `json:"hooks,omitempty"`
	Commands []CommandReg `json:"commands,omitempty"`

	// tool.execute / tool.result / hook.dispatch / hook.result /
	// command.dispatch / command.done / events.emit / ui.*
	Name    string          `json:"name,omitempty"`    // 工具名/事件名/命令名/挂点名
	Payload json.RawMessage `json:"payload,omitempty"` // 入参/结果/事件载荷（形状随 T）
	IsError bool            `json:"isError,omitempty"`
	Error   string          `json:"error,omitempty"` // 应答错误（处理器抛错文本）

	// log
	Level string `json:"level,omitempty"` // debug | info | warn | error
	Msg   string `json:"msg,omitempty"`
}

// ToolReg 是 host 侧工具的注册申报（执行经 tool.execute 往返）。
type ToolReg struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"` // JSON Schema（入参）
}

// HookReg 申报一个决策挂点 handler。Index 是同一插件实例、同一挂点内的
// 注册序号；host 只按 index 执行单个 handler，折叠语义留在引擎侧。
type HookReg struct {
	Point string `json:"point"` // input | tool_call | tool_result | …（总纲 §6 生死表）
	Index int    `json:"index"`
}

// CommandReg 申报一个 ext.command 服务端处理器（c2）。
type CommandReg struct {
	Name string `json:"name"`
}

// UIRequest is the host→engine frame payload after Manager has copied the
// mutable JSON bytes out of the read loop.
type UIRequest struct {
	CorrID    string
	PluginID  string
	Owner     string
	SessionID string
	EnvID     string
	Kind      string
	Payload   []byte
}
