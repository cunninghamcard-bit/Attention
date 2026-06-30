#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const MAX_OUTPUT_CHARS = 12_000;
const READ_EXACT_OUTPUT_LINE_THRESHOLD = 80;
const READ_COMPACTION_BANNER_PREFIX = "[RTK compacted output:";

function readEvent() {
	try {
		const raw = readFileSync(0, "utf8");
		return raw.trim() ? JSON.parse(raw) : null;
	} catch {
		return null;
	}
}

function writeEnvelope(hookEventName, fields) {
	process.stdout.write(JSON.stringify({ hookSpecificOutput: { hookEventName, ...fields } }));
}

function toRecord(value) {
	return value && typeof value === "object" && !Array.isArray(value) ? value : {};
}

function toolNameOf(event) {
	return typeof event?.tool_name === "string" ? event.tool_name.toLowerCase() : "";
}

const SINGLE_QUOTED_SHELL_VALUE_PATTERN = "'(?:'\\\\''|[^'])*'";
const ENV_ASSIGNMENT_VALUE_PATTERN = `(?:"[^"]*"|${SINGLE_QUOTED_SHELL_VALUE_PATTERN}|[^\\s]+)`;
const LEADING_ENV_ASSIGNMENT_PATTERN = new RegExp(
	`^((?:[A-Za-z_][A-Za-z0-9_]*=${ENV_ASSIGNMENT_VALUE_PATTERN}\\s+)*)`,
);

function splitLeadingEnvAssignments(input) {
	const envPrefix = input.match(LEADING_ENV_ASSIGNMENT_PATTERN)?.[1] ?? "";
	return { envPrefix, command: input.slice(envPrefix.length) };
}

function isAlreadyRtk(command) {
	const effective = splitLeadingEnvAssignments(command.trimStart()).command.trimStart();
	return effective === "rtk" || effective.startsWith("rtk ");
}

function splitTopLevelShellSegments(command) {
	const segments = [];
	const separators = [];
	let quote = null;
	let escaped = false;
	let segmentStart = 0;

	for (let index = 0; index < command.length; index += 1) {
		const character = command[index] ?? "";
		const nextCharacter = command[index + 1] ?? "";
		const previousCharacter = index > 0 ? (command[index - 1] ?? "") : "";

		if (escaped) {
			escaped = false;
			continue;
		}
		if (quote !== null) {
			if (character === "\\" && quote !== "'") escaped = true;
			else if (character === quote) quote = null;
			continue;
		}
		if (character === "\\") {
			escaped = true;
			continue;
		}
		if (character === '"' || character === "'" || character === "`") {
			quote = character;
			continue;
		}

		const two = `${character}${nextCharacter}`;
		const separator =
			two === "&&" || two === "||" || two === "|&"
				? two
				: character === ";" || (character === "|" && previousCharacter !== ">")
					? character
					: null;
		if (separator === null) continue;

		segments.push(command.slice(segmentStart, index));
		separators.push(separator);
		index += separator.length - 1;
		segmentStart = index + 1;
	}

	segments.push(command.slice(segmentStart));
	return { segments, separators };
}

function startsWithRipgrepCommand(segment) {
	const effective = splitLeadingEnvAssignments(segment.trimStart()).command.trimStart();
	return /^rg(?=\s|$)/u.test(effective);
}

function replaceRtkGrepProxyWithRtkRg(segment) {
	const leadingWhitespace = segment.match(/^\s*/u)?.[0] ?? "";
	const withoutLeadingWhitespace = segment.slice(leadingWhitespace.length);
	const { envPrefix, command } = splitLeadingEnvAssignments(withoutLeadingWhitespace);
	const nextCommand = command.replace(/^(rtk)(\s+)grep(?=\s|$)/u, "$1$2rg");
	return nextCommand === command ? segment : `${leadingWhitespace}${envPrefix}${nextCommand}`;
}

function normalizeRipgrepRewrite(originalCommand, rewrittenCommand) {
	const original = splitTopLevelShellSegments(originalCommand);
	const rewritten = splitTopLevelShellSegments(rewrittenCommand);
	if (original.segments.length !== rewritten.segments.length) return rewrittenCommand;

	let changed = false;
	const rewrittenSegments = rewritten.segments.map((segment, index) => {
		if (!startsWithRipgrepCommand(original.segments[index] ?? "")) return segment;
		const nextSegment = replaceRtkGrepProxyWithRtkRg(segment);
		changed ||= nextSegment !== segment;
		return nextSegment;
	});
	if (!changed) return rewrittenCommand;

	return rewrittenSegments.reduce((accumulator, segment, index) => {
		const separator = rewritten.separators[index - 1];
		return separator === undefined ? segment : `${accumulator}${separator}${segment}`;
	}, "");
}

const RTK_DB_PATH_ASSIGNMENT_PATTERN = new RegExp(`(?:^|\\s)RTK_DB_PATH=${ENV_ASSIGNMENT_VALUE_PATTERN}(?=\\s|$)`);
const RTK_DB_PATH_EXPORT_PATTERN = new RegExp(`^export\\s+RTK_DB_PATH=${ENV_ASSIGNMENT_VALUE_PATTERN}(?=\\s*(?:;|$))`);

