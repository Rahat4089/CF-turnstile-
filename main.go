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
	"runtime"
	"runtime/debug"
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
	AuthToken           string
}

type SolveTask struct {
	TaskID      string            `json:"task_id"`
	URL         string            `json:"url"`
	Sitekey     string            `json:"sitekey"`
	Proxy       *string           `json:"proxy,omitempty"`
	Action      *string           `json:"action,omitempty"`
	Cdata       *string           `json:"cdata,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Cookies     []TaskCookie      `json:"cookies,omitempty"`
	Status      string            `json:"status"`
	Token       *string           `json:"token,omitempty"`
	Error       *string           `json:"error,omitempty"`
	Attempts    int               `json:"attempts"`
	CreatedAt   float64           `json:"created_at"`
	CompletedAt *float64          `json:"completed_at,omitempty"`
}

type TaskCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HttpOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite,omitempty"`
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
	Token     string            `json:"token"`
	Headers   map[string]string `json:"headers,omitempty"`
	Cookies   []TaskCookie      `json:"cookies,omitempty"`
	SolveTime *float64          `json:"solveTime,omitempty"`
}

type CloudflareProxy struct {
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	URL      string `json:"url,omitempty"`
}

type CloudflareRequest struct {
	Mode      string           `json:"mode"`
	Domain    string           `json:"domain,omitempty"`
	SiteKey   string           `json:"siteKey,omitempty"`
	Sitekey   string           `json:"sitekey,omitempty"`
	Action    string           `json:"action,omitempty"`
	Cdata     string           `json:"cdata,omitempty"`
	CData     string           `json:"cData,omitempty"`
	TTL       int64            `json:"ttl,omitempty"`
	Expire    int64            `json:"expire,omitempty"`
	AuthToken string           `json:"authToken,omitempty"`
	Proxy     *CloudflareProxy `json:"proxy,omitempty"`
	ProxyURL  string           `json:"proxyUrl,omitempty"`
	ProxyURL2 string           `json:"proxyURL,omitempty"`
	Task      *Task            `json:"task,omitempty"`
}

