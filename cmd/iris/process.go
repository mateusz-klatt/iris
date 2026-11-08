package main

import "github.com/mateusz-klatt/iris/internal/proc"

// watchParent is the process boundary for commands that follow their parent.
var watchParent = proc.WatchParent