function quoteForShellEnv(value) {
	const normalized = process.platform === "win32" ? value.replace(/\\/g, "/") : value;
	return `'${normalized.replace(/'/g, `'\\''`)}'`;
}

function hasLeadingRtkDbPathAssignment(command) {
	const trimmed = command.trimStart();
	return RTK_DB_PATH_ASSIGNMENT_PATTERN.test(splitLeadingEnvAssignments(trimmed).envPrefix) ||
		RTK_DB_PATH_EXPORT_PATTERN.test(trimmed);
}

function applyRtkCommandEnvironment(command) {
	if (!command.trim() || hasLeadingRtkDbPathAssignment(command)) return command;
	return `export RTK_DB_PATH=${quoteForShellEnv(join(tmpdir(), "pi-rtk-optimizer", "history.db"))}; ${command}`;
}

const SHELL_ENV_VALUE_PATTERN = `(?:"(?:\\\\.|[^"])*"|${SINGLE_QUOTED_SHELL_VALUE_PATTERN}|[^\\s;]+)`;
const LEADING_RTK_DB_PATH_EXPORT_PRELUDE_PATTERN = new RegExp(
	`^(\\s*export\\s+RTK_DB_PATH=${SHELL_ENV_VALUE_PATTERN}\\s*;\\s*)([\\s\\S]*)$`,
	"u",
);

function splitLeadingRtkDbPathExportPrelude(command) {
	const match = command.match(LEADING_RTK_DB_PATH_EXPORT_PRELUDE_PATTERN);
	return match ? { environmentPrelude: match[1] ?? "", command: match[2] ?? "" } : { environmentPrelude: "", command };
}

function parseSimpleTopLevelPipeline(command) {
	const segments = [];
	const separators = [];
	let quote = null;
	let escaped = false;
	let segmentStart = 0;
	let suffix = "";

	for (let index = 0; index < command.length; index += 1) {
		const character = command[index] ?? "";
		const nextCharacter = command[index + 1] ?? "";
		const previousCharacter = index > 0 ? (command[index - 1] ?? "") : "";

		if (escaped) {
			escaped = false;
			continue;
		}
		if (quote !== null) {
			if (character === "\\" && quote !== "'") escaped = true;
			else if (character === quote) quote = null;
			continue;
		}
		if (character === "\\") {
			escaped = true;
			continue;
		}
		if (character === '"' || character === "'" || character === "`") {
			quote = character;
			continue;
		}
		if (character === "|" && nextCharacter === "|") {
			if (separators.length === 0) return null;
			segments.push(command.slice(segmentStart, index));
			suffix = command.slice(index);
			break;
		}
		if (character === "|" && previousCharacter !== ">") {
			const separatorLength = nextCharacter === "&" ? 2 : 1;
			segments.push(command.slice(segmentStart, index));
			separators.push(command.slice(index, index + separatorLength));
			segmentStart = index + separatorLength;
			if (separatorLength === 2) index += 1;
			continue;
		}
		if (character === "&" && nextCharacter === "&") {
			if (separators.length === 0) return null;
			segments.push(command.slice(segmentStart, index));
			suffix = command.slice(index);
			break;
		}
		if (character === "&" && nextCharacter !== ">" && previousCharacter !== ">" && previousCharacter !== "<") return null;
		if (character === ";") {
			if (separators.length === 0) return null;
			segments.push(command.slice(segmentStart, index));
			suffix = command.slice(index);
			break;
		}
	}

	if (separators.length === 0) return null;
	if (!suffix) segments.push(command.slice(segmentStart));
	return { segments, separators, suffix };
}

function extractProducerRewritePlan(segment, firstSeparator) {
	const { envPrefix, command: commandWithOptionalRedirect } = splitLeadingEnvAssignments(segment.trim());
	if (!/^rtk\s+/i.test(commandWithOptionalRedirect)) return null;
	const stderrMergeMatch = commandWithOptionalRedirect.match(/^(.*?)(?:\s+)?2>\s*&1\s*$/u);
	if (stderrMergeMatch) {
		const command = stderrMergeMatch[1]?.trimEnd() ?? "";
		return command ? { command: `${envPrefix}${command}`.trim(), captureStderr: true } : null;
	}
	return { command: `${envPrefix}${commandWithOptionalRedirect}`.trim(), captureStderr: firstSeparator === "|&" };
}

function buildBufferedPipelineCommand(producer, remainder) {
	const tempFileVariable = "__pi_rtk_pipe_tmp";
	const statusVariable = "__pi_rtk_pipe_status";
	const producerRedirect = producer.captureStderr ? `> "$${tempFileVariable}" 2>&1` : `> "$${tempFileVariable}"`;
	return [
		"{",
		`${tempFileVariable}="$(mktemp)" || exit $?;`,
		`${statusVariable}=0;`,
		`trap 'rm -f "$${tempFileVariable}"' EXIT HUP INT TERM;`,
		`${producer.command} ${producerRedirect};`,
		`${statusVariable}=$?;`,
		`if [ $${statusVariable} -eq 0 ]; then (${remainder}) < "$${tempFileVariable}"; ${statusVariable}=$?; fi;`,
		`exit $${statusVariable};`,
		"}",
	].join(" ");
}

