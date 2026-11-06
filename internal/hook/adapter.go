package hook

type adapter interface {
	Decode(eventName string, payload []byte) (Event, error)
	Encode(ev Event, d Decision) Output
}

func adapterFor(c Client) (adapter, bool) {
	switch c {
	case ClientClaude:
		return claudeAdapter{}, true
	case ClientCodex:
		return codexAdapter{}, true
	case ClientCopilot:
		return copilotAdapter{}, true
	case ClientCursor:
		return cursorAdapter{}, true
	case ClientAntigravity:
		return antigravityAdapter{}, true
	case ClientGrok:
		return grokAdapter{}, true
	case ClientKimi:
		return kimiAdapter{}, true
	default:
		return nil, false
	}
}

// Decode parses a client payload. eventName is the --event argument.
func Decode(client Client, eventName string, payload []byte) (Event, error) {
	a, ok := adapterFor(client)
	if !ok {
		return Event{}, errNoSession
	}
	return a.Decode(eventName, payload)
}

// Encode renders a decision in the client's output contract.
func Encode(ev Event, d Decision) Output {
	a, ok := adapterFor(ev.Client)
	if !ok {
		return FallbackAllow(ev.Client)
	}
	return a.Encode(ev, d)
}
