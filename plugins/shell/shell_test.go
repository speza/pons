package shell

import (
	"testing"
)

func TestAllowlist(t *testing.T) {
	p := New(Config{Allow: []string{"ls", "echo"}})

	if res, _ := p.run(t.Context(), Run("rm -rf /")); res.OK {
		t.Fatal("rm should be denied")
	}
	res, _ := p.run(t.Context(), Run("echo hi"))
	if res.OK == false || res.Output != "hi\n" {
		t.Fatalf("echo: %+v", res)
	}
}

func TestExitCodeIsObservation(t *testing.T) {
	p := New(Config{Allow: []string{"false"}})
	res, _ := p.run(t.Context(), Run("false"))
	if res.OK == false || res.ExitCode != 1 {
		t.Fatalf("non-zero exit should be an observation: %+v", res)
	}
}