function applyRewrittenCommandShellSafetyFixups(command) {
	if (process.platform !== "win32") return command;
	const target = splitLeadingRtkDbPathExportPrelude(command);
	const parsed = parseSimpleTopLevelPipeline(target.command);
	if (!parsed) return command;
	const producer = extractProducerRewritePlan(parsed.segments[0] ?? "", parsed.separators[0] ?? "");
	if (!producer) return command;
	const remainder = parsed.segments
		.slice(1)
		.map((segment, index) => `${index === 0 ? "" : (parsed.separators[index] ?? "")}${segment}`)
		.join("")
		.trim();
	if (!remainder) return command;
	const suffix = parsed.suffix ? ` ${parsed.suffix.trimStart()}` : "";
	return `${target.environmentPrelude}${buildBufferedPipelineCommand(producer, remainder)}${suffix}`;
}

function runRtkRewrite(command) {
	const rtkBin = process.env.RTK_BIN && process.env.RTK_BIN.trim() ? process.env.RTK_BIN.trim() : "rtk";
	const result = spawnSync(rtkBin, ["rewrite", command], {
		encoding: "utf8",
		timeout: 3000,
		maxBuffer: 1024 * 1024,
	});
	if (result.error || (result.status !== 0 && result.status !== 3)) return null;
	const rewritten = result.stdout.trim();
	if (!rewritten) return null;
	return normalizeRipgrepRewrite(command, rewritten);
}

function handlePreToolUse(event) {
	if (toolNameOf(event) !== "bash") return;
	const input = toRecord(event.tool_input);
	const command = typeof input.command === "string" ? input.command : "";
	if (!command.trim() || isAlreadyRtk(command)) return;

	const rewritten = runRtkRewrite(command);
	if (!rewritten || rewritten === command) return;

	const updatedCommand = applyRewrittenCommandShellSafetyFixups(applyRtkCommandEnvironment(rewritten));
	writeEnvelope("PreToolUse", { updatedInput: { ...input, command: updatedCommand } });
}

function stripAnsi(text) {
	return text
		.replace(/\x1b\[[0-9;]*[a-zA-Z]/g, "")
		.replace(/\x1b\][0-9;]*(?:\x07|\x1b\\)/g, "")
		.replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, "");
}

function stripAnsiFast(text) {
	return text.includes("\x1b") ? stripAnsi(text) : text;
}

