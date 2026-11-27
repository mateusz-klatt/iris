# Interactive cursor-agent argv on macOS

`argv-interactive-2026.11.22-darwin.json` records the arguments of a live interactive
`cursor-agent` 2026.11.22-e9042b2 process on macOS 26 (Darwin 25.6.0, Intel), captured
on 2026-11-27 at approximately 20:13 UTC with `ps -o args=`. The shell launcher's
`exec -a` preserves the invoking path as argv[0]. The `ps` output was transcribed
as a JSON array using the original invocation to preserve the prompt as a single
argument. The capturing user's home directory was replaced with `/Users/user` in
argv[0] and the `index.js` path.
