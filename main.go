package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/playwright-community/playwright-go"
)

type Config struct {
	NumBrowsers         int
	TabsPerBrowser      int
	MaxRetries          int
	TimeoutSeconds      int
	MaxQueueSize        int
	ResultTTL           time.Duration
	BrowserRecycleAfter int
	APIKey              string
	ServerHost          string
	ServerPort          int
	TaskDBPath          string
	Headless            bool
}

type SolveTask struct {
	TaskID      string   `json:"task_id"`
	URL         string   `json:"url"`
	Sitekey     string   `json:"sitekey"`
	Proxy       *string  `json:"proxy,omitempty"`
	Action      *string  `json:"action,omitempty"`
	Cdata       *string  `json:"cdata,omitempty"`
	Status      string   `json:"status"`
	Token       *string  `json:"token,omitempty"`
	Error       *string  `json:"error,omitempty"`
	Attempts    int      `json:"attempts"`
	CreatedAt   float64  `json:"created_at"`
	CompletedAt *float64 `json:"completed_at,omitempty"`
}

type CreateTaskRequest struct {
	ClientKey string `json:"clientKey"`
	Task      Task   `json:"task"`
}

type Task struct {
	Type          string `json:"type"`
	WebsiteURL    string `json:"websiteURL"`
	WebsiteKey    string `json:"websiteKey"`
	Action        string `json:"action,omitempty"`
	Cdata         string `json:"cdata,omitempty"`
	Proxy         string `json:"proxy,omitempty"`
	ProxyURL      string `json:"proxyUrl,omitempty"`
	ProxyURLUpper string `json:"proxyURL,omitempty"`
}

type CreateTaskResponse struct {
	ErrorID int   `json:"errorId"`
	TaskID  int64 `json:"taskId"`
}

type GetTaskResultRequest struct {
	ClientKey string `json:"clientKey"`
	TaskID    int64  `json:"taskId"`
}

type GetTaskResultResponse struct {
	ErrorID  int       `json:"errorId"`
	Status   string    `json:"status,omitempty"`
	Solution *Solution `json:"solution,omitempty"`
}

type Solution struct {
	Token string `json:"token"`
}

type BrowserWorker struct {
	WorkerID   int
	BrowserID  int
	Browser    playwright.Browser
	Context    playwright.BrowserContext
	Page       playwright.Page
	IsRunning  bool
	SolveCount int
	TabReady   bool
	sync.Mutex
}

type JSONDatabase struct {
	Path  string
	TTL   time.Duration
	mu    sync.RWMutex
	Tasks map[string]*SolveTask
}

var (
	CONFIG Config

	taskQueue chan *SolveTask
	taskDB    *JSONDatabase

	stats = struct {
		Total   int
		Success int
		Failed  int
		sync.RWMutex
	}{}

	browserPool []*BrowserWorker
	pw          *playwright.Playwright
)

func getenvString(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		log.Printf("invalid int env %s=%q, using default %d", key, value, fallback)
		return fallback
	}

	return parsed
}

func getenvBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		log.Printf("invalid bool env %s=%q, using default %t", key, value, fallback)
		return fallback
	}

	return parsed
}

func loadConfig() {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: failed to load .env: %v", err)
	}

	resultTTLSeconds := getenvInt("RESULT_TTL_SECONDS", 1800)

	CONFIG = Config{
		NumBrowsers:         getenvInt("NUM_BROWSERS", 2),
		TabsPerBrowser:      getenvInt("TABS_PER_BROWSER", 10),
		MaxRetries:          getenvInt("MAX_RETRIES", 1),
		TimeoutSeconds:      getenvInt("TIMEOUT_SECONDS", 30),
		MaxQueueSize:        getenvInt("MAX_QUEUE_SIZE", 3000),
		ResultTTL:           time.Duration(resultTTLSeconds) * time.Second,
		BrowserRecycleAfter: getenvInt("BROWSER_RECYCLE_AFTER", 100),
		APIKey:              getenvString("API_KEY", "TEST_API_KEY_12345"),
		ServerHost:          getenvString("SERVER_HOST", "0.0.0.0"),
		ServerPort:          getenvInt("SERVER_PORT", 5073),
		TaskDBPath:          getenvString("TASK_DB_PATH", "tasks.json"),
		Headless:            getenvBool("HEADLESS", false),
	}
}

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneTask(task *SolveTask) *SolveTask {
	if task == nil {
		return nil
	}

	cloned := *task
	cloned.Proxy = cloneStringPtr(task.Proxy)
	cloned.Action = cloneStringPtr(task.Action)
	cloned.Cdata = cloneStringPtr(task.Cdata)
	cloned.Token = cloneStringPtr(task.Token)
	cloned.Error = cloneStringPtr(task.Error)
	if task.CompletedAt != nil {
		completedAt := *task.CompletedAt
		cloned.CompletedAt = &completedAt
	}
	return &cloned
}

