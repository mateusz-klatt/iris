-- The path a host first named for a worktree directory, keyed by that directory's device and
-- inode, so another path to the same directory, through a bind mount or a symlink, is the same
-- worktree and the same agent (D6).
-- The repository it was seen with guards against a device and inode that a remount or a reboot
-- gave to another directory: only a path of the same repository folds into the first one.
CREATE TABLE WorktreeAlias (host_id TEXT NOT NULL, worktree_id TEXT NOT NULL, worktree TEXT NOT NULL,
 remote_url TEXT NOT NULL, root_commit TEXT NOT NULL, PRIMARY KEY (host_id, worktree_id));
PRAGMA user_version=15;
