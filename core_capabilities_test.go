package pons

import "testing"

func TestRegisterCapability(t *testing.T) {
	core := New()
	if _, ok := core.Capability("classifier:example"); ok {
		t.Fatal("unregistered capability was found")
	}
	if err := core.RegisterCapability("classifier:example", 42); err != nil {
		t.Fatal(err)
	}
	if got, ok := core.Capability("classifier:example"); !ok || got != 42 {
		t.Fatalf("capability = %v, %v", got, ok)
	}
	if err := core.RegisterCapability("classifier:example", 7); err == nil {
		t.Fatal("duplicate capability was accepted")
	}
}

func TestRegisterCapabilityRejectsNil(t *testing.T) {
	core := New()
	var typedNil *int
	for _, value := range []any{nil, typedNil} {
		if err := core.RegisterCapability("classifier:example", value); err == nil {
			t.Fatalf("nil capability %T was accepted", value)
		}
	}
	if err := core.RegisterCapability("", 42); err == nil {
		t.Fatal("empty capability name was accepted")
	}
}