func isTaskExpired(task *SolveTask, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 || task == nil {
		return false
	}

	reference := task.CreatedAt
	if task.CompletedAt != nil {
		reference = *task.CompletedAt
	}
	return now.Sub(time.Unix(0, int64(reference*1e9))) > ttl
}

func NewJSONDatabase(path string, ttl time.Duration) (*JSONDatabase, error) {
	db := &JSONDatabase{
		Path:  path,
		TTL:   ttl,
		Tasks: map[string]*SolveTask{},
	}
	if err := db.loadFromDisk(); err != nil {
		return nil, err
	}
	return db, nil
}

func (db *JSONDatabase) loadFromDisk() error {
	data, err := os.ReadFile(db.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read task database: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}

	loadedTasks := map[string]*SolveTask{}
	if err := json.Unmarshal(data, &loadedTasks); err != nil {
		return fmt.Errorf("parse task database: %w", err)
	}

	now := time.Now()
	for taskID, task := range loadedTasks {
		if isTaskExpired(task, now, db.TTL) {
			continue
		}
		db.Tasks[taskID] = task
	}
	return nil
}

func (db *JSONDatabase) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(db.Path), 0o755); err != nil {
		return fmt.Errorf("create task database directory: %w", err)
	}

	data, err := json.MarshalIndent(db.Tasks, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize task database: %w", err)
	}
	if err := os.WriteFile(db.Path, data, 0o644); err != nil {
		return fmt.Errorf("write task database: %w", err)
	}
	return nil
}

func (db *JSONDatabase) cleanupExpiredLocked(now time.Time) bool {
	if db.TTL <= 0 {
		return false
	}

	changed := false
	for taskID, task := range db.Tasks {
		if isTaskExpired(task, now, db.TTL) {
			delete(db.Tasks, taskID)
			changed = true
		}
	}
	return changed
}

func (db *JSONDatabase) SaveTask(task *SolveTask) {
	db.mu.Lock()
	defer db.mu.Unlock()

	_ = db.cleanupExpiredLocked(time.Now())
	db.Tasks[task.TaskID] = cloneTask(task)
	if err := db.flushLocked(); err != nil {
		log.Printf("failed to persist task %s: %v", task.TaskID, err)
	}
}

func (db *JSONDatabase) GetTask(taskID string) *SolveTask {
	db.mu.Lock()
	defer db.mu.Unlock()

	changed := db.cleanupExpiredLocked(time.Now())
	task, ok := db.Tasks[taskID]
	if !ok {
		if changed {
			_ = db.flushLocked()
		}
		return nil
	}

	if changed {
		_ = db.flushLocked()
	}
	return cloneTask(task)
}

func initTaskDatabase() error {
	var err error
	taskDB, err = NewJSONDatabase(CONFIG.TaskDBPath, CONFIG.ResultTTL)
	if err != nil {
		return err
	}
	log.Printf("task database ready at %s", CONFIG.TaskDBPath)
	return nil
}

func defaultLaunchArgs() []string {
	return []string{
		"--disable-blink-features=AutomationControlled",
		"--no-sandbox",
		"--disable-setuid-sandbox",
		"--disable-dev-shm-usage",
		"--disable-extensions",
		"--disable-plugins",
		"--disable-gpu",
		"--disable-software-rasterizer",
		"--disable-background-networking",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-breakpad",
		"--disable-component-extensions-with-background-pages",
		"--disable-features=TranslateUI,BlinkGenPropertyTrees",
		"--disable-ipc-flooding-protection",
		"--disable-renderer-backgrounding",
	}
}

