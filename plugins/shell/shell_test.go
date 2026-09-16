package shell

import (
	"strings"
	"testing"
	"time"
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

func TestTimeoutIsFailure(t *testing.T) {
	p := New(Config{Allow: []string{"while"}, Timeout: 25 * time.Millisecond})
	res, err := p.run(t.Context(), Run("while true; do :; done"))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Error, "timed out") {
		t.Fatalf("timeout should be a failed observation: %+v", res)
	}
}
