package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The community app catalog lives at https://github.com/rinkhals-community/Rinkhals.Apps.
// We hit two GitHub endpoints (Contents API + latest release) and then raw.githubusercontent.com
// for individual app.json files - the raw host isn't rate-limited, which keeps us well
// under the 60 unauthenticated API requests/hour ceiling.
const (
	catalogOwner          = "rinkhals-community"
	catalogRepo           = "Rinkhals.Apps"
	catalogBranch         = "master"
	catalogContentsAPI    = "https://api.github.com/repos/rinkhals-community/Rinkhals.Apps/contents/apps"
	catalogLatestRelease  = "https://api.github.com/repos/rinkhals-community/Rinkhals.Apps/releases/latest"
	catalogRawBase        = "https://raw.githubusercontent.com/rinkhals-community/Rinkhals.Apps"
	catalogCacheTTL       = 10 * time.Minute
	catalogHTTPTimeout    = 15 * time.Second
	// Download to persistent eMMC, not /tmp: /tmp is tmpfs (RAM) and a large app
	// SWU would be held in memory while install_swu also extracts into /useremain,
	// risking OOM on memory-constrained printers.
	catalogDownloadTmp    = "/useremain/rinkhals-catalog"
	catalogUserAgent      = "rinkhals-web"
)

// CatalogApp is the wire shape returned to the UI - the per-app manifest plus
// the installation-aware fields (download URL for this printer model, install state).
type CatalogApp struct {
	ID                 string           `json:"id"`
	Name               string           `json:"name"`
	Description        string           `json:"description"`
	Version            string           `json:"version"`
	Requirements       *AppRequirements `json:"requirements,omitempty"`
	Depends            []string         `json:"depends,omitempty"`
	AvailableForModel  bool             `json:"available_for_model"`
	DownloadURL        string           `json:"download_url,omitempty"`
	AssetSize          int64            `json:"asset_size,omitempty"`
	Installed          bool             `json:"installed"`
	InstalledVersion   string           `json:"installed_version,omitempty"`
}

type Catalog struct {
	Release   string       `json:"release"`
	FetchedAt time.Time    `json:"fetched_at"`
	Model     string       `json:"model"`         // resolved KOBRA_MODEL_CODE
	AssetGroup string      `json:"asset_group"`   // mapped SWU suffix (e.g. "k2p-k3")
	Apps      []CatalogApp `json:"apps"`
	Notice    string       `json:"notice,omitempty"` // surfaced when model has no SWU group mapping
}

// Cache state.
var (
	catalogMu    sync.Mutex
	catalogData  *Catalog
	catalogStamp time.Time
)

// modelToAssetGroup maps the KOBRA_MODEL_CODE that tools.sh exposes to the
// SWU asset suffix the Rinkhals.Apps release uses. Models without an
// explicit SWU build fall back to the closest hardware match - if Rinkhals.Apps
// later ships a dedicated build, the user will just stop seeing the fallback notice.
func modelToAssetGroup(model string) (string, string) {
	switch strings.ToUpper(strings.TrimSpace(model)) {
	case "K2P", "K3":
		return "k2p-k3", ""
	case "K3V2":
		return "k2p-k3", "no dedicated K3V2 SWU group; using K2P/K3 build"
	case "K3M":
		return "k3m", ""
	case "KS1":
		return "ks1", ""
	case "KS1M":
		return "ks1", "no dedicated KS1M SWU group; using KS1 build"
	default:
		return "", "unknown printer model (" + model + "); SWU downloads disabled"
	}
}

func currentModelCode() string {
	// tools.sh exports KOBRA_MODEL_CODE; rinkhals-web runs in an environment
	// that sources rinkhals, so it's available in os.Environ. As a safety net
	// we shell out if the var isn't already set.
	if m := os.Getenv("KOBRA_MODEL_CODE"); m != "" {
		return m
	}
	out, err := exec.Command("sh", "-c", ". "+toolsSh+" >/dev/null 2>&1; printf %s \"$KOBRA_MODEL_CODE\"").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// installedAppVersions reads each currently-installed user app's manifest and
// returns a name -> version map so we can mark catalog entries as already installed.
func installedAppVersions() map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(userAppPath)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := readManifest(filepath.Join(userAppPath, e.Name()))
		if err != nil || m == nil {
			out[e.Name()] = "" // installed, version unknown
			continue
		}
		out[e.Name()] = m.AppVersion
	}
	return out
}

// --- GitHub fetchers ---