func buildContextOptions() playwright.BrowserNewContextOptions {
	return playwright.BrowserNewContextOptions{
		UserAgent: playwright.String("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"),
		ExtraHttpHeaders: map[string]string{
			"sec-ch-ua": `"Google Chrome";v="137", "Chromium";v="137", "Not/A)Brand";v="24"`,
		},
		Viewport: &playwright.Size{Width: 200, Height: 40},
	}
}

func parseProxyConfig(rawProxy string) (*playwright.Proxy, error) {
	proxy := strings.TrimSpace(rawProxy)
	if proxy == "" {
		return nil, nil
	}

	if !strings.Contains(proxy, "://") {
		proxy = "http://" + proxy
	}

	parsed, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("proxy must include scheme and host")
	}

	config := &playwright.Proxy{
		Server: fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host),
	}

	if parsed.User != nil {
		username := parsed.User.Username()
		if username != "" {
			config.Username = playwright.String(username)
		}
		if password, ok := parsed.User.Password(); ok {
			config.Password = playwright.String(password)
		}
	}

	return config, nil
}

func launchBrowser(proxyURL string) (playwright.Browser, error) {
	options := playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(CONFIG.Headless),
		Args:     defaultLaunchArgs(),
	}

	proxyConfig, err := parseProxyConfig(proxyURL)
	if err != nil {
		return nil, err
	}
	if proxyConfig != nil {
		options.Proxy = proxyConfig
	}

	return pw.Chromium.Launch(options)
}

func (w *BrowserWorker) initialize() error {
	contextOptions := buildContextOptions()
	var err error
	w.Context, err = w.Browser.NewContext(contextOptions)
	if err != nil {
		return err
	}

	w.Page, err = w.Context.NewPage()
	if err != nil {
		return err
	}

	_, _ = w.Page.Goto("about:blank")
	w.TabReady = true
	return nil
}

func (w *BrowserWorker) recreateTab() {
	w.Lock()
	defer w.Unlock()

	if w.Page != nil {
		_ = w.Page.Close()
		w.Page = nil
	}

	var err error
	w.Page, err = w.Context.NewPage()
	if err != nil {
		log.Printf("failed to recreate page for worker %d: %v", w.WorkerID, err)
		return
	}

	_, _ = w.Page.Goto("about:blank")
	w.TabReady = true
}

func (w *BrowserWorker) recycle() {
	w.Lock()
	defer w.Unlock()

	if w.Page != nil {
		_ = w.Page.Close()
	}
	if w.Context != nil {
		_ = w.Context.Close()
	}

	if err := w.initialize(); err != nil {
		log.Printf("failed to recycle worker %d: %v", w.WorkerID, err)
		return
	}
	w.SolveCount = 0
}

func (w *BrowserWorker) cleanup() {
	w.Lock()
	defer w.Unlock()

	if w.Page != nil {
		_ = w.Page.Close()
	}
	if w.Context != nil {
		_ = w.Context.Close()
	}
}

