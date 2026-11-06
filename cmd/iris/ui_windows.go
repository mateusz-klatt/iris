//go:build windows

package main

func browserCommand(url string) []string {
	return []string{"rundll32", "url.dll,FileProtocolHandler", url}
}
