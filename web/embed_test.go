package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func serve(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func TestHandlerServesBuiltClient(t *testing.T) {
	client := handler(fstest.MapFS{
		"index.html":       {Data: []byte(`<div id="root"></div>`)},
		"assets/index.js":  {Data: []byte(`console.log("app")`)},
		"assets/index.css": {Data: []byte(`body{}`)},
	})

	for _, path := range []string{"/", "/conversations/123"} {
		recorder := serve(t, client, path)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `<div id="root"></div>`) {
			t.Fatalf("GET %s = %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	recorder := serve(t, client, "/assets/index.js")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset = %d, Cache-Control %q", recorder.Code, recorder.Header().Get("Cache-Control"))
	}
}

func TestHandlerExplainsMissingBuild(t *testing.T) {
	recorder := serve(t, handler(fstest.MapFS{}), "/")
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "make web-build") {
		t.Fatalf("unbuilt UI = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestEmbeddedHandlerServesClientOrBuildHint(t *testing.T) {
	recorder := serve(t, Handler(), "/")
	body := recorder.Body.String()
	built := recorder.Code == http.StatusOK && strings.Contains(body, `<div id="root"></div>`)
	unbuilt := recorder.Code == http.StatusNotFound && strings.Contains(body, "make web-build")
	if !built && !unbuilt {
		t.Fatalf("embedded UI = %d %s", recorder.Code, body)
	}
}
