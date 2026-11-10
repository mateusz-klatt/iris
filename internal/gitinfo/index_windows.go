package gitinfo

import "errors"

// ReadIndexID is not available on Windows, where a reduced snapshot is never taken (ADR 018).
func ReadIndexID(string) (IndexID, error) {
	return IndexID{}, errors.New("index identity is not read on Windows")
}