func jsString(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func injectCaptchaOverlay(page playwright.Page, sitekey string, action string, cdata string) error {
	actionScript := ""
	if action != "" {
		actionScript = fmt.Sprintf("captchaDiv.setAttribute('data-action', %s);", jsString(action))
	}

	cdataScript := ""
	if cdata != "" {
		cdataScript = fmt.Sprintf("captchaDiv.setAttribute('data-cdata', %s);", jsString(cdata))
	}

	script := fmt.Sprintf(`
		if (!document.querySelector('#captcha-overlay')) {
			const overlay = document.createElement('div');
			overlay.id = 'captcha-overlay';
			overlay.style.cssText = 'position:fixed;top:0;left:0;width:100vw;height:100vh;background:#000;display:flex;justify-content:center;align-items:center;z-index:999999';
			const captchaDiv = document.createElement('div');
			captchaDiv.className = 'cf-turnstile';
			captchaDiv.setAttribute('data-sitekey', %s);
			%s
			%s
			overlay.appendChild(captchaDiv);
			document.body.appendChild(overlay);
			if (!window.turnstileScriptLoaded) {
				const script = document.createElement('script');
				script.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js';
				script.async = true;
				document.head.appendChild(script);
				window.turnstileScriptLoaded = true;
			}
		}
	`, jsString(sitekey), actionScript, cdataScript)

	_, err := page.Evaluate(script)
	return err
}

func setTaskFailed(task *SolveTask, message string) {
	task.Status = "failed"
	task.Error = &message
	task.Token = nil
}

func solveTaskOnPage(page playwright.Page, task *SolveTask) bool {
	_, err := page.Goto(task.URL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(float64(CONFIG.TimeoutSeconds * 1000)),
	})
	if err != nil {
		setTaskFailed(task, fmt.Sprintf("navigation error: %v", err))
		return false
	}

	action := ""
	if task.Action != nil {
		action = *task.Action
	}
	cdata := ""
	if task.Cdata != nil {
		cdata = *task.Cdata
	}

	if err := injectCaptchaOverlay(page, task.Sitekey, action, cdata); err != nil {
		setTaskFailed(task, fmt.Sprintf("overlay error: %v", err))
		return false
	}

	maxAttempts := CONFIG.TimeoutSeconds * 200
	if maxAttempts < 200 {
		maxAttempts = 200
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		tokenResult, evalErr := page.Evaluate(`() => {
			const input = document.querySelector('input[name="cf-turnstile-response"]');
			return input?.value?.length > 100 ? input.value : null;
		}`)

		if evalErr == nil {
			if token, ok := tokenResult.(string); ok && len(token) > 100 {
				task.Status = "success"
				task.Error = nil
				task.Token = &token
				return true
			}
		}

		if attempt%40 == 10 {
			_ = page.Click(".cf-turnstile", playwright.PageClickOptions{Timeout: playwright.Float(500)})
		}
		time.Sleep(5 * time.Millisecond)
	}

	setTaskFailed(task, "timeout")
	return false
}

func taskProxy(task *SolveTask) string {
	if task == nil || task.Proxy == nil {
		return ""
	}
	return strings.TrimSpace(*task.Proxy)
}

func taskUsesProxy(task *SolveTask) bool {
	return taskProxy(task) != ""
}

func solveTurnstileWithProxy(task *SolveTask) bool {
	proxyURL := taskProxy(task)
	browser, err := launchBrowser(proxyURL)
	if err != nil {
		setTaskFailed(task, fmt.Sprintf("proxy launch error: %v", err))
		return false
	}
	defer func() {
		_ = browser.Close()
	}()

	contextOptions := buildContextOptions()
	context, err := browser.NewContext(contextOptions)
	if err != nil {
		setTaskFailed(task, fmt.Sprintf("proxy context error: %v", err))
		return false
	}
	defer func() {
		_ = context.Close()
	}()

	page, err := context.NewPage()
	if err != nil {
		setTaskFailed(task, fmt.Sprintf("proxy page error: %v", err))
		return false
	}
	defer func() {
		_ = page.Close()
	}()

	return solveTaskOnPage(page, task)
}

func (w *BrowserWorker) solveTurnstile(task *SolveTask) bool {
	w.Lock()
	defer w.Unlock()

	if !w.TabReady || w.Page == nil {
		setTaskFailed(task, "tab not ready")
		return false
	}

	success := solveTaskOnPage(w.Page, task)
	if success {
		w.SolveCount++
	}

	if w.Page != nil {
		_ = w.Page.Close()
		w.Page = nil
	}
	w.TabReady = false
	return success
}

