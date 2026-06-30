import assert from "node:assert/strict";
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { spawnSync } from "node:child_process";

const pluginRoot = resolve(import.meta.dirname, "..");
const hookPath = join(pluginRoot, "dist", "rtk-optimizer-hook.mjs");

function runHook(input, env = {}) {
	const result = spawnSync(process.execPath, [hookPath], {
		input: `${JSON.stringify(input)}\n`,
		encoding: "utf8",
		env: { ...process.env, ...env },
	});
	assert.equal(result.status, 0, result.stderr);
	return result.stdout.trim() ? JSON.parse(result.stdout) : null;
}

const tmp = await mkdtemp(join(tmpdir(), "rtk-optimizer-test-"));
try {
	await writeFile(
		join(tmp, "rtk"),
		"#!/bin/sh\n[ \"$1\" = rewrite ] && [ \"$2\" = 'npm test' ] && { printf 'rtk npm test\\n'; exit 0; }\nexit 1\n",
	);
	await chmod(join(tmp, "rtk"), 0o755);

	const pre = runHook(
		{
			hook_event_name: "PreToolUse",
			tool_name: "Bash",
			tool_input: { command: "npm test", description: "Run tests" },
		},
		{ PATH: `${tmp}:${process.env.PATH ?? ""}` },
	);
	const updated = pre?.hookSpecificOutput?.updatedInput;
	assert.equal(pre.hookSpecificOutput.hookEventName, "PreToolUse");
	assert.equal(updated.description, "Run tests");
	assert.match(updated.command, /^export RTK_DB_PATH='/);
	assert.match(updated.command, /;\s*rtk npm test$/);

	const post = runHook({
		hook_event_name: "PostToolUse",
		tool_name: "Bash",
		tool_input: { command: "npm test" },
		tool_response: [
			{
				type: "text",
				text: [
					"PASS src/a.test.ts",
					"PASS src/b.test.ts",
					"PASS src/c.test.ts",
					"Tests: 3 passed, 3 total",
				].join("\n"),
			},
			{ type: "image", source: { type: "base64", media_type: "image/png", data: "abc" } },
		],
	});
	assert.equal(post.hookSpecificOutput.hookEventName, "PostToolUse");
	assert.match(post.hookSpecificOutput.updatedToolOutput[0].text, /Test Results:/);
	assert.deepEqual(post.hookSpecificOutput.updatedToolOutput[1], {
		type: "image",
		source: { type: "base64", media_type: "image/png", data: "abc" },
	});

	assert.equal(
		runHook({
			hook_event_name: "PreToolUse",
			tool_name: "Bash",
			tool_input: { command: "rtk npm test" },
		}),
		null,
	);
} finally {
	await rm(tmp, { recursive: true, force: true });
}
