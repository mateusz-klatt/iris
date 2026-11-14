//go:build !linux

package hook

import "errors"

func init() {
	readCursorArgs = func(int, int64) ([]string, error) {
		return nil, errors.New("arguments unreadable")
	}
}
