package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/biga-sh/cli/internal/session"
	"github.com/biga-sh/cli/internal/ui"
)

func TestExtensionCommandsUseSavedSessionOwner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-owner" {
			t.Error("request did not use the saved owner credential")
		}
		if r.Header.Get("X-Biga-Key") != "synthetic-access-key" {
			t.Error("request did not include the server access key")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/sessions/sample/extensions":
			var body struct{ Name string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name != "Build monitor" {
				t.Error("incorrect extension creation payload")
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"ok":true,"data":{"id":"extension-id","name":"Build monitor","token":"synthetic-extension-token"}}`)
		case "GET /api/sessions/sample/extensions":
			io.WriteString(w, `{"ok":true,"data":{"extensions":[{"id":"extension-id","name":"Build monitor"}]}}`)
		case "DELETE /api/sessions/sample/extensions/extension-id":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("BIGA_HOME", home)
	t.Setenv("BIGA_ACCESS_KEY", "synthetic-access-key")
	t.Setenv("BIGA_NO_UPDATE_CHECK", "1")
	if err := session.NewStore(home).Save(session.Record{RootDir: dir, SessionID: "sample", APIURL: server.URL, Token: "synthetic-owner"}); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	for _, args := range [][]string{{"create", "Build monitor"}, {"list"}, {"revoke", "extension-id"}} {
		a := &app{ui: ui.New(output)}
		cmd := a.rootCmd()
		cmd.SetArgs(append([]string{"extensions", "--dir", dir}, args...))
		if err := cmd.ExecuteContext(t.Context()); err != nil {
			t.Fatalf("%s: %v", args[0], err)
		}
	}
	a := &app{ui: ui.New(output)}
	cmd := a.rootCmd()
	cmd.SetArgs([]string{"extensions", "list", "--dir", dir, "--api-url", "https://unrelated.example"})
	if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "this session belongs to") {
		t.Fatalf("expected server override to be rejected, got %v", err)
	}
}
