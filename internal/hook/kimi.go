package hook

type kimiAdapter struct{}

func (kimiAdapter) Decode(eventName string, payload []byte) (Event, error) {
	return decodeCommon(ClientKimi, eventName, payload)
}

func (kimiAdapter) Encode(ev Event, d Decision) Output {
	return encodeKimi(ev, d)
}
