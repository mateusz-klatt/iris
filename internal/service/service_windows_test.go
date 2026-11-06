package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRegistryOwnValue(t *testing.T) {
	v := registryValue{fmt.Sprintf("Iris-test-%d-%d", os.Getpid(), time.Now().UnixNano())}
	t.Cleanup(func() {
		if err := v.Remove(); err != nil {
			t.Error(err)
		}
	})
	if err := v.Write(`"C:\Program Files\Iris\iris.exe" service start`); err != nil {
		t.Fatal(err)
	}
	got, err := v.Read()
	if err != nil || !strings.Contains(got, "service start") {
		t.Fatalf("%q %v", got, err)
	}
	if err := v.Remove(); err != nil {
		t.Fatal(err)
	}
	got, err = v.Read()
	if err != nil || got != "" {
		t.Fatalf("%q %v", got, err)
	}
}
func TestDetachedHubHelper(t *testing.T) {
	if os.Getenv("IRIS_SERVICE_TEST_CHILD") != "1" {
		return
	}
	fmt.Println("detached child")
	time.Sleep(time.Minute)
	os.Exit(0)
}
func TestDetachedHubIdentity(t *testing.T) {
	t.Setenv("IRIS_SERVICE_TEST_CHILD", "1")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := detachedHub{argv: []string{bin, "-test.run=^TestDetachedHubHelper$"}, dir: t.TempDir(), url: "http://127.0.0.1:1/v1/status"}
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.dir, "hub.pid")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var id processIdentity
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatal(err)
	}
	// Keep the original identity available even if a later assertion fails.
	t.Cleanup(func() {
		_ = os.WriteFile(path, raw, 0600)
		if err := p.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	h, err := p.openRecorded()
	if err != nil || h == 0 {
		t.Fatalf("identity: %v %v", h, err)
	}
	defer windows.CloseHandle(h)
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != string(raw) {
		t.Fatal("duplicate start replaced identity")
	}
	id.Created++
	wrong, _ := json.Marshal(id)
	if err := os.WriteFile(path, wrong, 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if state, err := windows.WaitForSingleObject(h, 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("stopped mismatched process: %v %v", state, err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if state, err := windows.WaitForSingleObject(h, 0); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("did not stop: %v %v", state, err)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "hub.log")); err != nil {
		t.Fatal(err)
	}
}