func httpGetJSON(url string, into interface{}) error {
	client := &http.Client{Timeout: catalogHTTPTimeout}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", catalogUserAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

type ghContentEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type ghReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type ghRelease struct {
	TagName string           `json:"tag_name"`
	Assets  []ghReleaseAsset `json:"assets"`
}

// fetchManifest pulls apps/<name>/app.json from raw.githubusercontent.com.
// raw isn't rate-limited so we hammer it freely.
func fetchManifest(appName string) (*AppManifest, error) {
	url := fmt.Sprintf("%s/%s/apps/%s/app.json", catalogRawBase, catalogBranch, appName)
	var m AppManifest
	if err := httpGetJSON(url, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// rebuildCatalog is the slow path: list app dirs, fetch each manifest, fetch
// the latest release, fuse everything together, and return it. Safe to call
// concurrently with reads via the mutex.
func rebuildCatalog() (*Catalog, error) {
	// 1. List apps/
	var entries []ghContentEntry
	if err := httpGetJSON(catalogContentsAPI, &entries); err != nil {
		return nil, fmt.Errorf("listing catalog apps/: %w", err)
	}

	// 2. Latest release
	var rel ghRelease
	if err := httpGetJSON(catalogLatestRelease, &rel); err != nil {
		// Non-fatal: we can still show the catalog without download URLs.
		rel = ghRelease{}
	}

	// 3. Model resolution
	model := currentModelCode()
	group, notice := modelToAssetGroup(model)
	installed := installedAppVersions()

	// Index release assets by app name for quick lookup.
	type assetMatch struct {
		url  string
		size int64
	}
	assetByApp := map[string]assetMatch{}
	if group != "" {
		suffix := "-" + group + ".swu"
		prefix := "app-"
		for _, a := range rel.Assets {
			if !strings.HasPrefix(a.Name, prefix) || !strings.HasSuffix(a.Name, suffix) {
				continue
			}
			appName := strings.TrimSuffix(strings.TrimPrefix(a.Name, prefix), suffix)
			assetByApp[appName] = assetMatch{url: a.BrowserDownloadURL, size: a.Size}
		}
	}

	// 4. Pull each manifest. The listing has ~12 entries so doing this serially is fine
	//    and a lot cheaper than parallel HTTP for our latency budget.
	apps := make([]CatalogApp, 0, len(entries))
	for _, e := range entries {
		if e.Type != "dir" {
			continue
		}
		m, err := fetchManifest(e.Name)
		if err != nil {
			// One bad manifest shouldn't take down the catalog; surface a stub.
			apps = append(apps, CatalogApp{
				ID:   e.Name,
				Name: e.Name,
				Description: fmt.Sprintf("Failed to read manifest: %v", err),
			})
			continue
		}

		entry := CatalogApp{
			ID:           e.Name,
			Name:         stringOr(m.Name, e.Name),
			Description:  m.Description,
			Version:      m.AppVersion,
			Requirements: m.Requirements,
		}
		if asset, ok := assetByApp[e.Name]; ok {
			entry.AvailableForModel = true
			entry.DownloadURL = asset.url
			entry.AssetSize = asset.size
		}
		if v, ok := installed[e.Name]; ok {
			entry.Installed = true
			entry.InstalledVersion = v
		}
		apps = append(apps, entry)
	}

	return &Catalog{
		Release:    rel.TagName,
		FetchedAt:  time.Now().UTC(),
		Model:      model,
		AssetGroup: group,
		Apps:       apps,
		Notice:     notice,
	}, nil
}

// getCatalog returns a cached catalog if fresh, otherwise rebuilds.
func getCatalog(force bool) (*Catalog, error) {
	catalogMu.Lock()
	defer catalogMu.Unlock()

	if !force && catalogData != nil && time.Since(catalogStamp) < catalogCacheTTL {
		return catalogData, nil
	}
	c, err := rebuildCatalog()
	if err != nil {
		// If we have a stale cache, return it rather than nothing.
		if catalogData != nil {
			return catalogData, err
		}
		return nil, err
	}
	catalogData = c
	catalogStamp = time.Now()
	return c, nil
}

// --- HTTP handlers ---

func handleCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSONHeaders(w)
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	force := r.URL.Query().Get("refresh") == "1"
	c, err := getCatalog(force)
	if err != nil {
		// If we returned stale data alongside an error, include both.
		if c != nil {
			w.Header().Set("X-Catalog-Warning", err.Error())
			json.NewEncoder(w).Encode(c)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(c)
}

func handleCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	writeJSONHeaders(w)
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, err := getCatalog(true)
	if err != nil && c == nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(c)
}

// handleCatalogInstall handles POST /api/catalog/{id}/install. Downloads the
// matching SWU for the current model into /tmp and pipes it through
// install_swu (the same code path the USB-stick installer uses).
func handleCatalogInstall(w http.ResponseWriter, r *http.Request) {
	writeJSONHeaders(w)
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Path is /api/catalog/{id}/install
	rest := strings.TrimPrefix(r.URL.Path, "/api/catalog/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "install" || parts[0] == "" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	id := parts[0]
	if !isSafeKey(id) {
		http.Error(w, "Invalid app id", http.StatusBadRequest)
		return
	}

	// Find the catalog entry to discover the download URL for this model.
	c, err := getCatalog(false)
	if err != nil && c == nil {
		http.Error(w, "Catalog unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	var entry *CatalogApp
	for i := range c.Apps {
		if c.Apps[i].ID == id {
			entry = &c.Apps[i]
			break
		}
	}
	if entry == nil {
		http.Error(w, "Unknown app: "+id, http.StatusNotFound)
		return
	}
	if !entry.AvailableForModel || entry.DownloadURL == "" {
		http.Error(w, "No SWU available for this printer model", http.StatusUnprocessableEntity)
		return
	}

	// Download to /tmp/rinkhals-catalog/<id>.swu
	if err := os.MkdirAll(catalogDownloadTmp, 0755); err != nil {
		http.Error(w, "Failed to prepare download dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	swuPath := filepath.Join(catalogDownloadTmp, id+".swu")
	if err := downloadFile(entry.DownloadURL, swuPath); err != nil {
		http.Error(w, "Download failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer os.Remove(swuPath)

	// Pipe through install_swu (in tools.sh). This is the same call path the
	// USB-stick installer uses, so success here matches what users get today.
	cmd := fmt.Sprintf(". %s\ninstall_swu %s\n", toolsSh, shellQuote(swuPath))
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	resp := map[string]interface{}{
		"success": err == nil,
		"output":  string(out),
		"app":     id,
	}
	// Bust the catalog cache so a subsequent GET shows "installed: true".
	catalogMu.Lock()
	catalogData = nil
	catalogMu.Unlock()
	json.NewEncoder(w).Encode(resp)
}

func downloadFile(url, dst string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", catalogUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}

// Suppress unused-import warning until base64 ends up needed (will be used by
// future "preview README" endpoint that fetches README.md via the Contents API).
var _ = base64.StdEncoding
