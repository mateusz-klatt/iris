//go:build darwin

package main

func browserCommand(url string) []string {
	return []string{"open", url}
}
