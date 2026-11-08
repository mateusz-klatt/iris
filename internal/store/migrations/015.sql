-- The path a host first named for a worktree directory, keyed by that directory's device and
-- inode, so another path to the same directory, through a bind mount or a symlink, is the same
-- worktree and the same agent (D6).
CREATE TABLE WorktreeAlias (host_id TEXT NOT NULL, worktree_id TEXT NOT NULL, worktree TEXT NOT NULL, PRIMARY KEY (host_id, worktree_id));
PRAGMA user_version=15;
