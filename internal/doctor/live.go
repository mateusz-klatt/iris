package doctor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// LiveArgs is the opt-in client invocation. Stdin must be closed: codex exec
// waits while stdin is open. Models are the cheapest ones named in the
// client reports. The others use the client's own default, because the
// report does not name a cheaper model. No trust-bypass flag is set.
func LiveArgs(client, bin string) (string, []string, error) {
	switch client {
	case integrate.Claude:
		return bin, []string{"-p", "--model", "haiku", "Reply with ok"}, nil
	case integrate.Codex:
		return bin, []string{"exec", "Reply with ok"}, nil
	case integrate.Copilot:
		return bin, []string{"-p", "Reply with ok"}, nil
	case integrate.Cursor:
		return bin, []string{"-p", "--model", "auto", "Reply with ok"}, nil
	case integrate.Antigravity:
		return bin, []string{"--print=Reply with ok", "--model", "gemini-3.8-flash-low"}, nil
	case integrate.Grok:
		return bin, []string{"-p", "Reply with ok"}, nil
	case integrate.Kimi:
		return bin, []string{"-p", "Reply with ok"}, nil
	default:
		return "", nil, fmt.Errorf("unknown client %s", client)
	}
}

func runLive(ctx context.Context, opt Options, client string) error {
	bin, binErr := opt.Layout.Binary(client)
	name, args, argsErr := LiveArgs(client, bin)
	if err := errors.Join(binErr, argsErr); err != nil {
		return err
	}
	if opt.LiveRun != nil {
		return opt.LiveRun(ctx, name, args)
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Stdin = closedStdin()
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := string(out)
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}