type iuamCacheEntry struct {
	ExpireAt time.Time
	Value    map[string]any
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

	iuamCache = struct {
		sync.RWMutex
		Entries map[string]iuamCacheEntry
	}{
		Entries: map[string]iuamCacheEntry{},
	}
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
		AuthToken:           getenvString("AUTH_TOKEN", ""),
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
	if task.Headers != nil {
		cloned.Headers = make(map[string]string, len(task.Headers))
		for key, value := range task.Headers {
			cloned.Headers[key] = value
		}
	}
	if task.Cookies != nil {
		cloned.Cookies = append([]TaskCookie(nil), task.Cookies...)
	}
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

func buildTurnstileSandboxHTML(sitekey string, action string, cdata string) string {
	actionConfig := ""
	if action != "" {
		actionConfig = fmt.Sprintf(", action: %s", jsString(action))
	}

	cdataConfig := ""
	if cdata != "" {
		cdataConfig = fmt.Sprintf(", cData: %s", jsString(cdata))
	}

	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Turnstile Solver</title>
  <style>
    html,body { margin:0; padding:0; width:100%%; height:100%%; background:#0b0f18; color:#fff; font-family:Arial,sans-serif; }
    .wrap { min-height:100%%; display:flex; align-items:center; justify-content:center; }
    .card { width:380px; padding:24px; border-radius:12px; background:#121826; border:1px solid #22314d; }
    #widget { min-height:70px; display:flex; align-items:center; justify-content:center; }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="card">
      <div id="widget"></div>
    </div>
  </div>
  <input type="hidden" id="cf-response" name="cf-turnstile-response" value="">
  <script>
    window.__turnstileToken = "";
    window.__turnstileError = "";
    window.__turnstileRendered = false;

    function setToken(token) {
      window.__turnstileToken = token || "";
      const input = document.getElementById("cf-response");
      if (input) input.value = window.__turnstileToken;
    }

    function renderTurnstile() {
      try {
        if (window.__turnstileRendered) return;
        if (!window.turnstile || typeof window.turnstile.render !== "function") return;

        window.turnstile.render("#widget", {
          sitekey: %s%s%s,
          callback: function(token) {
            setToken(token);
          },
          "error-callback": function(code) {
            window.__turnstileError = String(code || "unknown_error");
          },
          "timeout-callback": function() {
            window.__turnstileError = "interactive_timeout";
          }
        });
        window.__turnstileRendered = true;
      } catch (err) {
        window.__turnstileError = String(err && err.message ? err.message : err);
      }
    }

    window.onloadTurnstileCallback = function() {
      renderTurnstile();
    };
  </script>
  <script src="https://challenges.cloudflare.com/turnstile/v0/api.js?onload=onloadTurnstileCallback" async defer></script>
</body>
</html>`, jsString(sitekey), actionConfig, cdataConfig)
}

func shouldAllowTurnstileRequest(requestURL string) bool {
	normalized := strings.ToLower(strings.TrimSpace(requestURL))
	if normalized == "" {
		return false
	}

	allowedPrefixes := []string{
		"https://challenges.cloudflare.com/",
		"https://challenges.fed.cloudflare.com/",
	}
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

func loadTurnstileInTab(page playwright.Page, task *SolveTask) (map[string]string, error) {
	_ = page.UnrouteAll()

	action := ""
	if task.Action != nil {
		action = *task.Action
	}
	cdata := ""
	if task.Cdata != nil {
		cdata = *task.Cdata
	}
	html := buildTurnstileSandboxHTML(task.Sitekey, action, cdata)

	if err := page.Route("**/*", func(route playwright.Route) {
		request := route.Request()
		requestURL := request.URL()

		if shouldAllowTurnstileRequest(requestURL) {
			_ = route.Continue()
			return
		}

		if request.IsNavigationRequest() && request.ResourceType() == "document" {
			_ = route.Fulfill(playwright.RouteFulfillOptions{
				Status:      playwright.Int(200),
				ContentType: playwright.String("text/html; charset=utf-8"),
				Body:        html,
			})
			return
		}

		_ = route.Abort()
	}); err != nil {
		return nil, fmt.Errorf("route setup error: %w", err)
	}

	resp, err := page.Goto(task.URL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateCommit,
		Timeout:   playwright.Float(6000),
	})
	if err != nil {
		return nil, fmt.Errorf("navigation error: %w", err)
	}

	headers := map[string]string{}
	if resp != nil && resp.Request() != nil {
		if allHeaders, allErr := resp.Request().AllHeaders(); allErr == nil && len(allHeaders) > 0 {
			headers = allHeaders
		} else {
			headers = resp.Request().Headers()
		}
	}

	return headers, nil
}

func setTaskFailed(task *SolveTask, message string) {
	task.Status = "failed"
	task.Error = &message
	task.Token = nil
}

func calculateSolveTime(task *SolveTask) *float64 {
	if task == nil || task.CompletedAt == nil {
		return nil
	}

	solveTime := *task.CompletedAt - task.CreatedAt
	if solveTime < 0 {
		solveTime = 0
	}
	return &solveTime
}

func clickTurnstileCheckbox(page playwright.Page) {
	coordsResult, err := page.Evaluate(`() => {
		const iframe = document.querySelector("iframe[src*='challenges.cloudflare.com']");
		if (!iframe) return null;
		const rect = iframe.getBoundingClientRect();
		if (!rect || rect.width <= 0 || rect.height <= 0) return null;

		return {
			x: rect.left + Math.min(35, rect.width / 2),
			y: rect.top + rect.height / 2
		};
	}`)
	if err == nil {
		if coords, ok := coordsResult.(map[string]interface{}); ok {
			xVal, xOK := coords["x"].(float64)
			yVal, yOK := coords["y"].(float64)
			if xOK && yOK {
				_ = page.Mouse().Click(xVal, yVal)
				// Extra click helps pass to "verify you are human" quickly.
				_ = page.Mouse().Click(xVal+2, yVal+1)
			}
		}
	}

	selectors := []string{
		"iframe[src*='challenges.cloudflare.com']",
		"#widget",
		".cf-turnstile",
	}
	for _, selector := range selectors {
		_ = page.Click(selector, playwright.PageClickOptions{Timeout: playwright.Float(50)})
	}
}

func readDOMValidationState(page playwright.Page) map[string]any {
	result, err := page.Evaluate(`() => {
		const widget = document.querySelector("#widget");
		const input = document.querySelector("input[name='cf-turnstile-response']");
		const iframe = document.querySelector("iframe[src*='challenges.cloudflare.com'], iframe[src*='challenges.fed.cloudflare.com']");
		const iframeRect = iframe ? iframe.getBoundingClientRect() : null;
		const iframeVisible = !!iframeRect && iframeRect.width > 0 && iframeRect.height > 0;

		return {
			widget: !!widget,
			input: !!input,
			turnstile: !!window.turnstile,
			rendered: !!window.__turnstileRendered,
			iframe: !!iframe,
			iframeVisible: iframeVisible,
			error: String(window.__turnstileError || "")
		};
	}`)
	if err != nil {
		return map[string]any{
			"error": err.Error(),
		}
	}
	if state, ok := result.(map[string]any); ok {
		return state
	}
	return map[string]any{
		"error": "invalid DOM validation response",
	}
}

func validateTurnstileDOM(page playwright.Page) error {
	deadline := time.Now().Add(3500 * time.Millisecond)
	var lastState map[string]any

	for time.Now().Before(deadline) {
		lastState = readDOMValidationState(page)

		if errText, ok := lastState["error"].(string); ok && strings.TrimSpace(errText) != "" {
			if !strings.Contains(strings.ToLower(errText), "unknown_error") {
				return fmt.Errorf("%s", errText)
			}
		}

		widget, _ := lastState["widget"].(bool)
		input, _ := lastState["input"].(bool)
		turnstile, _ := lastState["turnstile"].(bool)
		rendered, _ := lastState["rendered"].(bool)
		iframe, _ := lastState["iframe"].(bool)
		iframeVisible, _ := lastState["iframeVisible"].(bool)

		if widget && input && turnstile && rendered && iframe && iframeVisible {
			return nil
		}

		time.Sleep(50 * time.Millisecond)
	}

	stateJSON, _ := json.Marshal(lastState)
	return fmt.Errorf("turnstile DOM invalid: %s", string(stateJSON))
}

func collectTaskCookies(page playwright.Page, taskURL string) []TaskCookie {
	if page == nil || page.Context() == nil {
		return nil
	}

	rawCookies, err := page.Context().Cookies(taskURL)
	if err != nil {
		return nil
	}

	cookies := make([]TaskCookie, 0, len(rawCookies))
	for _, item := range rawCookies {
		sameSite := ""
		if item.SameSite != nil {
			sameSite = string(*item.SameSite)
		}
		cookies = append(cookies, TaskCookie{
			Name:     item.Name,
			Value:    item.Value,
			Domain:   item.Domain,
			Path:     item.Path,
			Expires:  item.Expires,
			HttpOnly: item.HttpOnly,
			Secure:   item.Secure,
			SameSite: sameSite,
		})
	}
	return cookies
}

func cleanupPageAfterTask(page playwright.Page) {
	if page != nil {
		_ = page.UnrouteAll()

		_, _ = page.Evaluate(`() => {
			try {
				localStorage.clear();
				sessionStorage.clear();
			} catch (_err) {}

			try {
				document.cookie.split(";").forEach((pair) => {
					const eq = pair.indexOf("=");
					const name = eq > -1 ? pair.substr(0, eq).trim() : pair.trim();
					if (name) {
						document.cookie = name + "=;expires=Thu, 01 Jan 1970 00:00:00 GMT;path=/";
					}
				});
			} catch (_err) {}

			return true;
		}`)

		_, _ = page.Goto("about:blank", playwright.PageGotoOptions{
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
			Timeout:   playwright.Float(5000),
		})
	}

	if page != nil && page.Context() != nil {
		_ = page.Context().ClearCookies()
	}
}

func releaseMemory() {
	runtime.GC()
	debug.FreeOSMemory()
}

func solveTaskOnPage(page playwright.Page, task *SolveTask) bool {
	headers, err := loadTurnstileInTab(page, task)
	if len(headers) > 0 {
		task.Headers = headers
	}
	if err != nil {
		setTaskFailed(task, fmt.Sprintf("overlay error: %v", err))
		return false
	}

	if err := validateTurnstileDOM(page); err != nil {
		setTaskFailed(task, fmt.Sprintf("dom validation error: %v", err))
		return false
	}

	maxSolveSeconds := CONFIG.TimeoutSeconds
	if maxSolveSeconds > 15 {
		maxSolveSeconds = 15
	}
	if maxSolveSeconds < 8 {
		maxSolveSeconds = 8
	}
	deadline := time.Now().Add(time.Duration(maxSolveSeconds) * time.Second)
	nextClickAt := time.Now()

	for time.Now().Before(deadline) {
		if time.Now().After(nextClickAt) {
			clickTurnstileCheckbox(page)
			nextClickAt = time.Now().Add(300 * time.Millisecond)
		}

		stateResult, evalErr := page.Evaluate(`() => {
			const input = document.querySelector('input[name="cf-turnstile-response"]');
			const token = window.__turnstileToken || input?.value || '';
			const error = window.__turnstileError || '';
			return { token, error };
		}`)

		if evalErr == nil {
			if state, ok := stateResult.(map[string]interface{}); ok {
				if token, okToken := state["token"].(string); okToken && len(token) > 20 {
					task.Status = "success"
					task.Error = nil
					task.Token = &token
					task.Cookies = collectTaskCookies(page, task.URL)
					return true
				}

				if errorMsg, okError := state["error"].(string); okError && strings.TrimSpace(errorMsg) != "" {
					setTaskFailed(task, "turnstile error: "+strings.TrimSpace(errorMsg))
					return false
				}
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	task.Cookies = collectTaskCookies(page, task.URL)
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
	defer releaseMemory()

	success := solveTaskOnPage(page, task)
	cleanupPageAfterTask(page)
	return success
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

	if w.Page == nil {
		w.TabReady = false
		return success
	}

	cleanupPageAfterTask(w.Page)
	releaseMemory()

	if w.Page == nil {
		w.TabReady = false
		return success
	}
	_, err := w.Page.Title()
	if err != nil {
		w.TabReady = false
		return success
	}

	w.TabReady = true
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
				if !worker.TabReady {
					worker.recreateTab()
				}
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

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeDomainURL(raw string) (string, error) {
	domain := strings.TrimSpace(raw)
	if domain == "" {
		return "", fmt.Errorf("missing domain")
	}
	if !strings.Contains(domain, "://") {
		domain = "https://" + domain
	}

	parsed, err := url.Parse(domain)
	if err != nil {
		return "", fmt.Errorf("invalid domain: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid domain URL")
	}
	return parsed.String(), nil
}

func cloudflareProxyURL(req CloudflareRequest) *string {
	for _, value := range []string{req.ProxyURL2, req.ProxyURL} {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return &trimmed
		}
	}

	if req.Task != nil {
		if proxy := extractProxyValue(*req.Task); proxy != nil {
			return proxy
		}
	}

	if req.Proxy == nil {
		return nil
	}

	if raw := strings.TrimSpace(req.Proxy.URL); raw != "" {
		return &raw
	}

	host := strings.TrimSpace(req.Proxy.Host)
	if host == "" {
		return nil
	}

	port := req.Proxy.Port
	if port <= 0 {
		port = 80
	}

	userInfo := ""
	username := strings.TrimSpace(req.Proxy.Username)
	password := strings.TrimSpace(req.Proxy.Password)
	if username != "" {
		userInfo = url.QueryEscape(username)
		if password != "" {
			userInfo = userInfo + ":" + url.QueryEscape(password)
		}
		userInfo = userInfo + "@"
	}

	proxyURL := fmt.Sprintf("http://%s%s:%d", userInfo, host, port)
	return &proxyURL
}

func resolveTurnstilePayload(req CloudflareRequest) (string, string, string, string, *string, error) {
	domain := firstNonEmptyString(req.Domain)
	sitekey := firstNonEmptyString(req.SiteKey, req.Sitekey)
	action := firstNonEmptyString(req.Action)
	cdata := firstNonEmptyString(req.CData, req.Cdata)

	if req.Task != nil {
		domain = firstNonEmptyString(domain, req.Task.WebsiteURL)
		sitekey = firstNonEmptyString(sitekey, req.Task.WebsiteKey)
		action = firstNonEmptyString(action, req.Task.Action)
		cdata = firstNonEmptyString(cdata, req.Task.Cdata)
	}

	normalizedDomain, err := normalizeDomainURL(domain)
	if err != nil {
		return "", "", "", "", nil, err
	}
	if sitekey == "" {
		return "", "", "", "", nil, fmt.Errorf("missing siteKey")
	}

	return normalizedDomain, sitekey, action, cdata, cloudflareProxyURL(req), nil
}

func submitTaskAndWait(task *SolveTask, timeout time.Duration) (*SolveTask, error) {
	saveTaskToDB(task)

	select {
	case taskQueue <- task:
		stats.Lock()
		stats.Total++
		stats.Unlock()
	default:
		return nil, fmt.Errorf("task queue is full")
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		latest := getTaskFromDB(task.TaskID)
		if latest == nil {
			time.Sleep(25 * time.Millisecond)
			continue
		}

		if latest.Status == "success" || latest.Status == "failed" {
			return latest, nil
		}
		time.Sleep(25 * time.Millisecond)
	}

	timeoutMsg := "timeout waiting for solve result"
	now := float64(time.Now().UnixNano()) / 1e9
	task.Status = "failed"
	task.Error = &timeoutMsg
	task.CompletedAt = &now
	saveTaskToDB(task)
	return task, nil
}

func copyAnyMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func getIUAMCache(key string) (map[string]any, bool) {
	iuamCache.RLock()
	entry, ok := iuamCache.Entries[key]
	iuamCache.RUnlock()
	if !ok || time.Now().After(entry.ExpireAt) {
		if ok {
			iuamCache.Lock()
			delete(iuamCache.Entries, key)
			iuamCache.Unlock()
		}
		return nil, false
	}
	return copyAnyMap(entry.Value), true
}

func setIUAMCache(key string, value map[string]any, ttl time.Duration) {
	iuamCache.Lock()
	iuamCache.Entries[key] = iuamCacheEntry{
		ExpireAt: time.Now().Add(ttl),
		Value:    copyAnyMap(value),
	}
	iuamCache.Unlock()
}

func solveIUAM(domain string, proxyURL string) (map[string]any, error) {
	browser, err := launchBrowser(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("browser launch error: %w", err)
	}
	defer func() { _ = browser.Close() }()

	context, err := browser.NewContext(buildContextOptions())
	if err != nil {
		return nil, fmt.Errorf("context error: %w", err)
	}
	defer func() { _ = context.Close() }()

	page, err := context.NewPage()
	if err != nil {
		return nil, fmt.Errorf("page error: %w", err)
	}
	defer func() { _ = page.Close() }()
	defer releaseMemory()

	start := time.Now()
	resp, err := page.Goto(domain, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateCommit,
		Timeout:   playwright.Float(float64((CONFIG.TimeoutSeconds + 5) * 1000)),
	})
	if err != nil {
		return nil, fmt.Errorf("navigation error: %w", err)
	}

	headers := map[string]string{}
	if resp != nil && resp.Request() != nil {
		if allHeaders, allErr := resp.Request().AllHeaders(); allErr == nil && len(allHeaders) > 0 {
			headers = allHeaders
		} else {
			headers = resp.Request().Headers()
		}
	}

	deadline := time.Now().Add(time.Duration(CONFIG.TimeoutSeconds) * time.Second)
	if CONFIG.TimeoutSeconds < 8 {
		deadline = time.Now().Add(8 * time.Second)
	}

	for time.Now().Before(deadline) {
		cookies := collectTaskCookies(page, domain)
		for _, cookie := range cookies {
			if cookie.Name == "cf_clearance" && strings.TrimSpace(cookie.Value) != "" {
				uaValue := ""
				if uaResult, uaErr := page.Evaluate(`() => navigator.userAgent || ""`); uaErr == nil {
					if ua, ok := uaResult.(string); ok {
						uaValue = ua
					}
				}

				solveTime := time.Since(start).Seconds()
				return map[string]any{
					"code":         200,
					"cf_clearance": cookie.Value,
					"user_agent":   uaValue,
					"headers":      headers,
					"cookies":      cookies,
					"solveTime":    solveTime,
				}, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	return nil, fmt.Errorf("timeout waiting for cf_clearance")
}

func cloudflareHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()
	var req CloudflareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    400,
			"message": "bad request: invalid JSON body",
		})
		return
	}

	if CONFIG.AuthToken != "" && strings.TrimSpace(req.AuthToken) != CONFIG.AuthToken {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    401,
			"message": "unauthorized",
		})
		return
	}

	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	switch mode {
	case "turnstile":
		domain, sitekey, action, cdata, proxyURL, err := resolveTurnstilePayload(req)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    400,
				"message": err.Error(),
			})
			return
		}

		task := &SolveTask{
			TaskID:    uuid.New().String(),
			URL:       domain,
			Sitekey:   sitekey,
			Proxy:     proxyURL,
			Status:    "pending",
			CreatedAt: float64(time.Now().UnixNano()) / 1e9,
		}
		if action != "" {
			task.Action = &action
		}
		if cdata != "" {
			task.Cdata = &cdata
		}

		finalTask, submitErr := submitTaskAndWait(task, time.Duration(CONFIG.TimeoutSeconds+5)*time.Second)
		if submitErr != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    429,
				"message": submitErr.Error(),
			})
			return
		}

		if finalTask == nil || finalTask.Status != "success" || finalTask.Token == nil {
			errMessage := "failed to solve turnstile"
			if finalTask != nil && finalTask.Error != nil {
				errMessage = *finalTask.Error
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    500,
				"message": errMessage,
				"elapsed": fmt.Sprintf("%.2fs", time.Since(start).Seconds()),
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":      200,
			"token":     *finalTask.Token,
			"headers":   finalTask.Headers,
			"cookies":   finalTask.Cookies,
			"solveTime": calculateSolveTime(finalTask),
			"elapsed":   fmt.Sprintf("%.2fs", time.Since(start).Seconds()),
		})
		return

	case "iuam":
		domain := firstNonEmptyString(req.Domain)
		if req.Task != nil {
			domain = firstNonEmptyString(domain, req.Task.WebsiteURL)
		}

		normalizedDomain, err := normalizeDomainURL(domain)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    400,
				"message": err.Error(),
			})
			return
		}

		proxy := ""
		if proxyURL := cloudflareProxyURL(req); proxyURL != nil {
			proxy = *proxyURL
		}

		cacheKey := fmt.Sprintf("%s|%s|%s", mode, normalizedDomain, proxy)
		if cached, ok := getIUAMCache(cacheKey); ok {
			cached["cached"] = true
			cached["elapsed"] = fmt.Sprintf("%.2fs", time.Since(start).Seconds())
			_ = json.NewEncoder(w).Encode(cached)
			return
		}

		result, solveErr := solveIUAM(normalizedDomain, proxy)
		if solveErr != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    500,
				"message": solveErr.Error(),
				"elapsed": fmt.Sprintf("%.2fs", time.Since(start).Seconds()),
			})
			return
		}

		ttlMillis := req.TTL
		if ttlMillis <= 0 {
			ttlMillis = req.Expire
		}
		if ttlMillis <= 0 {
			ttlMillis = int64((30 * time.Minute) / time.Millisecond)
		}
		setIUAMCache(cacheKey, result, time.Duration(ttlMillis)*time.Millisecond)

		result["cached"] = false
		result["elapsed"] = fmt.Sprintf("%.2fs", time.Since(start).Seconds())
		_ = json.NewEncoder(w).Encode(result)
		return

	default:
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    400,
			"message": "invalid mode, expected turnstile or iuam",
		})
		return
	}
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
			Token:     *task.Token,
			Headers:   task.Headers,
			Cookies:   task.Cookies,
			SolveTime: calculateSolveTime(task),
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
			"token":     *task.Token,
			"headers":   task.Headers,
			"cookies":   task.Cookies,
			"solveTime": calculateSolveTime(task),
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
	mux.HandleFunc("/cloudflare", cloudflareHandler)
	mux.HandleFunc("/createTask", createTaskHandler)
	mux.HandleFunc("/getTaskResult", getTaskResultHandler)
	mux.HandleFunc("/turnstile", turnstileHandler)
	mux.HandleFunc("/result", resultHandler)
	mux.HandleFunc("/", indexHandler)

	listenAddr := fmt.Sprintf("%s:%d", CONFIG.ServerHost, CONFIG.ServerPort)
	fmt.Printf("starting server on http://%s\n", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
