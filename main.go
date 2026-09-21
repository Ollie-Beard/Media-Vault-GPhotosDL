// Package main implements gphotosdl
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const (
	program    = "gphotosdl"
	gphotosURL = "https://photos.google.com/"
	loginURL   = "https://photos.google.com/login"
)

// Flags
var (
	debug   = flag.Bool("debug", false, "set to see debug messages")
	login   = flag.Bool("login", false, "set to launch login browser")
	show    = flag.Bool("show", false, "set to show the browser (not headless)")
	addr    = flag.String("addr", "localhost:8282", "address for the web server")
	useJSON = flag.Bool("json", false, "log in JSON format")
)

// Global variables
var (
	configRoot    string
	browserConfig string
	browserPath   string
	downloadDir   string
	browserPrefs  string
	version       = "DEV"
	commit        = "NONE"
	date          = "UNKNOWN"
)

// Remove the download directory and contents.
func removeDownloadDirectory() {
	if downloadDir == "" {
		return
	}

	err := os.RemoveAll(downloadDir)
	if err == nil {
		slog.Debug("Removed download directory")
	} else {
		slog.Error("Failed to remove download directory", "err", err)
	}
}

// Set up global variables from flags.
func config() error {
	versionString := fmt.Sprintf(
		"%s version %s, commit %s, built at %s",
		program,
		version,
		commit,
		date,
	)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n%s\n", versionString)
	}

	flag.Parse()

	// Set up logging.
	level := slog.LevelInfo

	if *debug {
		level = slog.LevelDebug
	}

	if *useJSON {
		logger := slog.New(
			slog.NewJSONHandler(
				os.Stderr,
				&slog.HandlerOptions{Level: level},
			),
		)

		slog.SetDefault(logger)
	} else {
		slog.SetLogLoggerLevel(level)
	}

	slog.Debug(versionString)

	// Configuration directory.
	var err error

	if configuredRoot := strings.TrimSpace(os.Getenv("GPHOTOSDL_CONFIG_DIR")); configuredRoot != "" {
		configRoot = filepath.Clean(configuredRoot)
	} else {
		configRoot, err = os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("didn't find config directory: %w", err)
		}
		configRoot = filepath.Join(configRoot, program)
	}

	browserConfig = filepath.Join(configRoot, "browser")

	err = os.MkdirAll(browserConfig, 0700)
	if err != nil {
		return fmt.Errorf("config directory creation: %w", err)
	}

	// Persistent download directory. Preserve the existing Windows location,
	// while allowing Docker/Linux to provide an explicit persistent path.
	if configuredDownloadDir := strings.TrimSpace(os.Getenv("GPHOTOSDL_DOWNLOAD_DIR")); configuredDownloadDir != "" {
		downloadDir = filepath.Clean(configuredDownloadDir)
	} else if userProfile := strings.TrimSpace(os.Getenv("USERPROFILE")); userProfile != "" {
		downloadDir = filepath.Join(userProfile, "Downloads", "gphotosdl")
	} else {
		downloadDir = filepath.Join(configRoot, "downloads")
	}

	err = os.MkdirAll(downloadDir, 0755)
	if err != nil {
		return fmt.Errorf("download directory creation: %w", err)
	}

	slog.Debug(
		"Configured config",
		"config_root", configRoot,
		"browser_config", browserConfig,
		"download_directory", downloadDir,
	)

	// Find Chrome/Chromium. An explicit path is useful in Docker, while normal
	// Windows installs continue to use Rod's existing browser discovery.
	browserPath = strings.TrimSpace(os.Getenv("GPHOTOSDL_BROWSER_PATH"))
	if browserPath == "" {
		var ok bool
		browserPath, ok = launcher.LookPath()
		if !ok && runtime.GOOS == "linux" {
			for _, candidate := range []string{
				"/usr/bin/chromium",
				"/usr/bin/chromium-browser",
				"/usr/bin/google-chrome-stable",
				"/usr/bin/google-chrome",
			} {
				if _, statErr := os.Stat(candidate); statErr == nil {
					browserPath = candidate
					ok = true
					break
				}
			}
		}
		if !ok {
			return errors.New("browser not found")
		}
	} else if _, err := os.Stat(browserPath); err != nil {
		return fmt.Errorf("configured browser not found at %q: %w", browserPath, err)
	}

	slog.Debug("Found browser", "browser_path", browserPath)

	// Configure Chrome downloads to go into our temporary directory.
	pref := map[string]any{
		"download": map[string]any{
			"default_directory":   downloadDir,
			"prompt_for_download": false,
		},
	}

	prefJSON, err := json.Marshal(pref)
	if err != nil {
		return fmt.Errorf("failed to make browser preferences: %w", err)
	}

	browserPrefs = string(prefJSON)

	slog.Debug("Made browser preferences", "prefs", browserPrefs)

	return nil
}

// logger forwards Rod/Chrome logging through slog.
type logger struct{}

func (logger) Write(p []byte) (n int, err error) {
	s := strings.TrimSpace(string(p))
	if s != "" {
		slog.Debug(s)
	}

	return len(p), nil
}

func (logger) Println(vs ...any) {
	s := strings.TrimSpace(fmt.Sprint(vs...))
	if s != "" {
		slog.Debug(s)
	}
}

// DownloadStatus tracks the active state of an asynchronous download.
type DownloadStatus struct {
	ID                  string   `json:"id"`
	Filename            string   `json:"filename"`
	Status              string   `json:"status"` // "starting", "downloading", "complete", "failed", "cancelled"
	BytesDownloaded     int64    `json:"bytes_downloaded,omitempty"`
	TotalBytes          int64    `json:"total_bytes,omitempty"`
	Progress            float64  `json:"progress,omitempty"`
	SpeedBytesPerSecond float64  `json:"speed_bytes_per_second,omitempty"`
	SpeedMBPerSecond    float64  `json:"speed_mb_per_second,omitempty"`
	ETASeconds          *float64 `json:"eta_seconds,omitempty"`
	Error               string   `json:"error,omitempty"`

	path            string             `json:"-"`
	cancel          context.CancelFunc `json:"-"`
	lastSampleAt    time.Time          `json:"-"`
	lastSampleBytes int64              `json:"-"`
}

