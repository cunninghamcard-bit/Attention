// Package exporthtml renders an along session transcript to pi's
// self-contained interactive HTML viewer.
//
// Pi defines the export contract at
// .agents/references/pi/packages/coding-agent/src/core/agent-session.ts:2973
// (exportToHtml(outputPath?) returns the exported path) and routes RPC
// export_html through it at
// .agents/references/pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts:568-571.
//
// This package mirrors pi's browser-template export path:
// .agents/references/pi/packages/coding-agent/src/core/export-html/index.ts:143-174.
// It embeds template.html/css/js plus vendored marked/highlight assets, then
// injects a base64-encoded SESSION_DATA JSON blob for template.js to render.
package exporthtml
