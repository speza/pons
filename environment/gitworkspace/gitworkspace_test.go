package gitworkspace

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/protocol"
)

func TestValidatePlan(t *testing.T) {
	valid := environment.WorkspacePlan{
		Strategy:     environment.WorkspaceStrategyGit,
		SourceRef:    "https://github.com/example/project.git",
		BaseRevision: strings.Repeat("a", 40),
	}
	if err := ValidatePlan(valid); err != nil {
		t.Fatalf("valid Git plan rejected: %v", err)
	}
	for name, mutate := range map[string]func(*environment.WorkspacePlan){
		"credential in URL": func(plan *environment.WorkspacePlan) {
			plan.SourceRef = "https://token@github.com/example/project.git"
		},
		"query in URL": func(plan *environment.WorkspacePlan) {
			plan.SourceRef = "https://github.com/example/project.git?token=secret"
		},
		"mutable revision": func(plan *environment.WorkspacePlan) { plan.BaseRevision = "main" },
		"SHA-256 revision": func(plan *environment.WorkspacePlan) { plan.BaseRevision = strings.Repeat("a", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			plan := valid
			mutate(&plan)
			if err := ValidatePlan(plan); err == nil {
				t.Fatal("invalid Git plan was accepted")
			}
		})
	}
}

func TestHTTPSBasicCredentialsAreTransientAndRedacted(t *testing.T) {
	token := "installation-secret"
	credentials := HTTPSBasicCredentials("https://github.com/", "x-access-token", token)
	env := credentials.Environment()
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "http.https://github.com/.extraheader" ||
		env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("Git environment = %v", env)
	}
	header := strings.TrimPrefix(env["GIT_CONFIG_VALUE_0"], "Authorization: Basic ")
	credential, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		t.Fatal(err)
	}
	if string(credential) != "x-access-token:"+token {
		t.Fatalf("credential = %q", credential)
	}
	result := credentials.RedactResult(protocol.ToolResult{
		Output:  "TOKEN=" + token,
		Error:   "Git sent " + env["GIT_CONFIG_VALUE_0"],
		Payload: json.RawMessage(`{"value":"` + header + `"}`),
	})
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{token, header, env["GIT_CONFIG_VALUE_0"]} {
		if bytes.Contains(encoded, []byte(sensitive)) {
			t.Fatalf("redacted result contains %q: %s", sensitive, encoded)
		}
	}
	if strings.Count(string(encoded), "[REDACTED]") != 3 {
		t.Fatalf("redacted result = %s", encoded)
	}
}

func TestBranchNameIsStableAndWorkspaceSpecific(t *testing.T) {
	first := BranchName("workspace-one")
	if first != BranchName("workspace-one") || first == BranchName("workspace-two") ||
		!strings.HasPrefix(first, "pons/") || !strings.HasSuffix(first, "/work") {
		t.Fatalf("branch names = %q, %q", first, BranchName("workspace-two"))
	}
}
