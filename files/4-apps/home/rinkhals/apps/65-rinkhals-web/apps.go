package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Locations that match tools.sh:
//   BUILTIN_APP_PATH = $RINKHALS_ROOT/home/rinkhals/apps   (system apps shipped in the SWU)
//   USER_APP_PATH    = $RINKHALS_HOME/apps                 (user overrides + per-app .config files)
//   TEMPORARY_APP_PATH = /tmp/rinkhals/apps                (session-only overrides)
//
// RINKHALS_ROOT is the SWU mount; we resolve it from the live symlink so we
// pick up version upgrades without restarting rinkhals-web.
const (
	rinkhalsCurrent  = "/useremain/rinkhals/.current"
	userAppPath      = "/useremain/home/rinkhals/apps"
	temporaryAppPath = "/tmp/rinkhals/apps"
	toolsSh          = "/useremain/rinkhals/.current/tools.sh"
)

func builtinAppPath() string {
	return filepath.Join(rinkhalsCurrent, "home/rinkhals/apps")
}

// AppProperty mirrors a single entry in app.json's "properties" object,
// augmented with the resolved current value.
type AppProperty struct {
	Key        string   `json:"key"`
	Display    string   `json:"display"`
	Type       string   `json:"type"`
	Options    []string `json:"options,omitempty"`
	Default    string   `json:"default"`
	Value      string   `json:"value"`
	Overridden bool     `json:"overridden"`
}

// AppRequirements mirrors the optional "requirements" object.
type AppRequirements struct {
	Memory int `json:"memory,omitempty"`
	CPU    int `json:"cpu,omitempty"`
}

// AppManifest is the raw shape of app.json on disk.
type AppManifest struct {
	Version      string                            `json:"$version"`
	Name         string                            `json:"name"`
	Description  string                            `json:"description"`
	AppVersion   string                            `json:"version"`
	Requirements *AppRequirements                  `json:"requirements,omitempty"`
	Properties   map[string]map[string]interface{} `json:"properties,omitempty"`
}

// App is the wire shape returned by /api/apps.
type App struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Description  string           `json:"description"`
	Version      string           `json:"version"`
	Source       string           `json:"source"` // "system" or "user"
	Enabled      bool             `json:"enabled"`
	Running      bool             `json:"running"`
	Requirements *AppRequirements `json:"requirements,omitempty"`
	Properties   []AppProperty    `json:"properties"`
}

// listAppIDs walks builtin + user directories and returns a deduplicated,
// sorted list of app IDs (the directory names, e.g. "40-moonraker").
func listAppIDs() []string {
	seen := map[string]bool{}
	add := func(root string) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			seen[e.Name()] = true
		}
	}
	add(builtinAppPath())
	add(userAppPath)

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// resolveAppRoot picks the user app directory when present, otherwise the
// builtin one — same precedence tools.sh's get_app_root uses.
func resolveAppRoot(id string) (root, source string) {
	userRoot := filepath.Join(userAppPath, id)
	if st, err := os.Stat(userRoot); err == nil && st.IsDir() {
		return userRoot, "user"
	}
	return filepath.Join(builtinAppPath(), id), "system"
}