// Gphotos is a browser controller for Google Photos.
type Gphotos struct {
	browser *rod.Browser

	// Only one UI interaction at a time for now.
	mu sync.Mutex

	// Protects the downloads map
	downloadsMu sync.RWMutex
	downloads   map[string]*DownloadStatus
}

// New creates the browser and starts the HTTP server.
func New() (*Gphotos, error) {
	g := &Gphotos{
		downloads: make(map[string]*DownloadStatus),
	}

	if err := g.startBrowser(); err != nil {
		return nil, err
	}

	if err := g.startServer(); err != nil {
		g.Close()
		return nil, err
	}

	return g, nil
}

// cleanupStaleBrowserLocks removes Chromium profile lock files left behind
// after an unclean shutdown. The browser profile is dedicated to GPhotosDL,
// so these files can be safely removed when no Chromium process is using the
// profile. Cookies, Login Data, and the rest of the profile are never removed.
func cleanupStaleBrowserLocks() {
	if runtime.GOOS != "linux" || strings.TrimSpace(os.Getenv("GPHOTOSDL_CONTAINER")) == "" {
		return
	}

	if browserProfileInUse() {
		slog.Debug("Chromium profile is currently in use; keeping profile locks", "browser_config", browserConfig)
		return
	}

	lockFiles := []string{
		filepath.Join(browserConfig, "SingletonLock"),
		filepath.Join(browserConfig, "SingletonCookie"),
		filepath.Join(browserConfig, "SingletonSocket"),
		filepath.Join(browserConfig, "Default", "LOCK"),
	}

	for _, path := range lockFiles {
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				slog.Debug("Could not remove stale Chromium profile lock", "path", path, "err", err)
			}
			continue
		}

		slog.Info("Removed stale Chromium profile lock", "path", path)
	}
}

// browserProfileInUse checks Linux /proc entries for a Chromium process using
// this exact GPhotosDL browser profile. This prevents us from deleting a real
// lock if another Chromium process is legitimately using the profile.
func browserProfileInUse() bool {
	if runtime.GOOS != "linux" {
		return false
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		slog.Debug("Could not inspect /proc for Chromium profile usage", "err", err)
		return false
	}

	target := "--user-data-dir=" + browserConfig

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}

		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}

		if strings.Contains(string(cmdline), target) {
			slog.Debug("Found Chromium process using GPhotosDL profile", "pid", entry.Name())
			return true
		}
	}

	return false
}

// googlePhotosURLIsAuthenticated recognises the normal authenticated Google
// Photos URL without requiring an exact string match. This tolerates harmless
// query strings/fragments while still rejecting Google login/account URLs.
func googlePhotosURLIsAuthenticated(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	return strings.EqualFold(u.Scheme, "https") &&
		strings.EqualFold(u.Host, "photos.google.com") &&
		strings.TrimRight(u.Path, "/") == ""
}

// startBrowser starts Chrome and checks authentication.
func (g *Gphotos) startBrowser() error {
	cleanupStaleBrowserLocks()

	l := launcher.New().
		Bin(browserPath).
		Headless(!*show).
		UserDataDir(browserConfig).
		Preferences(browserPrefs).
		Set("disable-gpu").
		Set("disable-audio-output").
		Logger(logger{})

		// Chromium in the Linux/Docker container needs a persistent,
	// non-OS password store so encrypted Google session data remains
	// usable across Chromium restarts.
	if runtime.GOOS == "linux" && strings.TrimSpace(os.Getenv("GPHOTOSDL_CONTAINER")) != "" {
		l.Set("password-store", "basic")
		// Alpine Docker does not provide a persistent Linux keyring. Disable
		// Chromium cookie encryption for this dedicated GPhotosDL profile so
		// the Google session survives Chromium process/container restarts.
		// This is Linux/Docker-only; Windows keeps its existing behaviour.
		l.Set("disable-cookie-encryption")
	}

	// These flags are required by Chromium in a typical Linux/Docker
	// container. Do not add them to the normal Windows browser launch.
	if runtime.GOOS == "linux" && strings.TrimSpace(os.Getenv("GPHOTOSDL_CONTAINER")) != "" {
		l.Set("no-sandbox")
		l.Set("disable-setuid-sandbox")
		l.Set("disable-dev-shm-usage")
	}

	url, err := l.Launch()
	if err != nil {
		return fmt.Errorf("browser launch: %w", err)
	}

	g.browser = rod.New().
		ControlURL(url).
		NoDefaultDevice().
		Logger(logger{})

	err = g.browser.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to browser: %w", err)
	}

	authPage, err := g.browser.Page(
		proto.TargetCreateTarget{
			URL: gphotosURL,
		},
	)
	if err != nil {
		g.Close()
		return fmt.Errorf("couldn't open Google Photos URL: %w", err)
	}

	defer func() {
		if err := authPage.Close(); err != nil {
			slog.Debug("Failed to close startup authentication page", "err", err)
		}
	}()

	eventCallback := func(e *proto.PageLifecycleEvent) {
		slog.Debug("Page lifecycle event", "name", e.Name)
	}

	authPage.EachEvent(eventCallback)

	err = authPage.WaitLoad()
	if err != nil {
		g.Close()
		return fmt.Errorf("Google Photos page load: %w", err)
	}

	authenticated := false

	for try := 0; try < 60; try++ {
		time.Sleep(time.Second)

		info := authPage.MustInfo()

		slog.Debug("URL", "url", info.URL)

		if googlePhotosURLIsAuthenticated(info.URL) {
			authenticated = true
			slog.Debug("Authenticated")
			break
		}

		slog.Debug(
			"Waiting for Google Photos authentication",
			"attempt",
			try+1,
		)
	}

	if !authenticated {
		g.Close()
		return errors.New(
			"browser is not logged in - rerun with the -login flag",
		)
	}

	slog.Debug("Startup authentication page verified and will now close")

	// Remove any old cancelled/intercepted entries from the dedicated browser
	// profile without touching the actual files in downloadDir.
	clearChromeDownloadHistory(g, slog.Default())

	return nil
}

