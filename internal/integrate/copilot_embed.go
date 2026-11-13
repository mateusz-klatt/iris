package integrate

import _ "embed"

//go:embed copilotextension/extension.mjs
var copilotExtension string

//go:embed copilotextension/watcher.mjs
var copilotWatcher string
