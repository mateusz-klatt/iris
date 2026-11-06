package hook

type claudeAdapter struct{}

func (claudeAdapter) Decode(eventName string, payload []byte) (Event, error) {
	return decodeCommon(ClientClaude, eventName, payload)
}

func (claudeAdapter) Encode(ev Event, d Decision) Output {
	ev.Client = ClientClaude
	return encodeHookSpecific(ev, d)
}
