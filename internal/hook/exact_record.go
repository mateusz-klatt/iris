package hook

import (
	"os"
	"strings"
)

func prepareSessionFile(dir, body string, restrict func(string) error) (string, error) {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if body != "" {
		_, err = f.WriteString(strings.TrimRight(body, "\n") + "\n")
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	// The file is protected before it gets its name, so it is never readable as an entry.
	if err == nil && restrict != nil {
		err = restrict(name)
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func writePreparedSessionFile(path, body string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strings.TrimRight(body, "\n") + "\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