func workerLoop(worker *BrowserWorker) {
	consecutiveFailures := 0

	for worker.IsRunning {
		select {
		case task := <-taskQueue:
			useProxy := taskUsesProxy(task)

			if !useProxy && !worker.TabReady {
				select {
				case taskQueue <- task:
				default:
				}
				worker.recreateTab()
				continue
			}

			task.Status = "solving"
			task.Attempts++
			saveTaskToDB(task)

			var success bool
			if useProxy {
				success = solveTurnstileWithProxy(task)
			} else {
				success = worker.solveTurnstile(task)
			}

			if success {
				stats.Lock()
				stats.Success++
				stats.Unlock()
				if !useProxy {
					consecutiveFailures = 0
				}
			} else {
				stats.Lock()
				stats.Failed++
				stats.Unlock()
				if !useProxy {
					consecutiveFailures++
				}

				if task.Attempts < CONFIG.MaxRetries {
					task.Status = "pending"
					task.Error = nil
					task.Token = nil
					saveTaskToDB(task)
					select {
					case taskQueue <- task:
					default:
					}
					continue
				}
			}

			now := float64(time.Now().UnixNano()) / 1e9
			task.CompletedAt = &now
			saveTaskToDB(task)

			if !useProxy {
				worker.recreateTab()
				if consecutiveFailures > 10 || worker.SolveCount >= CONFIG.BrowserRecycleAfter {
					worker.recycle()
					consecutiveFailures = 0
				}
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func saveTaskToDB(task *SolveTask) {
	taskDB.SaveTask(task)
}

func getTaskFromDB(taskID string) *SolveTask {
	return taskDB.GetTask(taskID)
}

func initializeBrowserPool() error {
	var err error
	pw, err = playwright.Run()
	if err != nil {
		return fmt.Errorf("could not start playwright: %v", err)
	}

	totalTabs := CONFIG.NumBrowsers * CONFIG.TabsPerBrowser
	log.Printf("initializing %d browser(s) with %d tabs each (%d workers)", CONFIG.NumBrowsers, CONFIG.TabsPerBrowser, totalTabs)

	workerID := 1
	for browserID := 1; browserID <= CONFIG.NumBrowsers; browserID++ {
		browser, browserErr := launchBrowser("")
		if browserErr != nil {
			return fmt.Errorf("could not launch browser %d: %v", browserID, browserErr)
		}

		for tabNum := 0; tabNum < CONFIG.TabsPerBrowser; tabNum++ {
			worker := &BrowserWorker{
				WorkerID:  workerID,
				BrowserID: browserID,
				Browser:   browser,
				IsRunning: true,
			}
			if initErr := worker.initialize(); initErr != nil {
				log.Printf("failed to initialize worker %d: %v", workerID, initErr)
			}
			browserPool = append(browserPool, worker)
			workerID++
		}
	}

	log.Printf("browser pool ready: %d worker(s)", len(browserPool))
	return nil
}

func startWorkers() {
	for _, worker := range browserPool {
		go workerLoop(worker)
	}
	log.Printf("started %d workers", len(browserPool))
}

func cleanupBrowserPool() {
	log.Println("cleaning up browser pool")

	seenBrowsers := map[playwright.Browser]bool{}
	for _, worker := range browserPool {
		worker.IsRunning = false
		worker.cleanup()
		seenBrowsers[worker.Browser] = true
	}
	for browser := range seenBrowsers {
		_ = browser.Close()
	}

	if pw != nil {
		_ = pw.Stop()
	}
}

func validateAPIKey(clientKey string) bool {
	if CONFIG.APIKey == "" || CONFIG.APIKey == "YOUR_API_KEY_HERE" {
		return true
	}
	return clientKey == CONFIG.APIKey
}

func extractProxyValue(task Task) *string {
	for _, candidate := range []string{task.ProxyURLUpper, task.ProxyURL, task.Proxy} {
		trimmed := strings.TrimSpace(candidate)
		if trimmed != "" {
			return &trimmed
		}
	}
	return nil
}

func createTaskHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req CreateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		_ = json.NewEncoder(w).Encode(CreateTaskResponse{ErrorID: 1})
		return
	}

	if !validateAPIKey(req.ClientKey) {
		_ = json.NewEncoder(w).Encode(CreateTaskResponse{ErrorID: 1})
		return
	}

	if req.Task.Type != "TurnstileTask" || req.Task.WebsiteURL == "" || req.Task.WebsiteKey == "" {
		_ = json.NewEncoder(w).Encode(CreateTaskResponse{ErrorID: 1})
		return
	}

	taskID := time.Now().UnixNano() / 1e6
	var action, cdata *string
	if req.Task.Action != "" {
		action = &req.Task.Action
	}
	if req.Task.Cdata != "" {
		cdata = &req.Task.Cdata
	}

	task := &SolveTask{
		TaskID:    fmt.Sprintf("%d", taskID),
		URL:       req.Task.WebsiteURL,
		Sitekey:   req.Task.WebsiteKey,
		Proxy:     extractProxyValue(req.Task),
		Action:    action,
		Cdata:     cdata,
		Status:    "pending",
		CreatedAt: float64(time.Now().UnixNano()) / 1e9,
	}

	saveTaskToDB(task)

	select {
	case taskQueue <- task:
		stats.Lock()
		stats.Total++
		stats.Unlock()
		_ = json.NewEncoder(w).Encode(CreateTaskResponse{
			ErrorID: 0,
			TaskID:  taskID,
		})
	default:
		_ = json.NewEncoder(w).Encode(CreateTaskResponse{ErrorID: 1})
	}
}

func getTaskResultHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req GetTaskResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		_ = json.NewEncoder(w).Encode(GetTaskResultResponse{ErrorID: 1})
		return
	}

	if !validateAPIKey(req.ClientKey) {
		_ = json.NewEncoder(w).Encode(GetTaskResultResponse{ErrorID: 1})
		return
	}

	task := getTaskFromDB(fmt.Sprintf("%d", req.TaskID))
	if task == nil {
		_ = json.NewEncoder(w).Encode(GetTaskResultResponse{ErrorID: 1})
		return
	}

	if task.Status == "pending" || task.Status == "solving" {
		_ = json.NewEncoder(w).Encode(GetTaskResultResponse{
			ErrorID: 0,
			Status:  "processing",
		})
		return
	}

	if task.Status == "failed" || task.Token == nil {
		_ = json.NewEncoder(w).Encode(GetTaskResultResponse{ErrorID: 1})
		return
	}

	_ = json.NewEncoder(w).Encode(GetTaskResultResponse{
		ErrorID: 0,
		Status:  "ready",
		Solution: &Solution{
			Token: *task.Token,
		},
	})
}

