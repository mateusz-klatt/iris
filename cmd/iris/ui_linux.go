//go:build linux

package main

func browserCommand(url string) []string {
	return []string{"xdg-open", url}
}
