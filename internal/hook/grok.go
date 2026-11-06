package hook

type grokAdapter struct{}

func (grokAdapter) Decode(eventName string, payload []byte) (Event, error) {
	return decodeCommon(ClientGrok, eventName, payload)
}

func (grokAdapter) Encode(ev Event, d Decision) Output {
	return encodeGrok(ev, d)
}
