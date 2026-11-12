package wake

import (
	"context"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

type targetResolver interface {
	Primary() (binding.WakeTarget, bool)
	StartIfPrimary(binding.WakeTarget, func() error) (bool, error)
}

type attentionRead struct {
	Attention hubclient.Attention
	Stream    string
	HighWater int64
}

type eventFrame struct {
	Stream string `json:"stream"`
	Cursor int64  `json:"cursor"`
	Kind   string `json:"kind"`
}

type eventConnection interface {
	Read(context.Context) (eventFrame, error)
	Close()
}

type wakeSource interface {
	HookSeen(context.Context, binding.WakeTarget) (bool, error)
	Attention(context.Context, binding.WakeTarget, int64, string) (attentionRead, error)
	Connect(context.Context, binding.WakeTarget, int64, string) (eventConnection, error)
}

type stepResult struct {
	Due      time.Time
	FileWait bool
}

// An engine belongs to one positive binding snapshot. The session's persistent
// ledger is shared across replacements; replacing an engine never clears it.
type engine struct {
	target        binding.WakeTarget
	resolver      targetResolver
	source        wakeSource
	store         *stateStore
	commands      *commands
	clientVersion func() string
	now           func() time.Time
	observe       func(string)
	hookSeen      bool
	cursor        int64
	stream        string
}
