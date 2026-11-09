package integrate

import (
	"errors"
	"os"
)

// readExisting reads a client file about to be changed. It refuses anything but a regular file,
// and absent says there is no file yet: a file it cannot read is an error, never an empty file.
func readExisting(path string) (data []byte, absent bool, err error) {
	if err = refuseSpecial(path); err != nil {
		return nil, false, err
	}
	data, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	return data, false, err
}

func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotConfigured
	}
	return data, err
}
