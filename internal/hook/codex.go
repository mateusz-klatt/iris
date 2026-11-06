package hook

type codexAdapter struct{}

func (codexAdapter) Decode(eventName string, payload []byte) (Event, error) {
	return decodeCommon(ClientCodex, eventName, payload)
}

func (codexAdapter) Encode(ev Event, d Decision) Output {
	ev.Client = ClientCodex
	return encodeHookSpecific(ev, d)
}
