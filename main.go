package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"time"
	
	"github.com/RoughCookiexx/gg_sse"
	"github.com/RoughCookiexx/gg_twitch_types"
	"github.com/RoughCookiexx/twitch_chat_subscriber"
)

// Define the AppConfig structure for applications to be managed
type AppConfig struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	Args      []string `json:"args"`
	HealthURL string   `json:"health_url"`
}

// Define the AppState structure to hold runtime information about each app
type AppState struct {
	Config        AppConfig     `json:"config"`
	Cmd           *exec.Cmd     `json:"-"` // Don't expose Cmd in JSON
	Running       bool          `json:"running"`
	HealthStatus  string        `json:"health_status"`
	HealthLastCheck time.Time   `json:"health_last_check"`
	OutputBuffer *bytes.Buffer `json:"-"` // Buffer to capture output
	OutputChan    chan string   `json:"-"` // Channel to stream output
}

// Manager struct holds all application states and provides control
type Manager struct {
	apps map[string]*AppState
	mu   sync.RWMutex
}

// NewManager creates and initializes a new Manager instance
func NewManager(configs []AppConfig) *Manager {
	m := &Manager{
		apps: make(map[string]*AppState),
	}
	for _, cfg := range configs {
		m.apps[cfg.Name] = &AppState{
			Config:        cfg,
			Running:       false,
			HealthStatus:  "Unknown",
			OutputBuffer:  new(bytes.Buffer),
			OutputChan:    make(chan string, 100), // Buffered channel for output
		}
	}
	return m
}

// StartApp starts a specified application
func (m *Manager) StartApp(appName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	app, ok := m.apps[appName]
	if !ok {
		return fmt.Errorf("app %s not found", appName)
	}
	if app.Running {
		return fmt.Errorf("app %s is already running", appName)
	}

	cmd := exec.Command(app.Config.Path, app.Config.Args...)

	// Capture stdout and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe for %s: %w", appName, err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe for %s: %w", appName, err)
	}

	// Combined output reader
	multiReader := io.MultiReader(stdoutPipe, stderrPipe)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start app %s: %w", appName, err)
	}

	app.Cmd = cmd
	app.Running = true
	app.OutputBuffer.Reset() // Clear buffer on restart

	// Goroutine to continuously read process output
	go func(appName string, reader io.Reader) {
		buf := make([]byte, 1024)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				line := string(buf[:n])
				m.mu.Lock()
				app.OutputBuffer.WriteString(line) // Write to buffer
				// Keep buffer size reasonable
				if app.OutputBuffer.Len() > 4096 {
					app.OutputBuffer = bytes.NewBuffer(app.OutputBuffer.Bytes()[app.OutputBuffer.Len()-2048:])
				}
				m.mu.Unlock()
				select {
				case app.OutputChan <- line: // Send to channel for streaming if needed
				default:
					// Drop if channel is full
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("Error reading output from %s: %v", appName, err)
				}
				break
			}
		}
	}(appName, multiReader)

	// Goroutine to wait for the process to exit
	go func(appName string, cmd *exec.Cmd) {
		err := cmd.Wait()
		m.mu.Lock()
		defer m.mu.Unlock()
		if app.Cmd == cmd { // Ensure it's the current command for this app
			app.Running = false
			app.Cmd = nil
			if err != nil {
				log.Printf("App %s exited with error: %v", appName, err)
				app.HealthStatus = fmt.Sprintf("Exited: %v", err)
			} else {
				log.Printf("App %s exited normally.", appName)
				app.HealthStatus = "Stopped"
			}
		}
	}(appName, cmd)

	log.Printf("Started app: %s", appName)
	return nil
}

// StopApp stops a specified application
func (m *Manager) StopApp(appName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	app, ok := m.apps[appName]
	if !ok {
		return fmt.Errorf("app %s not found", appName)
	}
	if !app.Running || app.Cmd == nil || app.Cmd.Process == nil {
		return fmt.Errorf("app %s is not running", appName)
	}

	if err := app.Cmd.Process.Kill(); err != nil {
		return fmt.Errorf("failed to kill app %s: %w", appName, err)
	}
	app.Running = false
	app.HealthStatus = "Stopped"
	app.Cmd = nil // Clear command reference
	log.Printf("Stopped app: %s", appName)
	return nil
}

// CheckAppHealth performs a health check on a specific app's HealthURL
func (m *Manager) CheckAppHealth(app *AppState) {
	if app.Config.HealthURL == "" {
		m.mu.Lock()
		app.HealthStatus = "N/A"
		app.HealthLastCheck = time.Now()
		m.mu.Unlock()
		return
	}

	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(app.Config.HealthURL)

	m.mu.Lock()
	defer m.mu.Unlock()

	app.HealthLastCheck = time.Now()
	if err != nil {
		app.HealthStatus = fmt.Sprintf("Error: %v", err)
		log.Printf("Health check for %s failed: %v", app.Config.Name, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		app.HealthStatus = "Healthy"
	} else {
		app.HealthStatus = fmt.Sprintf("Degraded (%d)", resp.StatusCode)
	}
	log.Printf("Health check for %s: %s (Status: %d)", app.Config.Name, app.HealthStatus, resp.StatusCode)
}

// RunHealthChecks periodically runs health checks for all apps
func (m *Manager) RunHealthChecks(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		m.mu.RLock()
		appsToHealthCheck := []*AppState{}
		for _, app := range m.apps {
			appsToHealthCheck = append(appsToHealthCheck, app)
		}
		m.mu.RUnlock()

		for _, app := range appsToHealthCheck {
			if app.Running { // Only check health of running apps
				m.CheckAppHealth(app)
			} else {
				m.mu.Lock()
				app.HealthStatus = "Stopped"
				app.HealthLastCheck = time.Now()
				m.mu.Unlock()
			}
		}
	}
}

