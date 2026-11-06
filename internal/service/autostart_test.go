package service

import (
	"context"
	"errors"
	"testing"
)

type memoryValue struct {
	text string
	err  error
}

func (v *memoryValue) Read() (string, error) { return v.text, v.err }
func (v *memoryValue) Write(s string) error {
	if v.err != nil {
		return v.err
	}
	v.text = s
	return nil
}
func (v *memoryValue) Remove() error { v.text = ""; return v.err }

type memoryProcess struct {
	up            bool
	starts, stops int
	err           error
}

func (p *memoryProcess) Responds(context.Context) bool { return p.up }
func (p *memoryProcess) Start(context.Context) error   { p.starts++; p.up = p.err == nil; return p.err }
func (p *memoryProcess) Stop(context.Context) error    { p.stops++; p.up = false; return p.err }
func TestAutostartLifecycle(t *testing.T) {
	ctx := context.Background()
	value := &memoryValue{}
	process := &memoryProcess{}
	m := autostartManager{value, `"C:\Program Files\Iris\iris.exe" service start --db hub.db`, process}
	if err := m.Install(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := m.Status(ctx)
	if err != nil || !st.Installed || !st.Active || st.Detail != m.command {
		t.Fatalf("%+v %v", st, err)
	}
	if err := m.Start(ctx); err != nil || process.starts != 1 {
		t.Fatalf("duplicate start: %d %v", process.starts, err)
	}
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil || process.starts != 2 {
		t.Fatalf("restart: %d %v", process.starts, err)
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = m.Status(ctx)
	if err != nil || st.Installed || st.Active || process.stops != 2 {
		t.Fatalf("%+v %v", st, err)
	}
}
func TestAutostartFailure(t *testing.T) {
	ctx := context.Background()
	broken := errors.New("denied")
	value := &memoryValue{err: broken}
	process := &memoryProcess{}
	m := autostartManager{value, "iris service start", process}
	if !errors.Is(m.Install(ctx), broken) || process.starts != 0 {
		t.Fatal("started after registry failure")
	}
	value.err = nil
	process.err = broken
	if !errors.Is(m.Install(ctx), broken) || value.text == "" {
		t.Fatal("lost autostart after spawn failure")
	}
	if !errors.Is(m.Uninstall(ctx), broken) || value.text != "" {
		t.Fatal("failed to remove value after stop failure")
	}
}
