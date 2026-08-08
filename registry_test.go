package main

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCPAPluginRegistry(t *testing.T) {
	raw, errRead := os.ReadFile("registry.json")
	if errRead != nil {
		t.Fatalf("read registry.json: %v", errRead)
	}
	var registry struct {
		SchemaVersion int `json:"schema_version"`
		Plugins       []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Author      string `json:"author"`
			Version     string `json:"version"`
			Repository  string `json:"repository"`
			Install     struct {
				Type string `json:"type"`
			} `json:"install"`
		} `json:"plugins"`
	}
	if errDecode := json.Unmarshal(raw, &registry); errDecode != nil {
		t.Fatalf("decode registry.json: %v", errDecode)
	}
	if registry.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", registry.SchemaVersion)
	}
	if len(registry.Plugins) != 1 {
		t.Fatalf("plugins length = %d, want 1", len(registry.Plugins))
	}
	plugin := registry.Plugins[0]
	if plugin.ID != pluginIdentifier {
		t.Fatalf("plugin id = %q, want %q", plugin.ID, pluginIdentifier)
	}
	if plugin.Version != "" {
		t.Fatalf("registry version = %q, want empty so CPA resolves the latest GitHub release", plugin.Version)
	}
	for field, value := range map[string]string{
		"name":        plugin.Name,
		"description": plugin.Description,
		"author":      plugin.Author,
		"repository":  plugin.Repository,
	} {
		if strings.TrimSpace(value) == "" {
			t.Fatalf("registry plugin field %s is empty", field)
		}
	}
	if strings.TrimSpace(plugin.Install.Type) != "" {
		t.Fatalf("install type = %q, want empty for CPA's default github-release installer", plugin.Install.Type)
	}
	repository, errURL := url.Parse(plugin.Repository)
	if errURL != nil || repository.Scheme != "https" || repository.Host != "github.com" || strings.Trim(repository.Path, "/") != "yueziji/model-retry-wrapper-plugin" {
		t.Fatalf("repository = %q, want the canonical GitHub repository URL", plugin.Repository)
	}
}

func TestReleaseWorkflowPublishesCPAAssets(t *testing.T) {
	raw, errRead := os.ReadFile(".github/workflows/release.yml")
	if errRead != nil {
		t.Fatalf("read release workflow: %v", errRead)
	}
	var workflow yaml.Node
	if errDecode := yaml.Unmarshal(raw, &workflow); errDecode != nil {
		t.Fatalf("decode release workflow: %v", errDecode)
	}
	text := string(raw)
	for name, expected := range map[string]string{
		"CPA archive name": "${{ env.PLUGIN_NAME }}_${version}_${{ matrix.goos }}_${{ matrix.goarch }}.zip",
		"checksum file":    "sha256sum *.zip > checksums.txt",
		"release files":    "files: release-assets/*",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("release workflow is missing %s pattern %q", name, expected)
		}
	}
}