func turnstileHandler(w http.ResponseWriter, r *http.Request) {
	taskURL := r.URL.Query().Get("url")
	sitekey := r.URL.Query().Get("sitekey")
	actionStr := r.URL.Query().Get("action")
	cdataStr := r.URL.Query().Get("cdata")
	proxyStr := strings.TrimSpace(r.URL.Query().Get("proxy"))
	if proxyStr == "" {
		proxyStr = strings.TrimSpace(r.URL.Query().Get("proxy_url"))
	}

	w.Header().Set("Content-Type", "application/json")

	if taskURL == "" || sitekey == "" {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errorId":          1,
			"errorCode":        "ERROR_WRONG_PAGEURL",
			"errorDescription": "Both 'url' and 'sitekey' are required",
		})
		return
	}

	taskID := uuid.New().String()
	var action, cdata, proxy *string
	if actionStr != "" {
		action = &actionStr
	}
	if cdataStr != "" {
		cdata = &cdataStr
	}
	if proxyStr != "" {
		proxy = &proxyStr
	}

	task := &SolveTask{
		TaskID:    taskID,
		URL:       taskURL,
		Sitekey:   sitekey,
		Proxy:     proxy,
		Action:    action,
		Cdata:     cdata,
		Status:    "pending",
		CreatedAt: float64(time.Now().UnixNano()) / 1e9,
	}

	saveTaskToDB(task)

	select {
	case taskQueue <- task:
		stats.Lock()
		stats.Total++
		stats.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errorId": 0,
			"taskId":  taskID,
		})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errorId":          1,
			"errorCode":        "ERROR_UNKNOWN",
			"errorDescription": "Task queue is full",
		})
	}
}

