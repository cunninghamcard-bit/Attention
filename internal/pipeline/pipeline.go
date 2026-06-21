// Package pipeline 是 run 引擎：中间件纯函数，run 间不持有任何状态（§7 三层纪律）。
package pipeline

import "context"

type RunHandler func(ctx context.Context, tc *RunContext) error

type RunMiddleware func(ctx context.Context, tc *RunContext, next RunHandler) error

// Build 自外向内合成。基建依赖（存储/emitter/路由）在各中间件构造时闭包注入，
// 永不进 RunContext（Arkloop RunContext 之戒，§7）。
func Build(final RunHandler, mws ...RunMiddleware) RunHandler {
	h := final
	for i := len(mws) - 1; i >= 0; i-- {
		mw, next := mws[i], h
		h = func(ctx context.Context, tc *RunContext) error { return mw(ctx, tc, next) }
	}
	return h
}