func readManifest(root string) (*AppManifest, error) {
	data, err := os.ReadFile(filepath.Join(root, "app.json"))
	if err != nil {
		return nil, err
	}
	var m AppManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// readUserConfig returns the persisted user overrides for an app (the JSON
// blob written by set_app_property). Returns empty map if absent.
func readUserConfig(id string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(filepath.Join(userAppPath, id+".config"))
	if err != nil {
		return out
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return out
	}
	for k, v := range raw {
		if v == nil {
			continue
		}
		out[k] = fmt.Sprint(v)
	}
	return out
}

// queryAppStates runs a single shell invocation that emits a line per app
// with enabled and running flags. This keeps the cost flat (one fork) instead
// of paying it per-app.
func queryAppStates(ids []string) map[string]struct {
	Enabled bool
	Running bool
} {
	result := map[string]struct {
		Enabled bool
		Running bool
	}{}
	if len(ids) == 0 {
		return result
	}

	// Build a small shell snippet that prints "<id>|<enabled>|<status>" for each.
	// is_app_enabled prints 1 when enabled, blank otherwise. get_app_status
	// prints "started" or "stopped".
	var b strings.Builder
	fmt.Fprintf(&b, ". %s\n", toolsSh)
	for _, id := range ids {
		fmt.Fprintf(&b,
			"printf '%s|%%s|%%s\\n' \"$(is_app_enabled %s 2>/dev/null)\" \"$(get_app_status %s 2>/dev/null)\"\n",
			id, id, id)
	}

	out, err := exec.Command("sh", "-c", b.String()).Output()
	if err != nil {
		return result
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		result[parts[0]] = struct {
			Enabled bool
			Running bool
		}{
			Enabled: strings.TrimSpace(parts[1]) == "1",
			Running: strings.Contains(parts[2], "started"),
		}
	}
	return result
}

// buildProperties merges app.json's property declarations with the
// persisted user overrides, picking the effective value and marking which
// keys are overridden.
func buildProperties(manifest *AppManifest, userCfg map[string]string) []AppProperty {
	if manifest == nil || len(manifest.Properties) == 0 {
		return []AppProperty{}
	}
	props := make([]AppProperty, 0, len(manifest.Properties))
	for key, raw := range manifest.Properties {
		p := AppProperty{Key: key}
		if v, ok := raw["display"].(string); ok {
			p.Display = v
		}
		if v, ok := raw["type"].(string); ok {
			p.Type = v
		}
		if v, ok := raw["default"]; ok && v != nil {
			p.Default = fmt.Sprint(v)
		}
		if opts, ok := raw["options"].([]interface{}); ok {
			for _, o := range opts {
				if o != nil {
					p.Options = append(p.Options, fmt.Sprint(o))
				}
			}
		}
		if override, has := userCfg[key]; has {
			p.Value = override
			p.Overridden = true
		} else {
			p.Value = p.Default
		}
		props = append(props, p)
	}
	sort.Slice(props, func(i, j int) bool { return props[i].Key < props[j].Key })
	return props
}

// loadApp returns the full wire shape for a single ID.
func loadApp(id string, state map[string]struct {
	Enabled bool
	Running bool
}) (*App, error) {
	root, source := resolveAppRoot(id)
	manifest, err := readManifest(root)
	if err != nil {
		// App directory exists but no manifest — surface a stub rather than failing the list.
		manifest = &AppManifest{Name: id}
	}

	userCfg := readUserConfig(id)
	props := buildProperties(manifest, userCfg)

	s := state[id]
	return &App{
		ID:           id,
		Name:         strings.TrimSpace(stringOr(manifest.Name, id)),
		Description:  manifest.Description,
		Version:      manifest.AppVersion,
		Source:       source,
		Enabled:      s.Enabled,
		Running:      s.Running,
		Requirements: manifest.Requirements,
		Properties:   props,
	}, nil
}

func stringOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// --- HTTP handlers ---

func handleAppsList(w http.ResponseWriter, r *http.Request) {
	writeJSONHeaders(w)
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ids := listAppIDs()
	state := queryAppStates(ids)

	apps := make([]*App, 0, len(ids))
	for _, id := range ids {
		app, err := loadApp(id, state)
		if err != nil {
			continue
		}
		apps = append(apps, app)
	}
	json.NewEncoder(w).Encode(apps)
}

func handleApp(w http.ResponseWriter, r *http.Request) {
	writeJSONHeaders(w)

	// Parse "/api/apps/{id}[/...]" with the suffix being subresource routing.
	rest := strings.TrimPrefix(r.URL.Path, "/api/apps/")
	parts := strings.SplitN(rest, "/", 2)
	if parts[0] == "" {
		http.Error(w, "App ID required", http.StatusBadRequest)
		return
	}
	id := parts[0]
	// App ids are directory names (e.g. "40-moonraker"); reject anything that
	// isn't a plain identifier so a crafted path can't reach the shell helpers,
	// several of which interpolate the id unquoted.
	if !isSafeKey(id) {
		http.Error(w, "Invalid app id", http.StatusBadRequest)
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}

	switch {
	case sub == "" && r.Method == "GET":
		state := queryAppStates([]string{id})
		app, err := loadApp(id, state)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(app)

	case sub == "enable" && r.Method == "POST":
		runAppHelper(w, "enable_app", id)

	case sub == "disable" && r.Method == "POST":
		runAppHelper(w, "disable_app", id)

	case sub == "action" && r.Method == "POST":
		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		switch req.Action {
		case "start":
			runAppHelper(w, "start_app", id)
		case "stop":
			runAppHelper(w, "stop_app", id)
		case "restart":
			q := shellQuote(id)
			runAppCmd(w, fmt.Sprintf("stop_app %s; sleep 1; start_app %s", q, q))
		default:
			http.Error(w, "Invalid action", http.StatusBadRequest)
		}

	case sub == "config" && r.Method == "GET":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(readUserConfig(id))

	case sub == "config" && r.Method == "PUT":
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		// Run all set_app_property calls in one shell invocation.
		var b strings.Builder
		fmt.Fprintf(&b, ". %s\n", toolsSh)
		for k, v := range body {
			// Reject any key that isn't a simple identifier so we can't inject jq
			// expressions into the helper.
			if !isSafeKey(k) {
				http.Error(w, "Invalid property key: "+k, http.StatusBadRequest)
				return
			}
			fmt.Fprintf(&b, "set_app_property %s %s %s\n", shellQuote(id), shellQuote(k), shellQuote(v))
		}
		out, err := exec.Command("sh", "-c", b.String()).CombinedOutput()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": err == nil,
			"output":  string(out),
		})

	case sub == "config" && r.Method == "DELETE":
		runAppHelper(w, "clear_app_properties", id)

	case strings.HasPrefix(sub, "config/") && r.Method == "DELETE":
		key := strings.TrimPrefix(sub, "config/")
		if !isSafeKey(key) {
			http.Error(w, "Invalid property key", http.StatusBadRequest)
			return
		}
		runAppCmd(w, fmt.Sprintf("remove_app_property %s %s", shellQuote(id), shellQuote(key)))

	default:
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

// --- helpers ---

func writeJSONHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
}

func runAppHelper(w http.ResponseWriter, helper, id string) {
	runAppCmd(w, fmt.Sprintf("%s %s", helper, shellQuote(id)))
}

func runAppCmd(w http.ResponseWriter, body string) {
	cmd := fmt.Sprintf(". %s\n%s\n", toolsSh, body)
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": err == nil,
		"output":  string(out),
	})
}

func shellQuote(s string) string {
	// Single-quote and escape embedded single quotes the POSIX-portable way.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func isSafeKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}