func handleMessage(message twitch_types.Message)(string) {
	json, _ := json.Marshal(message)
	bytes := []byte(json)
	sse.SendBytes(bytes)
	return ""
}

// getAppsHandler returns the JSON representation of all app states
func getAppsHandler(mgr *Manager, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	states := make([]AppState, 0, len(mgr.apps))
	for _, app := range mgr.apps {
		states = append(states, *app)
	}

	if err := json.NewEncoder(w).Encode(states); err != nil {
		http.Error(w, "Failed to encode app states", http.StatusInternalServerError)
		log.Printf("Error encoding app states: %v", err)
	}
}

// controlAppHandler handles start/stop requests for an app
func controlAppHandler(mgr *Manager, w http.ResponseWriter, r *http.Request) {
	appName := r.URL.Path[len("/api/app/"):] // Extract app name from URL
	var action string
	if r.Method == http.MethodPost {
		action = r.URL.Query().Get("action")
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var err error
	switch action {
	case "start":
		err = mgr.StartApp(appName)
	case "stop":
		err = mgr.StopApp(appName)
	default:
		http.Error(w, "Invalid action. Must be 'start' or 'stop'.", http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to %s app %s: %v", action, appName, err), http.StatusInternalServerError)
		log.Printf("Error during %s for app %s: %v", action, appName, err)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "{\"status\": \"success\", \"message\": \"%s app %s\"}", action, appName)
}

// getAppOutputHandler returns the last N lines of output for a given app
func getAppOutputHandler(mgr *Manager, w http.ResponseWriter, r *http.Request) {
	appName := r.URL.Path[len("/api/output/"):] // Extract app name from URL
	mgr.mu.RLock()
	app, ok := mgr.apps[appName]
	mgr.mu.RUnlock()

	if !ok {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	mgr.mu.RLock()
	output := app.OutputBuffer.String()
	mgr.mu.RUnlock()

	// Simple way to get last lines, could be improved
	lines := bytes.Split([]byte(output), []byte("\n"))
	numLines := len(lines)
	start := 0
	if numLines > 50 { // Limit to last 50 lines
		start = numLines - 50
	}

	// Join back relevant lines
	filteredOutput := bytes.Join(lines[start:], []byte("\n"))
	w.Write(filteredOutput)
}

func main() {
	log.Println("Starting Go App Manager...")

	// Define your applications here
	// Ensure that 'path' points to your compiled Go binaries.
	// For example, if you have 'my-go-app' in the same directory, use "./my-go-app"
	// Or a full path like "/usr/local/bin/my-go-app"
	// Replace "http://localhost:8081/health" with the actual health check URL for your apps.
	appConfigs := []AppConfig{
		{Name: "Cacaphony", Path: "/home/tommy/cacaphony/cacaphony", Args: []string{"--port", "6972"}, HealthURL: "http://127.0.0.1:6972/health"},
		{Name: "Heckler", Path: "/home/tommy/heckler/heckler", Args: []string{"--port", "6971"}, HealthURL: "http://127.0.0.1:6971/health"},
		{Name: "K Facts", Path: "/home/tommy/k_facts/k_facts", Args: []string{"--port", "6974"}, HealthURL: "http://127.0.0.1:6974/ping"},
		{Name: "Noise Machine", Path: "/home/tommy/noise_machine/noise_machine", Args: []string{"--port", "6976"}, HealthURL: "http://127.0.0.1:6976/health"},
		{Name: "Trombone", Path: "/home/tommy/trombone/trombone", Args: []string{"--port", "6973"}, HealthURL: "http://127.0.0.1:6973/health"},
	}

	mgr := NewManager(appConfigs)

	// Start health checking in a goroutine
	go mgr.RunHealthChecks(5 * time.Second)

	http.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) {
		getAppsHandler(mgr, w, r)
	})

	http.HandleFunc("/api/app/", func(w http.ResponseWriter, r *http.Request) {
		controlAppHandler(mgr, w, r)
	})

	http.HandleFunc("/api/output/", func(w http.ResponseWriter, r *http.Request) {
		getAppOutputHandler(mgr, w, r)
	})
	
	port := 6978
	subscriptionURL := "http://0.0.0.0:6969/subscribe"
	filterPattern := "PRIVMSG"
	twitch_chat_subscriber.SendRequestWithCallbackAndRegex(subscriptionURL, handleMessage, filterPattern, port)
	sse.Start()

	portStr := ":6978"
	log.Printf("App Manager listening on %d. Open http://localhost%s in your browser.", port, portStr)
	if err := http.ListenAndServe(portStr, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}

