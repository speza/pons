package runtime_test

import (
	"testing"

	ponsruntime "github.com/samperrin/pons/runtime"
)

func TestAgentValidateRejectsMultilineName(t *testing.T) {
	for _, name := range []string{"Ada\nGrace", " Ada", "Ada\r"} {
		if err := (ponsruntime.AgentDefinition{ID: "default", Name: name, WorkspacePolicy: ponsruntime.WorkspaceAgent}).Validate(); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	if err := (ponsruntime.AgentDefinition{ID: "default", Name: "Ada Lovelace", WorkspacePolicy: ponsruntime.WorkspaceAgent}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentValidateRequiresKnownWorkspacePolicy(t *testing.T) {
	for _, policy := range []string{"", "shared", "Agent"} {
		if err := (ponsruntime.AgentDefinition{ID: "default", WorkspacePolicy: policy}).Validate(); err == nil {
			t.Errorf("workspace policy %q accepted", policy)
		}
	}
}

func TestAgentRevisionIsDeterministicAndTracksPersona(t *testing.T) {
	base := ponsruntime.AgentDefinition{
		ID: ponsruntime.DefaultAgentID, Name: "Ada", Persona: "Be brief.", ProviderSlot: "primary",
		Model: "model", MaxTurns: 10, PluginPaths: []string{"/plugins/a.json"},
		WorkspacePolicy: ponsruntime.WorkspaceAgent,
	}
	same := base
	same.PluginPaths = []string{"/plugins/a.json"}
	if base.Revision() != same.Revision() {
		t.Fatal("equal definitions have different revisions")
	}
	if len(base.Revision()) != 64 {
		t.Fatalf("revision %q is not a hex SHA-256", base.Revision())
	}

	noPlugins, emptyPlugins := base, base
	noPlugins.PluginPaths, emptyPlugins.PluginPaths = nil, []string{}
	if noPlugins.Revision() != emptyPlugins.Revision() {
		t.Fatal("nil and empty plugin lists have different revisions")
	}

	for name, change := range map[string]func(*ponsruntime.AgentDefinition){
		"name":     func(d *ponsruntime.AgentDefinition) { d.Name = "Grace" },
		"persona":  func(d *ponsruntime.AgentDefinition) { d.Persona = "Be thorough." },
		"slot":     func(d *ponsruntime.AgentDefinition) { d.ProviderSlot = "backup" },
		"model":    func(d *ponsruntime.AgentDefinition) { d.Model = "other" },
		"turns":    func(d *ponsruntime.AgentDefinition) { d.MaxTurns = 11 },
		"plugins":  func(d *ponsruntime.AgentDefinition) { d.PluginPaths = nil },
		"identity": func(d *ponsruntime.AgentDefinition) { d.ID = "other" },
		"workspace": func(d *ponsruntime.AgentDefinition) {
			d.WorkspacePolicy = ponsruntime.WorkspacePerConversation
		},
	} {
		changed := base
		change(&changed)
		if changed.Revision() == base.Revision() {
			t.Errorf("changing %s kept revision", name)
		}
	}
}
