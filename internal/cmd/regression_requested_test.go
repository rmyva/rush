package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestProvidersAddLocalRefreshWritesModelsToWorkspace(t *testing.T) {
	requested := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requested <- r.Method + " " + r.URL.Path:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"local-model"}]}`))
	}))
	defer server.Close()

	fixture := `{"providers":{}}`
	id := "local-refresh"
	_, globalPath, workspacePath, err := runProvidersCmdInIsolatedAppFull(
		t, providersAddCmd, fixture,
		id+" --name Local --type openai-compat --base-url="+server.URL+" --enable=true --local",
		".rush",
	)
	require.NoError(t, err)
	select {
	case request := <-requested:
		require.Equal(t, "GET /models", request, "--enable=true must fetch models, not just persist provider config")
	default:
		t.Fatal("--enable=true did not fetch models")
	}
	globalData, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	workspaceData, err := os.ReadFile(workspacePath)
	require.NoError(t, err)
	var global, workspace struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(globalData, &global))
	require.NoError(t, json.Unmarshal(workspaceData, &workspace))
	require.NotContains(t, global.Providers, id)
	var provider struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(workspace.Providers[id], &provider))
	require.Len(t, provider.Models, 1)
	require.Equal(t, []string{"local-model"}, []string{provider.Models[0].ID})
}

func TestProvidersUpdateAllFailureReturnsError(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// App startup probes the provider first and drops it when that fails;
		// only the update command's own fetch may fail.
		if requests.Add(1) < 2 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"startup-model"}]}`))
			return
		}
		http.Error(w, "provider unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	fixture := `{"providers":{"broken":{"name":"Broken","type":"openai-compat","base_url":"` + server.URL + `","models":[]}}}`
	_, _, _, err := runProvidersCmdInIsolatedAppFull(t, providersUpdateCmd, fixture, "--all", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to update 1 provider")
	require.Contains(t, err.Error(), "HTTP 502")
}

func TestProvidersUpdatePersistenceErrorPropagates(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// App startup performs its own model request; only the update command's
		// fetch (the second one) may break persistence, otherwise setup fails
		// before the code under test runs.
		if requests.Add(1) < 2 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"startup-model"}]}`))
			return
		}
		// Replace the config file with a directory during fetch so
		// SetConfigFields cannot persist the result.
		configPath := filepath.Join(os.Getenv("RUSH_GLOBAL_DATA"), "rush.json")
		if err := os.Remove(configPath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Mkdir(configPath, 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"updated-model"}]}`))
	}))
	defer server.Close()
	fixture := `{"providers":{"persist":{"name":"Persist","type":"openai-compat","base_url":"` + server.URL + `","models":[]}}}`
	_, _, _, err := runProvidersCmdInIsolatedAppFull(t, providersUpdateCmd, fixture, "persist", "")
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to save updated models")
}

func TestProvidersDottedIDSetAndPatch(t *testing.T) {
	const id = "team.prod"
	fixture := `{"providers":{}}`
	_, _, workspacePath, err := runProvidersCmdInIsolatedAppFull(t, providersSetCmd, fixture,
		id+" --name Team --type openai --local", ".rush")
	require.NoError(t, err)
	workspaceData, err := os.ReadFile(workspacePath)
	require.NoError(t, err)
	var setResult struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(workspaceData, &setResult))
	require.Equal(t, "Team", setResult.Providers[id]["name"])

	_, patchGlobalPath, patchWorkspacePath, err := runProvidersCmdInIsolatedAppFull(t, providersPatchCmd, fixture,
		id+` --local --json {"api_key":"patched"}`, ".rush")
	require.NoError(t, err)
	patchData, err := os.ReadFile(patchWorkspacePath)
	require.NoError(t, err)
	globalData, err := os.ReadFile(patchGlobalPath)
	require.NoError(t, err)
	require.NotContains(t, string(globalData), id)
	var got struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(patchData, &got))
	require.Equal(t, "patched", got.Providers[id]["api_key"])
}

func TestProvidersAddLocalWritesWorkspaceConfig(t *testing.T) {
	const id = "regression-local-provider"
	requested := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requested <- r.Method + " " + r.URL.Path:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"local-model"}]}`))
	}))
	defer server.Close()

	fixture := `{"providers":{}}`
	_, globalPath, workspacePath, err := runProvidersCmdInIsolatedAppFull(
		t,
		providersAddCmd,
		fixture,
		id+" --name Local --type openai-compat --base-url="+server.URL+" --enable=true --local",
		".rush",
	)
	require.NoError(t, err)
	select {
	case request := <-requested:
		require.Equal(t, "GET /models", request, "enabled local provider should refresh models")
	default:
		t.Fatal("enabled local provider did not fetch models")
	}
	require.NotEmpty(t, workspacePath)
	globalData, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	workspaceData, err := os.ReadFile(workspacePath)
	require.NoError(t, err)
	var global, workspace struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(globalData, &global))
	require.NoError(t, json.Unmarshal(workspaceData, &workspace))
	require.NotContains(t, global.Providers, id, "--local must not modify global config")
	var provider struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(workspace.Providers[id], &provider))
	require.Len(t, provider.Models, 1)
	require.Equal(t, "local-model", provider.Models[0].ID)
}

func TestProviderSJSONPathEscapingPreservesDottedIDAndSpecialComponents(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, tc := range []struct {
		id, key string
	}{
		{"team.prod", "field.name"},
		{"team*prod", "field*name"},
		{"team?prod", "field?name"},
		{"team#prod", "field#name"},
		{"team|prod", "field|name"},
		{"team@prod", "field@name"},
		{`team\prod`, `field\name`},
	} {
		t.Run(tc.id+"/"+tc.key, func(t *testing.T) {
			path := "providers." + escapeSJSONPathComponent(tc.id) + "." + escapeSJSONPathComponent(tc.key)
			doc, err := sjson.Set(`{}`, path, "value")
			require.NoError(t, err)
			var got map[string]map[string]map[string]string
			require.NoError(t, json.Unmarshal([]byte(doc), &got))
			require.Equal(t, "value", got["providers"][tc.id][tc.key])
		})
	}
}

func TestExtractFilePathsPreservesEscapedQuotesAndWindowsSeparators(t *testing.T) {
	input := `{"file_path":"C:\\work\\a\"b.go"}`
	require.Equal(t, []string{`C:\work\a"b.go`}, extractFilePaths("edit", input))
}

func TestPrintTreeRootChildrenConnectors(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tree")
	require.NoError(t, err)
	root := session.Session{ID: "root", Title: "root"}
	children := map[string][]session.Session{
		"root":   {{ID: "child1", Title: "child1"}, {ID: "child2", Title: "child2"}},
		"child1": {{ID: "grandchild", Title: "grandchild"}},
	}
	printTreeNode(f, root, children, "", true, 0, 0)
	require.NoError(t, f.Close())
	data, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 4)
	require.Contains(t, lines[1], "├── child1")
	require.Contains(t, lines[2], "│   └── grandchild")
	require.Contains(t, lines[3], "└── child2")
}
