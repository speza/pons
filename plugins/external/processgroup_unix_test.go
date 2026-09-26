//go:build unix

package external_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samperrin/pons/plugins/external"
)

func TestCloseStopsPluginDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-write")
	release := func() { _ = os.WriteFile(marker+".release", nil, 0o600) }
	t.Cleanup(release)
	plugin, err := external.NewHands(rawHelperManifest(t), external.HostConfig{
		Env: []string{"PONS_EXTERNAL_RAW=1", "PONS_EXTERNAL_DESCENDANT_PATH=" + marker},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plugin.Close() })
	if err := plugin.Host().Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Close(); err != nil {
		t.Fatal(err)
	}
	release()
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("plugin descendant wrote after successful close: %v", err)
	}
}
