package service

import (
	"strings"
	"testing"
)

func TestUnitText(t *testing.T) {
	argv := []string{"/opt/iris", "serve", "--listen", "127.0.0.1:7878", "--token-file", "/home/a/token"}
	unit := string(SystemdUnit(argv))
	if !strings.Contains(unit, Marker) || !strings.Contains(unit, "ExecStart=/opt/iris serve --listen 127.0.0.1:7878 --token-file /home/a/token") {
		t.Fatal(unit)
	}
	plist := string(LaunchdPlist(argv))
	if !strings.Contains(plist, Marker) || !strings.Contains(plist, "<string>/opt/iris</string>") || !strings.Contains(plist, "<string>serve</string>") {
		t.Fatal(plist)
	}

}

func TestSystemdQuotesSpaces(t *testing.T) {
	unit := string(SystemdUnit([]string{`C:\Program Files\iris.exe`, "serve"}))
	if !strings.Contains(unit, `"C:\\Program Files\\iris.exe"`) && !strings.Contains(unit, `"C:\Program Files\iris.exe"`) {
		t.Fatal(unit)
	}
}
