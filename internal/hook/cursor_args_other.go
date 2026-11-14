//go:build !linux

package hook

import "errors"

func init() { readCursorArgs = unreadCursorArgs }

func unreadCursorArgs(int, int64) ([]string, error) {
	return nil, errors.New("arguments unreadable")
}
