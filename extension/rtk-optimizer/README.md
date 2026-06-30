# RTK Optimizer

Claude Code plugin port of the no-UI parts of
[MasuRii/pi-rtk-optimizer](https://github.com/MasuRii/pi-rtk-optimizer).

It adds two hook behaviors:

- `PreToolUse` for `Bash`: delegates command rewriting to `rtk rewrite`.
- `PostToolUse` for `Bash`, `Read`, `Grep`, and `Glob`: compacts text output while preserving non-text blocks.

Set `RTK_BIN` to override the `rtk` executable. If `rtk` is missing or returns
no rewrite, the hook prints nothing so Claude Code keeps the original input.