// startServer starts the local HTTP API.
func (g *Gphotos) startServer() error {
	http.HandleFunc("GET /", g.getRoot)
	http.HandleFunc("GET /api/downloads", g.getAllDownloadStatuses)
	http.HandleFunc("GET /id/{photoID}", g.getID)
	http.HandleFunc("POST /download", g.postDownload)
	http.HandleFunc("GET /download/{id}", g.getDownloadStatus)
	http.HandleFunc("DELETE /download/{id}", g.cancelDownload)

	go func() {
		err := http.ListenAndServe(*addr, nil)

		if errors.Is(err, http.ErrServerClosed) {
			slog.Debug("Web server closed")
		} else if err != nil {
			slog.Error("Error starting web server", "err", err)
			os.Exit(1)
		}
	}()

	slog.Info("HTTP server started", "address", "http://"+*addr)

	return nil
}

// Serve the local dashboard.
func (g *Gphotos) getRoot(w http.ResponseWriter, r *http.Request) {
	slog.Debug("Got GET / request")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>gphotosdl dashboard</title>
<style>
:root { color-scheme: dark; --bg:#0b1020; --panel:#151c2f; --panel2:#1b2540; --text:#eef3ff; --muted:#9aa8c7; --accent:#6ea8fe; --green:#4ade80; --red:#fb7185; --yellow:#fbbf24; --border:#2b3858; }
* { box-sizing:border-box; }
body { margin:0; font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; background:linear-gradient(135deg,#090d18,#111a30); color:var(--text); min-height:100vh; }
main { width:min(1050px,calc(100% - 32px)); margin:0 auto; padding:32px 0 50px; }
h1 { margin:0 0 6px; font-size:32px; }
.subtitle { margin:0 0 28px; color:var(--muted); }
.panel { background:rgba(21,28,47,.94); border:1px solid var(--border); border-radius:16px; padding:20px; box-shadow:0 12px 40px rgba(0,0,0,.2); }
.toolbar { display:flex; justify-content:space-between; align-items:center; gap:12px; margin-bottom:18px; }
.toolbar h2 { margin:0; font-size:20px; }
button { border:0; border-radius:9px; padding:9px 13px; font-weight:650; cursor:pointer; color:#08111f; background:var(--accent); }
button.secondary { color:var(--text); background:var(--panel2); border:1px solid var(--border); }
button.danger { color:white; background:var(--red); }
button:disabled { opacity:.5; cursor:not-allowed; }
.download { border:1px solid var(--border); background:#10182b; border-radius:14px; padding:17px; margin-top:12px; }
.top { display:flex; align-items:flex-start; justify-content:space-between; gap:16px; }
.name { font-size:18px; font-weight:700; overflow-wrap:anywhere; }
.id { color:var(--muted); font-family:ui-monospace,SFMono-Regular,Consolas,monospace; font-size:12px; margin-top:5px; }
.actions { display:flex; gap:7px; flex-wrap:wrap; justify-content:flex-end; }
.meta { display:flex; gap:14px; flex-wrap:wrap; margin:14px 0 9px; color:var(--muted); font-size:13px; }
.progress-track { height:14px; border-radius:999px; overflow:hidden; background:#27324d; }
.progress-bar { height:100%; width:0%; border-radius:inherit; background:linear-gradient(90deg,#4ade80,#22c55e); transition:width .35s ease; }
.progress-row { display:flex; justify-content:space-between; margin-top:7px; font-size:13px; }
.status { font-weight:700; text-transform:capitalize; }
.status.downloading { color:var(--green); }
.status.complete { color:var(--green); }
.status.failed, .status.cancelled { color:var(--red); }
.status.starting { color:var(--yellow); }
.error { margin-top:9px; color:#fda4af; font-size:13px; overflow-wrap:anywhere; }
.empty { color:var(--muted); text-align:center; padding:30px 10px; }
.hint { color:var(--muted); font-size:12px; margin-top:18px; }
@media (max-width:650px) { .top { flex-direction:column; } .actions { justify-content:flex-start; } main { width:min(100% - 18px,1050px); padding-top:20px; } }
</style>
</head>
<body>
<main>
<h1>Google Photos Media Downloader</h1>
<p class="subtitle">Google Photos download dashboard &mdash; <strong>http://localhost:8282</strong></p>
<section class="panel">
<div class="toolbar"><h2>Downloads</h2><button class="secondary" onclick="fetchDownloads()">Refresh</button></div>
<div id="downloads"><div class="empty">Loading downloads...</div></div>
<p class="hint">The list refreshes automatically every second. Use <strong>Copy ID</strong> if you need the download ID for the API, or <strong>Cancel</strong> to stop an active download.</p>
</section>
</main>
<script>
function esc(value) { const d=document.createElement('div'); d.textContent=value ?? ''; return d.innerHTML; }
function formatBytes(n) { if (!n || n < 0) return '0 B'; const units=['B','KB','MB','GB','TB']; let i=0, x=Number(n); while(x>=1024 && i<units.length-1){x/=1024;i++;} return (i===0?x.toFixed(0):x.toFixed(2))+' '+units[i]; }
function statusClass(s) { return String(s||'').toLowerCase(); }
function canCancel(s) { return s==='starting' || s==='downloading'; }

function render(items) {
  const root=document.getElementById('downloads');
  if (!items.length) { root.innerHTML='<div class="empty">No downloads yet.</div>'; return; }
  
  root.innerHTML = items.map(d => {
    const progress = Number.isFinite(Number(d.progress)) ? Math.max(0, Math.min(100, Number(d.progress))) : 0;
    const total = d.total_bytes ? formatBytes(d.total_bytes) : 'Unknown size';
    const done = d.bytes_downloaded ? formatBytes(d.bytes_downloaded) : '0 B';
    const err = d.error ? '<div class="error">' + esc(d.error) + '</div>' : '';
    
    const safeID = esc(d.id);
    const cancel = canCancel(d.status) ? 
        '<button class="danger" onclick="cancelDownload(\'' + safeID + '\')">Cancel</button>' : '';
        
    return '<article class="download">' +
      '<div class="top">' +
        '<div><div class="name">' + esc(d.filename) + '</div><div class="id">ID: ' + esc(d.id) + '</div></div>' +
        '<div class="actions"><button class="secondary" onclick="copyID(\'' + safeID + '\')">Copy ID</button>' + cancel + '</div>' +
      '</div>' +
      '<div class="meta"><span class="status ' + statusClass(d.status) + '">' + esc(d.status) + '</span><span>' + done + ' / ' + total + '</span></div>' +
      '<div class="progress-track"><div class="progress-bar" style="width:' + progress + '%"></div></div>' +
      '<div class="progress-row"><span>' + progress.toFixed(2) + '%</span><span>' + esc(d.id) + '</span></div>' + err +
      '</article>';
  }).join('');
}

async function fetchDownloads() {
  try { 
      const res = await fetch('/api/downloads', {cache: 'no-store'}); 
      if(!res.ok) throw new Error(await res.text()); 
      const data = await res.json(); 
      render(data); 
  } catch(e) { 
      document.getElementById('downloads').innerHTML = '<div class="empty">Could not load downloads: ' + esc(e.message) + '</div>'; 
  }
}

async function copyID(id) { 
    try { 
        await navigator.clipboard.writeText(id); 
        alert('Copied download ID: ' + id); 
    } catch(e) { 
        prompt('Copy this download ID:', id); 
    } 
}

async function cancelDownload(id) {
  if (!confirm('Cancel this download?')) return;
  try { 
      const res = await fetch('/download/' + encodeURIComponent(id), {method: 'DELETE'}); 
      if(!res.ok) throw new Error(await res.text()); 
      await fetchDownloads(); 
  } catch(e) { 
      alert('Failed to cancel download: ' + e.message); 
  }
}

fetchDownloads();
setInterval(fetchDownloads, 1000);
</script>
</body>
</html>
`)
}

// getAllDownloadStatuses returns all known asynchronous downloads for the dashboard.
func (g *Gphotos) getAllDownloadStatuses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	g.downloadsMu.RLock()
	items := make([]DownloadStatus, 0, len(g.downloads))
	for _, status := range g.downloads {
		items = append(items, *status)
	}
	g.downloadsMu.RUnlock()

	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(items); err != nil {
		slog.Error("Failed to encode download list", "err", err)
	}
}

// Existing photo-ID endpoint.
func (g *Gphotos) getID(w http.ResponseWriter, r *http.Request) {
	photoID := r.PathValue("photoID")

	slog.Info("Got photo request", "id", photoID)

	path, err := g.DownloadPhotoID(photoID)
	if err != nil {
		slog.Error("Download failed", "id", photoID, "err", err)
		var h httpError
		if errors.As(err, &h) {
			http.Error(w, h.Error(), int(h))
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	slog.Info("Downloaded photo", "id", photoID, "path", path)
	http.ServeFile(w, r, path)
}

// DownloadRequest is the JSON request accepted by POST /download.
type DownloadRequest struct {
	URL       string `json:"url"`
	Filename  string `json:"filename"`
	Directory string `json:"directory,omitempty"`
}

// postDownload handles the initiation of async download requests.
func (g *Gphotos) postDownload(w http.ResponseWriter, r *http.Request) {
	slog.Info("Got POST /download request")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var request DownloadRequest

	err := json.NewDecoder(r.Body).Decode(&request)
	if err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	request.URL = strings.TrimSpace(request.URL)
	request.Filename = strings.TrimSpace(request.Filename)

	if request.URL == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}

	// FIX 1: Pass user-requested filename through sanitizeFilename so it fixes colons / question marks
	displayFilename := request.Filename
	if displayFilename != "" {
		displayFilename = sanitizeFilename(displayFilename)
	} else {
		displayFilename = "Fetching filename..."
	}

	id := fmt.Sprintf("dl-%d", time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())

	status := &DownloadStatus{
		ID:       id,
		Filename: displayFilename,
		Status:   "starting",
		cancel:   cancel,
	}

	g.downloadsMu.Lock()
	g.downloads[id] = status
	g.downloadsMu.Unlock()

	// Launch in background
	go g.runDownloadAsync(ctx, id, request.URL, request.Filename, request.Directory)

	w.Header().Set("Content-Type", "application/json")
	response := map[string]any{
		"success":  true,
		"id":       id,
		"filename": displayFilename,
		"status":   "starting",
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		slog.Error("Failed to write JSON response", "err", err)
	}
}

// getDownloadStatus retrieves the current progress of a download ID.
func (g *Gphotos) getDownloadStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	g.downloadsMu.RLock()
	status, ok := g.downloads[id]
	if !ok {
		g.downloadsMu.RUnlock()
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Make a copy to safely encode to JSON without holding the lock for IO
	statusCopy := *status
	g.downloadsMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statusCopy)
}

// cancelDownload aborts an active download using its context.
func (g *Gphotos) cancelDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := r.PathValue("id")

	g.downloadsMu.Lock()
	status, ok := g.downloads[id]
	g.downloadsMu.Unlock()

	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if status.cancel != nil {
		status.cancel()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"id":      id,
		"status":  "cancelled",
	})
}

// runDownloadAsync manages the lifecycle of the download goroutine.
func (g *Gphotos) runDownloadAsync(ctx context.Context, id string, shareURL string, filename string, directory string) {
	slog.Info("Starting async download", "id", id, "url", shareURL, "filename", filename)

	path, err := g.DownloadShareURLWithContext(ctx, id, shareURL, filename, directory)

	g.downloadsMu.Lock()
	defer g.downloadsMu.Unlock()

	status := g.downloads[id]

	if err != nil {
		if errors.Is(err, context.Canceled) {
			status.Status = "cancelled"
			slog.Info("Download cancelled via API", "id", id)
		} else {
			status.Status = "failed"
			status.Error = err.Error()
			slog.Error("Async download failed", "id", id, "err", err)
		}
	} else {
		status.Status = "complete"
		status.path = path
		status.Progress = 100.0
		status.ETASeconds = nil
		slog.Info("Async download complete", "id", id, "path", path)
	}
}

// progressReader wraps an io.Reader to track download progress safely.
type progressReader struct {
	reader io.Reader
	status *DownloadStatus
	g      *Gphotos
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.reader.Read(p)
	if n > 0 {
		pr.g.downloadsMu.Lock()
		pr.status.BytesDownloaded += int64(n)

		now := time.Now()
		if pr.status.lastSampleAt.IsZero() {
			pr.status.lastSampleAt = now
			pr.status.lastSampleBytes = pr.status.BytesDownloaded
		} else {
			elapsed := now.Sub(pr.status.lastSampleAt).Seconds()
			if elapsed >= 0.25 {
				deltaBytes := pr.status.BytesDownloaded - pr.status.lastSampleBytes
				if deltaBytes >= 0 {
					instantaneous := float64(deltaBytes) / elapsed
					if pr.status.SpeedBytesPerSecond <= 0 {
						pr.status.SpeedBytesPerSecond = instantaneous
					} else {
						// Smooth the displayed speed so the UI/ETA does not jump
						// wildly when Google delivers data in uneven chunks.
						pr.status.SpeedBytesPerSecond =
							(pr.status.SpeedBytesPerSecond * 0.7) + (instantaneous * 0.3)
					}
					pr.status.SpeedMBPerSecond = pr.status.SpeedBytesPerSecond / (1024 * 1024)
					if pr.status.TotalBytes > pr.status.BytesDownloaded && pr.status.SpeedBytesPerSecond > 0 {
						eta := float64(pr.status.TotalBytes-pr.status.BytesDownloaded) / pr.status.SpeedBytesPerSecond
						pr.status.ETASeconds = &eta
					} else {
						pr.status.ETASeconds = nil
					}
				}
				pr.status.lastSampleAt = now
				pr.status.lastSampleBytes = pr.status.BytesDownloaded
			}
		}

		if pr.status.TotalBytes > 0 {
			pr.status.Progress = math.Round((float64(pr.status.BytesDownloaded)/float64(pr.status.TotalBytes))*10000) / 100
		}
		pr.g.downloadsMu.Unlock()
	}
	return
}

// DownloadShareURLWithContext executes the download with context cancellation support.
func (g *Gphotos) DownloadShareURLWithContext(
	ctx context.Context,
	id string,
	shareURL string,
	filename string,
	directory string,
) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	slog := slog.With("id", id, "url", shareURL, "filename", filename)

	page, err := g.browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return "", fmt.Errorf("failed to open browser tab: %w", err)
	}

	// Bind the page to our cancellation context. If cancelled, automation stops.
	page = page.Context(ctx)

	pageClosed := false
	closeDownloadPage := func() {
		if pageClosed || page == nil {
			return
		}
		if err := page.Close(); err != nil && !errors.Is(err, context.Canceled) {
			slog.Debug("Failed to close download tab", "err", err)
		}
		pageClosed = true
	}
	defer closeDownloadPage()

	slog.Debug("Navigating to Google Photos share URL")
	if err := page.Navigate(shareURL); err != nil {
		return "", fmt.Errorf("failed to navigate to share URL: %w", err)
	}

	slog.Debug("Waiting for Google Photos share page to load")
	if err := page.WaitLoad(); err != nil {
		return "", fmt.Errorf("share page load failed: %w", err)
	}

	slog.Debug("Waiting for Google Photos UI")
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		return "", ctx.Err()
	}

	info := page.MustInfo()
	slog.Debug("Share page loaded", "url", info.URL, "title", info.Title)

	if err := openFirstSharedMedia(page); err != nil {
		return "", err
	}

	slog.Debug("Waiting for individual media viewer")
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Extract original filename with colons directly from Google Photos page document.title
	var pageFilename string
	if pageTitleRes, err := page.Eval(`() => {
		let t = document.title || "";
		if (t.includes(" - Google Photos")) {
			t = t.replace(" - Google Photos", "").trim();
			if (t && t !== "Google Photos") return t;
		}
		const sel = ['header div[role="heading"]', 'div[aria-label*="Details"]', '[data-title]'];
		for (const s of sel) {
			const el = document.querySelector(s);
			if (el) {
				const txt = el.getAttribute('aria-label') || el.getAttribute('data-filename') || el.innerText || '';
				if (txt && txt.includes('.')) return txt.trim();
			}
		}
		return "";
	}`); err == nil {
		pageFilename = pageTitleRes.Value.String()
	}

	if err := (proto.BrowserSetDownloadBehavior{
		Behavior:         proto.BrowserSetDownloadBehaviorBehaviorAllowAndName,
		BrowserContextID: g.browser.BrowserContextID,
		DownloadPath:     downloadDir,
		EventsEnabled:    true,
	}).Call(g.browser); err != nil {
		return "", fmt.Errorf("failed to configure Chrome download events: %w", err)
	}

	type downloadStart struct {
		info *proto.BrowserDownloadWillBegin
	}

	downloadCh := make(chan downloadStart, 1)
	waitDownloadEvent := g.browser.EachEvent(func(e *proto.BrowserDownloadWillBegin) bool {
		if !strings.Contains(e.URL, "googleusercontent.com") {
			return false
		}
		downloadCh <- downloadStart{info: e}
		return true
	})
	go waitDownloadEvent()

	slog.Info("Starting individual Google Photos media download")
	if err := page.KeyActions().Press(input.ShiftLeft).Type('D').Do(); err != nil {
		return "", fmt.Errorf("failed to send download keyboard shortcut: %w", err)
	}

	var start *proto.BrowserDownloadWillBegin
	select {
	case event := <-downloadCh:
		start = event.info
	case <-time.After(30 * time.Second):
		return "", errors.New("Google Photos did not expose an individual media download URL within 30 seconds")
	case <-ctx.Done():
		return "", ctx.Err()
	}

	if start == nil || start.URL == "" {
		return "", errors.New("Google Photos returned an empty media download URL")
	}

	slog.Debug(
		"Captured direct Google Photos download URL",
		"guid", start.GUID,
		"suggested_filename", start.SuggestedFilename,
		"url_prefix", truncateForLog(start.URL, 120),
	)

	// Determine final output filename (prefer explicitly provided, then page title with colons, fall back to server suggested)
	chosenFilename := filename
	if chosenFilename == "" && pageFilename != "" {
		chosenFilename = pageFilename
	} else if chosenFilename == "" && start.SuggestedFilename != "" {
		chosenFilename = start.SuggestedFilename
	}

	chosenFilename = sanitizeFilename(chosenFilename)
	if chosenFilename == "" || chosenFilename == "." {
		chosenFilename = fmt.Sprintf("gphotos_%d.media", time.Now().Unix())
	}

	// Update live status entry so dashboard displays the actual sanitized filename
	g.downloadsMu.Lock()
	if status, ok := g.downloads[id]; ok {
		status.Filename = chosenFilename
	}
	g.downloadsMu.Unlock()

	outputDir := downloadDir
	if strings.TrimSpace(directory) != "" {
		outputDir = filepath.Clean(directory)
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create requested output directory: %w", err)
		}
	}

	finalPath := filepath.Join(outputDir, chosenFilename)

	if err := (proto.BrowserCancelDownload{
		GUID:             start.GUID,
		BrowserContextID: g.browser.BrowserContextID,
	}).Call(g.browser); err != nil {
		slog.Debug("Could not cancel Chrome's temporary download", "err", err)
	}

	_ = os.Remove(filepath.Join(downloadDir, start.GUID))

	cookies, err := page.Cookies([]string{start.URL})
	if err != nil {
		return "", fmt.Errorf("failed to read browser cookies: %w", err)
	}

	userAgent := page.MustEval(`() => navigator.userAgent`).Str()

	client := &http.Client{
		Timeout: 0,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			req.Header.Set("User-Agent", userAgent)
			if info.URL != "" {
				req.Header.Set("Referer", info.URL)
			}
			return nil
		},
	}

	// Tie the HTTP request to our cancellation context
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, start.URL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create direct media request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	if info.URL != "" {
		req.Header.Set("Referer", info.URL)
	}
	if cookieHeader := cookiesToHeader(cookies); cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}

	if err := os.Remove(finalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("failed to remove existing output file: %w", err)
	}

	slog.Info("Downloading media directly", "path", finalPath)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("direct media download request failed: %w", err)
	}

	// At this point the direct Go HTTP download has successfully started and
	// we no longer need the Google Photos media viewer. Close the dedicated
	// media tab immediately so Chrome stops rendering/streaming the media.
	closeDownloadPage()

	// Chrome records the intercepted/cancelled temporary download in its
	// download history. This profile is dedicated to GPhotosDL, so clear that
	// history without touching the user's normal Chrome profile.
	clearChromeDownloadHistory(g, slog)

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("direct media download returned HTTP %s", resp.Status)
	}

	// Update tracker to downloading and record total bytes
	g.downloadsMu.Lock()
	status := g.downloads[id]
	status.Status = "downloading"
	status.TotalBytes = resp.ContentLength
	g.downloadsMu.Unlock()

	out, err := os.Create(finalPath)
	if err != nil {
		return "", fmt.Errorf("failed to create output file: %w", err)
	}

	keepFile := false
	defer func() {
		_ = out.Close()
		if !keepFile {
			_ = os.Remove(finalPath)
		}
	}()

	prefix := make([]byte, 4)
	n, err := io.ReadFull(resp.Body, prefix)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", fmt.Errorf("failed to read direct media response: %w", err)
	}
	if n >= 4 && bytes.Equal(prefix, []byte{'P', 'K', 0x03, 0x04}) {
		return "", errors.New("Google returned a ZIP archive instead of an individual media file")
	}

	// Construct tracking reader
	pr := &progressReader{
		reader: io.MultiReader(bytes.NewReader(prefix[:n]), resp.Body),
		status: status,
		g:      g,
	}

	written, err := io.Copy(out, pr)
	if err != nil {
		return "", fmt.Errorf("direct media download failed after %d bytes: %w", written, err)
	}

	if err := out.Close(); err != nil {
		return "", fmt.Errorf("failed to close downloaded media file: %w", err)
	}

	fi, err := os.Stat(finalPath)
	if err != nil {
		return "", fmt.Errorf("downloaded file cannot be stat'ed: %w", err)
	}

	if fi.Size() == 0 {
		return "", errors.New("Google returned an empty media file")
	}

	keepFile = true
	return finalPath, nil
}

// clearChromeDownloadHistory clears the downloads list in the dedicated GPhotosDL
// Chromium profile. It never touches the user's normal Chrome profile or the
// actual files in downloadDir.
//
// Chromium's downloads UI exposes the Clear all action through shadow DOM:
// downloads-manager -> downloads-toolbar -> #clearAll.
// This is intentionally best-effort so a future Chromium UI change cannot
// break an otherwise valid media download.
func clearChromeDownloadHistory(g *Gphotos, logger *slog.Logger) {
	if g == nil || g.browser == nil {
		return
	}

	cleanupPage, err := g.browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		logger.Debug("Could not open Chrome downloads cleanup tab", "err", err)
		return
	}

	defer func() {
		if err := cleanupPage.Close(); err != nil && !errors.Is(err, context.Canceled) {
			logger.Debug("Could not close Chrome downloads cleanup tab", "err", err)
		}
	}()

	if err := cleanupPage.Navigate("chrome://downloads/"); err != nil {
		logger.Debug("Could not open chrome://downloads/ for cleanup", "err", err)
		return
	}

	// chrome://downloads/ is a Chromium WebUI. The document can finish loading
	// before downloads-manager and its shadow DOM have been attached, so poll
	// briefly instead of relying on WaitLoad alone.
	for attempt := 0; attempt < 30; attempt++ {
		result, evalErr := cleanupPage.Eval(`() => {
			const manager = document.querySelector('downloads-manager');
			if (!manager || !manager.shadowRoot) return false;

			const toolbar = manager.shadowRoot.querySelector('downloads-toolbar');
			if (!toolbar || !toolbar.shadowRoot) return false;

			const clearAll = toolbar.shadowRoot.querySelector('#clearAll');
			if (!clearAll || clearAll.disabled) return false;

			clearAll.click();
			return true;
		}`)

		var cleared bool
		if evalErr == nil {
			if err := result.Value.Unmarshal(&cleared); err == nil && cleared {
				logger.Debug("Cleared Chrome download history")
				return
			}
		}

		time.Sleep(100 * time.Millisecond)
	}

	logger.Debug("Chrome download history cleanup was not available")
}

// openFirstSharedMedia opens the first media item in a Google Photos shared album.
func openFirstSharedMedia(page *rod.Page) error {
	if el, err := page.Element(`a[href*="/photo/"]`); err == nil && el != nil {
		slog.Debug("Found first shared-album media item via photo link")

		href, err := el.Attribute("href")
		if err != nil {
			slog.Debug("Failed to read photo link href", "err", err)
		} else if href != nil && *href != "" {
			targetURL := *href

			// Google Photos currently exposes these links as values such as:
			//   ./share/AF1Qip.../photo/AF1Qip...?key=...
			//
			// That "./share/..." path is intended to point at the Google Photos
			// origin, not to be resolved relative to the current /share/... URL.
			// Resolving it against the current URL would incorrectly produce:
			//   /share/share/...
			//
			// Normalise the Google Photos link to an absolute URL explicitly.
			if strings.HasPrefix(targetURL, "./") {
				targetURL = strings.TrimPrefix(targetURL, ".")
			}

			if strings.HasPrefix(targetURL, "/") {
				currentURL := page.MustInfo().URL
				base, parseErr := url.Parse(currentURL)
				if parseErr != nil {
					return fmt.Errorf("failed to parse current Google Photos URL: %w", parseErr)
				}

				target, parseErr := url.Parse(targetURL)
				if parseErr != nil {
					return fmt.Errorf("failed to parse Google Photos media href %q: %w", targetURL, parseErr)
				}

				targetURL = (&url.URL{
					Scheme:   base.Scheme,
					Host:     base.Host,
					Path:     target.Path,
					RawPath:  target.RawPath,
					RawQuery: target.RawQuery,
					Fragment: target.Fragment,
				}).String()
			} else if !strings.HasPrefix(targetURL, "http://") &&
				!strings.HasPrefix(targetURL, "https://") {
				currentURL := page.MustInfo().URL
				base, parseErr := url.Parse(currentURL)
				if parseErr != nil {
					return fmt.Errorf("failed to parse current Google Photos URL: %w", parseErr)
				}

				targetURL = (&url.URL{
					Scheme: base.Scheme,
					Host:   base.Host,
					Path:   "/" + strings.TrimPrefix(targetURL, "/"),
				}).String()
			}

			slog.Debug(
				"Navigating directly to first shared-album media item",
				"url", targetURL,
			)

			if err := page.Navigate(targetURL); err != nil {
				return fmt.Errorf("failed to open first shared-album media item: %w", err)
			}

			return nil
		}
	}

	// Fallback: try clicking a suitable visible Google-hosted image directly
	// from JavaScript. This is only reached if the photo anchor did not give
	// us a usable href.
	js := `() => {
		const imgs = Array.from(document.images).filter(img => {
			const r = img.getBoundingClientRect();
			const src = img.currentSrc || img.src || '';

			return r.width >= 120 &&
				r.height >= 120 &&
				r.bottom > 0 &&
				r.right > 0 &&
				getComputedStyle(img).visibility !== 'hidden' &&
				getComputedStyle(img).display !== 'none' &&
				src.includes('googleusercontent.com');
		});

		if (!imgs.length) {
			return false;
		}

		imgs[0].click();
		return true;
	}`

	if result := page.MustEval(js); result.Bool() {
		slog.Debug("Opened first shared-album media item via image fallback")
		return nil
	}

	return errors.New("could not open the first media item in the Google Photos shared album")
}

// isDash returns true for standard ASCII hyphens and all Unicode dash/minus variants.
func isDash(r rune) bool {
	switch r {
	case '-', '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2212', '\uFF0D':
		return true
	default:
		return false
	}
}

// isSpace returns true for standard ASCII spaces, non-breaking spaces, and Unicode whitespace.
func isSpace(r rune) bool {
	return r == ' ' || r == '\u00A0' || unicode.IsSpace(r)
}

// sanitizeFilename converts a requested filename into a Windows-safe filename.
// It repairs Chromium/Google Photos colon sanitisation patterns ('Wars- Episode', 'Wars - Episode', or 'Wars– Episode'),
// maps standard colons to '꞉' (U+A789), literal question marks to '꞉', and cleans forbidden Windows path characters.
func sanitizeFilename(filename string) string {
	if unescaped, err := url.PathUnescape(filename); err == nil {
		filename = unescaped
	}

	filename = strings.TrimSpace(filepath.Base(filename))
	runes := []rune(filename)
	var builder strings.Builder

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		// Map explicit standard colons directly to U+A789
		if r == ':' {
			builder.WriteRune('\uA789')
			continue
		}

		// FIX 2: Handle literal question marks (like 'Star Wars? Episode VIII') by mapping them to U+A789 or cleaning
		if r == '?' {
			hasSpaceAfter := (i+1 < len(runes) && isSpace(runes[i+1]))
			hasSpaceBefore := (i > 0 && isSpace(runes[i-1]))
			if hasSpaceAfter || hasSpaceBefore || (i+1 < len(runes) && unicode.IsUpper(runes[i+1])) {
				builder.WriteRune('\uA789')
				continue
			}
		}

		// Detect colon replacement patterns created by Chrome/Google Photos
		if isDash(r) {
			hasSpaceAfter := (i+1 < len(runes) && isSpace(runes[i+1]))
			hasSpaceBefore := (i > 0 && isSpace(runes[i-1]))

			// Pattern 1: [non-space][dash][space] e.g. "Wars- Episode"
			if hasSpaceAfter && !hasSpaceBefore {
				builder.WriteRune('\uA789')
				continue
			}

			// Pattern 2: [space][dash][space] when followed by key subtitle/episode markers e.g. "Wars - Episode"
			if hasSpaceAfter && hasSpaceBefore {
				rest := strings.ToLower(string(runes[i+1:]))
				restTrimmed := strings.TrimSpace(rest)
				if strings.HasPrefix(restTrimmed, "episode") ||
					strings.HasPrefix(restTrimmed, "part") ||
					strings.HasPrefix(restTrimmed, "chapter") ||
					strings.HasPrefix(restTrimmed, "volume") ||
					strings.HasPrefix(restTrimmed, "vol") ||
					strings.HasPrefix(restTrimmed, "season") {
					buf := builder.String()
					if strings.HasSuffix(buf, " ") {
						builder.Reset()
						builder.WriteString(strings.TrimRight(buf, " "))
					}
					builder.WriteRune('\uA789')
					continue
				}
			}
		}

		builder.WriteRune(r)
	}

	filename = builder.String()

	// Filter forbidden remaining Windows filename characters (< > " / \ | *) - question mark handled above
	filename = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', '"', '/', '\\', '|', '*':
			return '-'
		case '?':
			return '\uA789'
		case 0:
			return -1
		default:
			return r
		}
	}, filename)

	filename = strings.TrimRight(filename, " .")
	return filename
}

func cookiesToHeader(cookies []*proto.NetworkCookie) string {
	parts := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie == nil || cookie.Name == "" {
			continue
		}
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(parts, "; ")
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// waitForFile waits until a downloaded file exists and its size stops changing.
func waitForFile(
	path string,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	var lastSize int64 = -1
	stableCount := 0

	for time.Now().Before(deadline) {
		fi, err := os.Stat(path)
		if err == nil {
			size := fi.Size()
			if size > 0 && size == lastSize {
				stableCount++
				if stableCount >= 2 {
					return nil
				}
			} else {
				stableCount = 0
			}
			lastSize = size
		}
		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf(
		"download did not become stable within %s: %s",
		timeout,
		path,
	)
}

// DownloadPhotoID retains the original gphotosdl-style download path.
func (g *Gphotos) DownloadPhotoID(photoID string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	url := "https://photos.google.com/lr/photo/" + photoID
	slog := slog.With("id", photoID)

	page, err := g.browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return "", fmt.Errorf("failed to open browser tab for photo %q: %w", photoID, err)
	}
	defer func() {
		if err := page.Close(); err != nil {
			slog.Error("Error closing tab", "err", err)
		}
	}()

	slog.Debug("Navigate to photo URL")
	err = page.Navigate(url)
	if err != nil {
		return "", fmt.Errorf("failed to navigate to photo %q: %w", photoID, err)
	}

	slog.Debug("Wait for page to load")
	err = page.WaitLoad()
	if err != nil {
		return "", fmt.Errorf("photo page load failed: %w", err)
	}

	time.Sleep(time.Second)

	waitDownload := g.browser.WaitDownload(downloadDir)

	slog.Debug("Sending Shift+D")
	err = page.KeyActions().Press(input.ShiftLeft).Type('D').Do()
	if err != nil {
		return "", fmt.Errorf("failed to send download shortcut: %w", err)
	}

	slog.Debug("Wait for download")
	info := waitDownload()
	if info == nil {
		return "", errors.New("Google Photos did not produce a download")
	}

	path := filepath.Join(downloadDir, info.GUID)

	if err := waitForFile(path, 30*time.Second); err != nil {
		return "", err
	}

	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}

	slog.Debug("Download successful", "size", fi.Size(), "path", path)

	// This path also creates a Chromium download-history entry. Clear the
	// browser history without removing the downloaded file.
	clearChromeDownloadHistory(g, slog)

	return path, nil
}

// Close the browser.
func (g *Gphotos) Close() {
	if g.browser == nil {
		return
	}

	err := g.browser.Close()
	if err == nil {
		slog.Debug("Closed browser")
	} else {
		slog.Error("Failed to close browser", "err", err)
	}
}

func main() {
	err := config()
	if err != nil {
		slog.Error("Configuration failed", "err", err)
		os.Exit(2)
	}

	if *login {
		slog.Info(
			"Log in to Google with the browser that pops up, close it, then re-run this without the -login flag",
		)

		loginArgs := []string{
			"--user-data-dir=" + browserConfig,
		}

		// The one-time Docker/Linux GUI login must use the same cookie storage
		// mode as normal GPhotosDL startup. Windows is deliberately unchanged.
		if runtime.GOOS == "linux" && strings.TrimSpace(os.Getenv("GPHOTOSDL_CONTAINER")) != "" {
			loginArgs = append(loginArgs,
				"--no-sandbox",
				"--disable-setuid-sandbox",
				"--disable-dev-shm-usage",
				"--password-store=basic",
				"--disable-cookie-encryption",
			)
		}

		loginArgs = append(loginArgs, loginURL)

		cmd := exec.Command(browserPath, loginArgs...)

		err = cmd.Start()
		if err != nil {
			slog.Error(
				"Failed to start browser",
				"err",
				err,
			)

			os.Exit(2)
		}

		slog.Info("Waiting for browser to be closed")

		err = cmd.Wait()
		if err != nil {
			slog.Error(
				"Browser run failed",
				"err",
				err,
			)

			os.Exit(2)
		}

		slog.Info("Now restart this program without -login")

		os.Exit(0)
	}

	g, err := New()
	if err != nil {
		slog.Error("Failed to make browser", "err", err)
		os.Exit(2)
	}
	defer g.Close()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)

	slog.Info("Press CTRL-C (or kill) to quit")
	sig := <-quit
	slog.Info("Signal received - shutting down", "signal", sig)
}

// httpError wraps an HTTP status code.
type httpError int

func (h httpError) Error() string {
	return fmt.Sprintf("HTTP Error %d", h)
}