const ENV_PREFIX_PATTERN = /^(?:[A-Za-z_][A-Za-z0-9_]*=(?:"[^"]*"|'[^']*'|[^\s]+)\s+)*/;
const CHAIN_OPERATORS = ["&&", "||", ";", "|"];

function sliceFirstSegment(command) {
	let cutIndex = -1;
	for (const operator of CHAIN_OPERATORS) {
		const index = command.indexOf(operator);
		if (index !== -1 && (cutIndex === -1 || index < cutIndex)) cutIndex = index;
	}
	return cutIndex === -1 ? command : command.slice(0, cutIndex);
}

function normalizeCommandForDetection(command) {
	if (typeof command !== "string") return null;
	const firstNonEmptyLine = command
		.split(/\r?\n/)
		.map((line) => line.trim())
		.find((line) => line.length > 0);
	if (!firstNonEmptyLine) return null;
	const withoutEnvPrefix = firstNonEmptyLine.replace(ENV_PREFIX_PATTERN, "").trim();
	return withoutEnvPrefix ? sliceFirstSegment(withoutEnvPrefix).trim().toLowerCase() || null : null;
}

function matchesCommandPatterns(command, patterns) {
	const normalized = normalizeCommandForDetection(command);
	return normalized ? patterns.some((pattern) => pattern.test(normalized)) : false;
}

function normalizeTechniqueResult(result, currentText) {
	return result === null ? currentText : result;
}

function truncate(text, maxLength = MAX_OUTPUT_CHARS) {
	if (text.length <= maxLength) return text;
	if (maxLength < 3) return "...";
	return `${text.slice(0, maxLength - 3)}...`;
}

const RTK_HOOK_WARNING_MESSAGES = [
	"No hook installed \u2014 run `rtk init -g` for automatic token savings",
	"Hook outdated \u2014 run `rtk init -g` to update",
];
const RTK_HOOK_WARNING_PREFIX_MARKERS = ["[rtk] /!\\", "\u26a0", "[WARN]"];

function stripRtkHookWarnings(output) {
	if (!RTK_HOOK_WARNING_MESSAGES.some((message) => output.includes(message))) return null;
	const filteredLines = [];
	let removedWarning = false;
	let skipImmediateBlankLine = false;
	for (const line of output.split("\n")) {
		if (skipImmediateBlankLine && line.trim() === "") {
			skipImmediateBlankLine = false;
			continue;
		}
		let nextLine = line;
		let removedLine = false;
		for (const message of RTK_HOOK_WARNING_MESSAGES) {
			const messageIndex = nextLine.indexOf(message);
			if (messageIndex === -1) continue;
			const prefixIndex = RTK_HOOK_WARNING_PREFIX_MARKERS.reduce(
				(closest, marker) => Math.max(closest, nextLine.lastIndexOf(marker, messageIndex)),
				-1,
			);
			if (nextLine.trim() === message || prefixIndex !== -1) {
				const removalStart = prefixIndex === -1 ? 0 : prefixIndex;
				nextLine = `${nextLine.slice(0, removalStart).trimEnd()}${nextLine.slice(messageIndex + message.length)}`;
				removedWarning = true;
				removedLine = nextLine.trim() === "";
				break;
			}
		}
		if (removedLine) {
			skipImmediateBlankLine = true;
			continue;
		}
		skipImmediateBlankLine = false;
		filteredLines.push(nextLine);
	}
	while (filteredLines.length > 0 && filteredLines[0]?.trim() === "") filteredLines.shift();
	return removedWarning ? filteredLines.join("\n") : null;
}

const RTK_COMMAND_PATTERN = /^\s*rtk(?:\.exe)?(?:\s|$)/;
const RTK_OUTPUT_SIGNATURE_PATTERNS = [
	/^\ud83d\udcc2 PATH Variables:/m,
	/^\ud83d\udd27 Language\/Runtime:/m,
	/^\u2601\ufe0f?\s+Cloud\/Services:/m,
	/^\ud83d\udee0\ufe0f?\s+Tools:/m,
	/^\ud83d\udccb Other:/m,
	/^\ud83d\udcca Total:/m,
	/^\u2705 Files are identical$/m,
	/^\u2705 Staged:/m,
	/^\ud83d\udcdd Modified:/m,
	/^\u2753 Untracked:/m,
	/^\u26a0\ufe0f?\s+Conflicts:/m,
	/^\ud83d\udd0d CI Checks Summary:/m,
];
const INLINE_REPLACEMENTS = [
	{ pattern: /\u2705|\u2713|\u2714/g, replacement: "[OK]" },
	{ pattern: /\u274c|\u2717|\u2715/g, replacement: "[ERROR]" },
	{ pattern: /\u26a0\ufe0f?|\u26a0/g, replacement: "[WARN]" },
	{ pattern: /\u2753/g, replacement: "[INFO]" },
	{ pattern: /\u23ed\ufe0f?|\u23ed/g, replacement: "[SKIP]" },
	{ pattern: /\u23f3/g, replacement: "Pending" },
	{ pattern: /\u2b06\ufe0f?|\u2b06/g, replacement: "up" },
	{ pattern: /\u2192/g, replacement: "->" },
	{ pattern: /\u2022/g, replacement: "-" },
];

function sanitizeRtkEmojiOutput(output, command) {
	if (!(typeof command === "string" && RTK_COMMAND_PATTERN.test(command)) &&
		!RTK_OUTPUT_SIGNATURE_PATTERNS.some((pattern) => pattern.test(output))) {
		return null;
	}
	let nextText = output
		.replace(/^\ud83d\udd0d\s+/gm, "")
		.replace(/^\ud83d\udcc4\s+/gm, "> ")
		.replace(/^[\ud83d\udcc2\ud83d\udd27\ud83d\udccb\ud83d\udcca\ud83d\udcdd]\s+/gm, "")
		.replace(/^\u2601\ufe0f?\s+/gm, "")
		.replace(/^\ud83d\udee0\ufe0f?\s+/gm, "");
	for (const { pattern, replacement } of INLINE_REPLACEMENTS) nextText = nextText.replace(pattern, replacement);
	nextText = nextText.replace(/\p{Extended_Pictographic}/gu, "").replace(/\uFE0F/g, "");
	return nextText === output ? null : nextText;
}

const TEST_COMMAND_PATTERNS = [
	/^npm\s+test\b/,
	/^pnpm\s+test\b/,
	/^yarn\s+test\b/,
	/^bun\s+test\b/,
	/^cargo\s+test\b/,
	/^go\s+test\b/,
	/^pytest\b/,
	/^python\s+-m\s+pytest\b/,
	/^(?:pnpm\s+)?(?:npx\s+)?vitest\b/,
	/^(?:npx\s+)?jest\b/,
	/^mocha\b/,
	/^ava\b/,
	/^tap\b/,
];
const TEST_RESULT_PATTERNS = [
	/test result:\s*(\w+)\.\s*(\d+)\s*passed;\s*(\d+)\s*failed;/,
	/(\d+)\s*passed(?:,\s*(\d+)\s*failed)?(?:,\s*(\d+)\s*skipped)?/i,
	/(\d+)\s*pass(?:,\s*(\d+)\s*fail)?(?:,\s*(\d+)\s*skip)?/i,
	/tests?:\s*(\d+)\s*passed(?:,\s*(\d+)\s*failed)?(?:,\s*(\d+)\s*skipped)?/i,
];
const FAILURE_START_PATTERNS = [/^FAIL\s+/, /^FAILED\s+/, /^\s*\u25cf\s+/, /^\s*\u2715\s+/, /test\s+\w+\s+\.\.\.\s*FAILED/, /thread\s+'\w+'\s+panicked/];

function aggregateTestOutput(output, command) {
	if (!matchesCommandPatterns(command, TEST_COMMAND_PATTERNS)) return null;
	const lines = output.split("\n");
	const summary = { passed: 0, failed: 0, skipped: 0, failures: [] };
	for (const pattern of TEST_RESULT_PATTERNS) {
		const match = output.match(pattern);
		if (!match) continue;
		summary.passed = Number.parseInt(match[1] ?? "0", 10) || 0;
		summary.failed = Number.parseInt(match[2] ?? "0", 10) || 0;
		summary.skipped = Number.parseInt(match[3] ?? "0", 10) || 0;
		break;
	}
	if (summary.passed === 0 && summary.failed === 0) {
		for (const line of lines) {
			if (/\b(ok|PASS|\u2713|\u2714)\b/.test(line)) summary.passed++;
			if (/\b(FAIL|fail|\u2717|\u2715)\b/.test(line)) summary.failed++;
		}
	}
	if (summary.failed > 0) {
		let inFailure = false;
		let currentFailure = [];
		let blankCount = 0;
		for (const line of lines) {
			if (FAILURE_START_PATTERNS.some((pattern) => pattern.test(line))) {
				if (inFailure && currentFailure.length > 0) summary.failures.push(currentFailure.join("\n"));
				inFailure = true;
				currentFailure = [line];
				blankCount = 0;
				continue;
			}
			if (!inFailure) continue;
			if (line.trim() === "") {
				blankCount++;
				if (blankCount >= 2 && currentFailure.length > 3) {
					summary.failures.push(currentFailure.join("\n"));
					inFailure = false;
					currentFailure = [];
				} else currentFailure.push(line);
				continue;
			}
			if (/^\s|^-/.test(line)) {
				currentFailure.push(line);
				blankCount = 0;
				continue;
			}
			summary.failures.push(currentFailure.join("\n"));
			inFailure = false;
			currentFailure = [];
		}
		if (inFailure && currentFailure.length > 0) summary.failures.push(currentFailure.join("\n"));
	}
	const result = ["Test Results:", `   PASS: ${summary.passed} passed`];
	if (summary.failed > 0) result.push(`   FAIL: ${summary.failed} failed`);
	if (summary.skipped > 0) result.push(`   SKIP: ${summary.skipped} skipped`);
	if (summary.failed > 0 && summary.failures.length > 0) {
		result.push("\n   Failures:");
		for (const failure of summary.failures.slice(0, 5)) {
			const failureLines = failure.split("\n");
			const firstLine = failureLines[0] ?? "";
			result.push(`   - ${firstLine.slice(0, 70)}${firstLine.length > 70 ? "..." : ""}`);
			for (const detailLine of failureLines.slice(1, 4)) {
				if (detailLine.trim()) result.push(`     ${detailLine.slice(0, 65)}${detailLine.length > 65 ? "..." : ""}`);
			}
			if (failureLines.length > 4) result.push(`     ... (${failureLines.length - 4} more lines)`);
		}
		if (summary.failures.length > 5) result.push(`   ... and ${summary.failures.length - 5} more failures`);
	}
	return result.join("\n");
}

const BUILD_COMMAND_PATTERNS = [
	/^cargo\s+(build|check)\b/,
	/^bun\s+build\b/,
	/^npm\s+run\s+build\b/,
	/^yarn\s+build\b/,
	/^pnpm\s+build\b/,
	/^(?:npx\s+)?tsc\b/,
	/^make\b/,
	/^cmake\b/,
	/^gradle\b/,
	/^mvn\b/,
	/^go\s+(build|install)\b/,
	/^python\s+setup\.py\s+build\b/,
	/^pip\s+install\b/,
];
const BUILD_SKIP_PATTERNS = [/^\s*Compiling\s+/, /^\s*Checking\s+/, /^\s*Downloading\s+/, /^\s*Downloaded\s+/, /^\s*Fetching\s+/, /^\s*Fetched\s+/, /^\s*Updating\s+/, /^\s*Updated\s+/, /^\s*Building\s+/, /^\s*Generated\s+/, /^\s*Creating\s+/, /^\s*Running\s+/];
const ERROR_START_PATTERNS = [/^error\[/, /^error:/, /^\[ERROR\]/, /^FAIL/];
const WARNING_PATTERNS = [/^warning:/, /^\[WARNING\]/, /^warn:/];

function filterBuildOutput(output, command) {
	if (!matchesCommandPatterns(command, BUILD_COMMAND_PATTERNS)) return null;
	const lines = output.split("\n");
	const stats = { compiled: 0, errors: [], warnings: [] };
	let inErrorBlock = false;
	let currentError = [];
	let blankCount = 0;
	for (const line of lines) {
		if (/^\s*(Compiling|Checking|Building)\s+/.test(line)) {
			stats.compiled++;
			continue;
		}
		if (BUILD_SKIP_PATTERNS.some((pattern) => pattern.test(line))) continue;
		if (ERROR_START_PATTERNS.some((pattern) => pattern.test(line))) {
			if (inErrorBlock && currentError.length > 0) stats.errors.push([...currentError]);
			inErrorBlock = true;
			currentError = [line];
			blankCount = 0;
			continue;
		}
		if (WARNING_PATTERNS.some((pattern) => pattern.test(line))) {
			stats.warnings.push(line);
			continue;
		}
		if (!inErrorBlock) continue;
		if (line.trim() === "") {
			blankCount++;
			if (blankCount >= 2 && currentError.length > 3) {
				stats.errors.push([...currentError]);
				inErrorBlock = false;
				currentError = [];
			} else currentError.push(line);
			continue;
		}
		if (/^\s|^-->/.test(line)) {
			currentError.push(line);
			blankCount = 0;
			continue;
		}
		stats.errors.push([...currentError]);
		inErrorBlock = false;
		currentError = [];
	}
	if (inErrorBlock && currentError.length > 0) stats.errors.push(currentError);
	if (stats.errors.length === 0 && stats.warnings.length === 0) return `[OK] Build successful (${stats.compiled} units compiled)`;
	const result = [];
	if (stats.errors.length > 0) {
		result.push(`[ERROR] ${stats.errors.length} error(s):`);
		for (const error of stats.errors.slice(0, 5)) {
			result.push(...error.slice(0, 10));
			if (error.length > 10) result.push("  ...");
		}
		if (stats.errors.length > 5) result.push(`... and ${stats.errors.length - 5} more errors`);
	}
	if (stats.warnings.length > 0) result.push(`\n[WARN] ${stats.warnings.length} warning(s)`);
	return result.join("\n");
}

function compactPath(path, maxLength) {
	if (path.length <= maxLength) return path;
	if (maxLength < 2) return path.slice(0, maxLength);
	const separator = path.includes("\\") && !path.includes("/") ? "\\" : "/";
	const prefix = /^[A-Za-z]:[\\/]/.test(path) ? `${path.slice(0, 2)}${separator}` : path.startsWith("/") || path.startsWith("\\") ? separator : "";
	const segments = path.slice(prefix.length).split(/[\\/]+/).filter(Boolean);
	const lastSegment = segments[segments.length - 1] ?? path.slice(-(maxLength - 1));
	const previousSegment = segments[segments.length - 2];
	const candidates = [
		[prefix, "...", previousSegment, lastSegment].filter(Boolean).join(separator),
		["...", previousSegment, lastSegment].filter(Boolean).join(separator),
		["...", lastSegment].join(separator),
		`...${path.slice(-(maxLength - 3))}`,
	];
	return candidates.find((candidate) => candidate.length <= maxLength) ?? `...${lastSegment.slice(-(maxLength - 3))}`;
}

const LINTER_COMMAND_PATTERNS = [
	/^(?:pnpm\s+)?(?:npx\s+)?eslint\b/,
	/^(?:npx\s+)?prettier\b/,
	/^ruff\b/,
	/^pylint\b/,
	/^mypy\b/,
	/^flake8\b/,
	/^black\b/,
	/^cargo\s+clippy\b/,
	/^golangci-lint\b/,
];

function aggregateLinterOutput(output, command) {
	if (!matchesCommandPatterns(command, LINTER_COMMAND_PATTERNS)) return null;
	const issues = [];
	for (const line of output.split("\n")) {
		const fileLineMatch = line.match(/^(.+):(\d+):(\d+):\s*(.+)$/);
		if (!fileLineMatch) continue;
		const content = fileLineMatch[4] ?? line;
		issues.push({
			severity: /warning/i.test(content) ? "WARNING" : "ERROR",
			rule: content.match(/\[(.+?)\]$/)?.[1] ?? "unknown",
			file: fileLineMatch[1] ?? "unknown",
			message: content,
		});
	}
	const normalized = normalizeCommandForDetection(command) ?? "";
	const linterType = /eslint\b/.test(normalized) ? "ESLint" : /^ruff\b/.test(normalized) ? "Ruff" : /prettier\b/.test(normalized) ? "Prettier" : "Linter";
	if (issues.length === 0) return `[OK] ${linterType}: No issues found`;
	const errors = issues.filter((issue) => issue.severity === "ERROR").length;
	const warnings = issues.length - errors;
	const byRule = new Map();
	const byFile = new Map();
	for (const issue of issues) {
		byRule.set(issue.rule, (byRule.get(issue.rule) ?? 0) + 1);
		const fileIssues = byFile.get(issue.file) ?? [];
		fileIssues.push(issue);
		byFile.set(issue.file, fileIssues);
	}
	let result = `${linterType}: ${errors} errors, ${warnings} warnings in ${byFile.size} files\n`;
	result += "---------------------------------------\nTop rules:\n";
	for (const [rule, count] of [...byRule.entries()].sort((left, right) => right[1] - left[1]).slice(0, 10)) {
		result += `  ${rule} (${count}x)\n`;
	}
	result += "\nTop files:\n";
	for (const [file, fileIssues] of [...byFile.entries()].sort((left, right) => right[1].length - left[1].length).slice(0, 10)) {
		result += `  ${compactPath(file, 40)} (${fileIssues.length} issues)\n`;
	}
	return result.trimEnd();
}

const GIT_COMMAND_PATTERNS = [/^git\s+(diff|status|log|show|stash)\b/];
const RAW_GIT_DIFF_PATTERN = /^diff --git /m;
const RAW_GIT_STATUS_PATTERN = /^(?:## |(?:M|A|D|R|C|U|\?| )\S)/m;

function compactDiff(output, maxLines = 50) {
	const result = [];
	let currentFile = "";
	let added = 0;
	let removed = 0;
	let inHunk = false;
	let hunkLines = 0;
	for (const line of output.split("\n")) {
		if (result.length >= maxLines) {
			result.push("\n... (more changes truncated)");
			break;
		}
		if (line.startsWith("diff --git")) {
			if (currentFile && (added > 0 || removed > 0)) result.push(`  +${added} -${removed}`);
			currentFile = line.match(/diff --git a\/(.+) b\/(.+)/)?.[2] ?? "unknown";
			result.push(`\n> ${currentFile}`);
			added = 0;
			removed = 0;
			inHunk = false;
			continue;
		}
		if (line.startsWith("@@")) {
			inHunk = true;
			hunkLines = 0;
			result.push(`  ${line.match(/@@ .+ @@/)?.[0] ?? "@@"}`);
			continue;
		}
		if (!inHunk) continue;
		if (line.startsWith("+") && !line.startsWith("+++")) {
			added++;
			if (hunkLines < 10) result.push(`  ${line}`);
			hunkLines++;
		} else if (line.startsWith("-") && !line.startsWith("---")) {
			removed++;
			if (hunkLines < 10) result.push(`  ${line}`);
			hunkLines++;
		} else if (hunkLines > 0 && hunkLines < 10 && !line.startsWith("\\")) {
			result.push(`  ${line}`);
			hunkLines++;
		}
		if (hunkLines === 10) {
			result.push("  ... (truncated)");
			hunkLines++;
		}
	}
	if (currentFile && (added > 0 || removed > 0)) result.push(`  +${added} -${removed}`);
	return result.join("\n");
}

function compactStatus(output) {
	const stats = { staged: [], modified: [], untracked: [], conflicts: 0 };
	let branchName = "";
	for (const line of output.split("\n")) {
		if (line.startsWith("##")) {
			branchName = line.match(/## (.+)/)?.[1]?.split("...")[0] ?? "";
			continue;
		}
		if (line.length < 3) continue;
		const status = line.slice(0, 2);
		const filename = line.slice(3);
		if (["M", "A", "D", "R", "C"].includes(status[0])) stats.staged.push(filename);
		if (status[0] === "U") stats.conflicts++;
		if (["M", "D"].includes(status[1])) stats.modified.push(filename);
		if (status === "??") stats.untracked.push(filename);
	}
	const result = [`Branch: ${branchName}`];
	for (const [label, files, limit] of [
		["Staged", stats.staged, 5],
		["Modified", stats.modified, 5],
		["Untracked", stats.untracked, 3],
	]) {
		if (files.length === 0) continue;
		result.push(`${label}: ${files.length} files`, ...files.slice(0, limit).map((file) => `  ${file}`));
		if (files.length > limit) result.push(`  ... +${files.length - limit} more`);
	}
	if (stats.conflicts > 0) result.push(`Conflicts: ${stats.conflicts} files`);
	return result.join("\n").trim();
}

function compactLog(output, limit = 20) {
	const lines = output.split("\n");
	const result = lines.slice(0, limit).map((line) => line.length > 80 ? `${line.slice(0, 77)}...` : line);
	if (lines.length > limit) result.push(`... and ${lines.length - limit} more commits`);
	return result.join("\n");
}

function compactGitOutput(output, command) {
	if (!matchesCommandPatterns(command, GIT_COMMAND_PATTERNS)) return null;
	const normalized = normalizeCommandForDetection(command);
	if (!normalized) return null;
	if (normalized.startsWith("git diff")) return RAW_GIT_DIFF_PATTERN.test(output) ? compactDiff(output) : null;
	if (normalized.startsWith("git status")) return RAW_GIT_STATUS_PATTERN.test(output) ? compactStatus(output) : null;
	if (normalized.startsWith("git log")) return compactLog(output);
	return null;
}

function groupSearchResults(output, maxResults = 50) {
	const results = [];
	for (const line of output.split("\n")) {
		if (!line.trim()) continue;
		const match = line.match(/^(.+?):(\d+)?:(.+)$/);
		if (!match) continue;
		results.push({ file: match[1] ?? "unknown", lineNumber: match[2] ?? "?", content: match[3] ?? "" });
	}
	if (results.length === 0) return null;
	const byFile = new Map();
	for (const result of results) {
		const existing = byFile.get(result.file) ?? [];
		existing.push(result);
		byFile.set(result.file, existing);
	}
	let outputText = `${results.length} matches in ${byFile.size} files:\n\n`;
	let shown = 0;
	for (const [file, matches] of [...byFile.entries()].sort((left, right) => left[0].localeCompare(right[0]))) {
		if (shown >= maxResults) break;
		outputText += `> ${compactPath(file, 50)} (${matches.length} matches):\n`;
		for (const match of matches.slice(0, 10)) {
			let cleaned = match.content.trim();
			if (cleaned.length > 70) cleaned = `${cleaned.slice(0, 67)}...`;
			outputText += `    ${match.lineNumber}: ${cleaned}\n`;
			shown++;
		}
		if (matches.length > 10) outputText += `  +${matches.length - 10} more\n`;
		outputText += "\n";
	}
	if (results.length > shown) outputText += `... +${results.length - shown} more\n`;
	return outputText;
}

function countLines(text) {
	if (!text) return 0;
	const normalized = text.endsWith("\n") ? text.slice(0, -1) : text;
	return normalized ? normalized.split("\n").length : 1;
}

function compactBashText(text, command) {
	let nextText = text;
	const techniques = [];
	const stripped = stripAnsiFast(nextText);
	if (stripped !== nextText) {
		nextText = stripped;
		techniques.push("ansi");
	}
	for (const [name, transform] of [
		["rtk-hook-warning", (value) => stripRtkHookWarnings(value)],
		["rtk-emoji", (value) => sanitizeRtkEmojiOutput(value, command)],
		["build", (value) => filterBuildOutput(value, command)],
		["test", (value) => aggregateTestOutput(value, command)],
		["git", (value) => compactGitOutput(value, command)],
		["linter", (value) => aggregateLinterOutput(value, command)],
	]) {
		const compacted = normalizeTechniqueResult(transform(nextText), nextText);
		if (compacted !== nextText) {
			nextText = compacted;
			techniques.push(name);
		}
	}
	if (nextText.length > MAX_OUTPUT_CHARS) {
		nextText = truncate(nextText);
		techniques.push("truncate");
	}
	return { text: nextText, techniques };
}

function compactReadText(text) {
	if (countLines(text) <= READ_EXACT_OUTPUT_LINE_THRESHOLD && text.length <= MAX_OUTPUT_CHARS) return { text, techniques: [] };
	let nextText = stripAnsiFast(text);
	const techniques = nextText === text ? [] : ["ansi"];
	if (nextText.length > MAX_OUTPUT_CHARS) {
		nextText = truncate(nextText);
		techniques.push("truncate");
	}
	if (techniques.length > 0 && !nextText.startsWith(READ_COMPACTION_BANNER_PREFIX)) {
		nextText = `${READ_COMPACTION_BANNER_PREFIX} ${techniques.join(", ")}]\n${nextText}`;
	}
	return { text: nextText, techniques };
}

function compactSearchText(text) {
	let nextText = stripAnsiFast(text);
	const techniques = nextText === text ? [] : ["ansi"];
	const grouped = normalizeTechniqueResult(groupSearchResults(nextText), nextText);
	if (grouped !== nextText) {
		nextText = grouped;
		techniques.push("search");
	}
	if (nextText.length > MAX_OUTPUT_CHARS) {
		nextText = truncate(nextText);
		techniques.push("truncate");
	}
	return { text: nextText, techniques };
}

function compactGenericText(text) {
	let nextText = stripAnsiFast(text);
	const techniques = nextText === text ? [] : ["ansi"];
	if (nextText.length > MAX_OUTPUT_CHARS) {
		nextText = truncate(nextText);
		techniques.push("truncate");
	}
	return { text: nextText, techniques };
}

function compactTextForTool(text, toolName, input) {
	const command = typeof input.command === "string" ? input.command : undefined;
	if (toolName === "bash") return compactBashText(text, command);
	if (toolName === "read") return compactReadText(text);
	if (toolName === "grep") return compactSearchText(text);
	return compactGenericText(text);
}

function transformTextBlocks(blocks, toolName, input) {
	let changed = false;
	const nextBlocks = blocks.map((block) => {
		if (!block || typeof block !== "object" || Array.isArray(block) || block.type !== "text" || typeof block.text !== "string") {
			return block;
		}
		const transformed = compactTextForTool(block.text, toolName, input);
		if (transformed.text === block.text) return block;
		changed = true;
		return { ...block, text: transformed.text };
	});
	return { changed, value: nextBlocks };
}

function transformString(text, toolName, input) {
	const transformed = compactTextForTool(text, toolName, input);
	return { changed: transformed.text !== text, value: transformed.text };
}

function transformToolResponse(response, toolName, input) {
	if (typeof response === "string") return transformString(response, toolName, input);
	if (Array.isArray(response)) return transformTextBlocks(response, toolName, input);
	if (!response || typeof response !== "object") return { changed: false, value: response };

	let changed = false;
	const next = { ...response };
	if (Array.isArray(response.content)) {
		const transformed = transformTextBlocks(response.content, toolName, input);
		if (transformed.changed) {
			next.content = transformed.value;
			changed = true;
		}
	}
	if (typeof response.text === "string") {
		const transformed = transformString(response.text, toolName, input);
		if (transformed.changed) {
			next.text = transformed.value;
			changed = true;
		}
	}
	if (toolName === "bash") {
		for (const key of ["stdout", "stderr"]) {
			if (typeof response[key] !== "string") continue;
			const transformed = transformString(response[key], toolName, input);
			if (transformed.changed) {
				next[key] = transformed.value;
				changed = true;
			}
		}
	}
	return { changed, value: next };
}

function handlePostToolUse(event) {
	const toolName = toolNameOf(event);
	if (!["bash", "read", "grep", "glob"].includes(toolName)) return;
	const response = Object.prototype.hasOwnProperty.call(event, "tool_response") ? event.tool_response : event.tool_output;
	const transformed = transformToolResponse(response, toolName, toRecord(event.tool_input));
	if (!transformed.changed) return;
	writeEnvelope("PostToolUse", { updatedToolOutput: transformed.value });
}

const event = readEvent();
if (event?.hook_event_name === "PreToolUse") {
	handlePreToolUse(event);
} else if (event?.hook_event_name === "PostToolUse") {
	handlePostToolUse(event);
}
