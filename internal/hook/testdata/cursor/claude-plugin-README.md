# Cross-loaded Claude plugin hooks under cursor-agent

Captured on 2026-11-23 from cursor-agent 2026.11.22-e9042b2.

`timeout 300 cursor-agent -p "say hi" --trust --force --sandbox disabled --approve-mcps --model auto` ran in a throwaway git repository. `HOME` was a scratch directory. That home held a copy of the Iris Claude plugin whose `hooks/hooks.json` command was a recorder, and a copy of the Cursor CLI config so the agent could start. The recorder appended stdin to a file, wrote the environment names and not their values, then exec'd `iris hook` with the same arguments. The copies under the scratch home were removed after the run. The real `~/.claude` and `~/.cursor` files were not modified.

The commands were `iris hook --client claude --event SessionStart` and `--event SessionEnd`. Stdin is Cursor's hook payload. `hook_event_name` is `sessionStart` or `sessionEnd`. There is no `cwd`. `workspace_roots` holds the project.

## Anonymisation

- `conversation_id`, `generation_id` and `session_id` were one UUID. They are `00000000-0000-4000-8000-000000000001`, including that id inside `transcript_path`.
- `user_email` is `<REDACTED_EMAIL>`.
- The capturing user's home prefix is `/home/user`. The scratch project is `/home/user/tmp/cursor-cap/proj`.
- JSON is reformatted with two-space indentation. Other field values are unchanged.

`SessionStart` has `transcript_path` null. `SessionEnd` has a transcript path, `reason` `completed` and `final_status` `completed`.

## Environment names

Values were not stored. The hook process did not have `CURSOR_CONVERSATION_ID`. Names set for these hooks that start with `CURSOR_` or `CLAUDE_` were `CURSOR_INVOKED_AS`, `CURSOR_PLUGIN_ROOT`, `CURSOR_PROJECT_DIR`, `CURSOR_RIPGREP_PATH`, `CURSOR_USER_EMAIL`, `CURSOR_VERSION`, `CLAUDE_PLUGIN_ROOT` and `CLAUDE_PROJECT_DIR`. `SessionEnd` also had `CURSOR_TRANSCRIPT_PATH` (`SessionStart` had `transcript_path` null, so that name was absent). The hook shell merges the ambient environment, so a parent `CLAUDE_CODE_SESSION_ID` would be inherited. This run's parent did not have one, and Cursor did not set it.