func resultHandler(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("id")
	w.Header().Set("Content-Type", "application/json")

	if taskID == "" {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errorId":          1,
			"errorCode":        "ERROR_WRONG_CAPTCHA_ID",
			"errorDescription": "Invalid task ID/Request parameter",
		})
		return
	}

	task := getTaskFromDB(taskID)
	if task == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errorId":          1,
			"errorCode":        "ERROR_CAPTCHA_UNSOLVABLE",
			"errorDescription": "Task not found",
		})
		return
	}

	if task.Status == "pending" || task.Status == "solving" {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "processing",
		})
		return
	}

	if task.Status == "failed" || task.Token == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errorId":          1,
			"errorCode":        "ERROR_CAPTCHA_UNSOLVABLE",
			"errorDescription": "Workers could not solve the captcha",
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"errorId": 0,
		"status":  "ready",
		"solution": map[string]any{
			"token": *task.Token,
		},
	})
}

func indexHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, "%s", `
		<!DOCTYPE html>
		<html lang="en">
		<head>
			<meta charset="UTF-8">
			<meta name="viewport" content="width=device-width, initial-scale=1.0">
			<title>Turnstile Solver API</title>
			<script src="https://cdn.tailwindcss.com"></script>
		</head>
		<body class="bg-gray-900 text-gray-200 min-h-screen flex items-center justify-center">
			<div class="bg-gray-800 p-8 rounded-lg shadow-md max-w-2xl w-full border border-green-500">
				<h1 class="text-3xl font-bold mb-6 text-center text-green-500">Turnstile Solver API</h1>

				<p class="mb-4 text-gray-300">Legacy endpoint:</p>
				<div class="bg-gray-700 p-4 rounded-lg mb-6 border border-green-500">
					<code class="text-sm break-all text-green-300">/turnstile?url=https://example.com&sitekey=sitekey&proxy=http://user:pass@host:port</code>
				</div>

				<p class="text-gray-300">
					Configuration is loaded from <code class="bg-green-700 text-white px-2 py-1 rounded">.env</code>.
					Tasks are persisted to a JSON file at startup path configured via <code class="bg-green-700 text-white px-2 py-1 rounded">TASK_DB_PATH</code>.
				</p>
			</div>
		</body>
		</html>
	`)
}

func main() {
	loadConfig()

	fmt.Println(strings.Repeat("=", 70))
	fmt.Println("Turnstile Solver - JSON DB + .env config")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("Browsers: %d\n", CONFIG.NumBrowsers)
	fmt.Printf("Tabs per Browser: %d\n", CONFIG.TabsPerBrowser)
	fmt.Printf("Total Workers: %d\n", CONFIG.NumBrowsers*CONFIG.TabsPerBrowser)
	fmt.Printf("Max Retries: %d\n", CONFIG.MaxRetries)
	fmt.Printf("Timeout: %ds\n", CONFIG.TimeoutSeconds)
	fmt.Printf("Max Queue Size: %d\n", CONFIG.MaxQueueSize)
	fmt.Printf("Task DB Path: %s\n", CONFIG.TaskDBPath)
	fmt.Printf("Result TTL: %s\n", CONFIG.ResultTTL)
	fmt.Printf("Headless: %t\n", CONFIG.Headless)
	fmt.Println(strings.Repeat("=", 70))

	taskQueue = make(chan *SolveTask, CONFIG.MaxQueueSize)

	if err := initTaskDatabase(); err != nil {
		log.Fatalf("failed to initialize JSON task database: %v", err)
	}

	if err := initializeBrowserPool(); err != nil {
		log.Fatalf("fatal error initializing browser pool: %v", err)
	}
	defer cleanupBrowserPool()

	startWorkers()

	mux := http.NewServeMux()
	mux.HandleFunc("/createTask", createTaskHandler)
	mux.HandleFunc("/getTaskResult", getTaskResultHandler)
	mux.HandleFunc("/turnstile", turnstileHandler)
	mux.HandleFunc("/result", resultHandler)
	mux.HandleFunc("/", indexHandler)

	listenAddr := fmt.Sprintf("%s:%d", CONFIG.ServerHost, CONFIG.ServerPort)
	fmt.Printf("starting server on http://%s\n", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
