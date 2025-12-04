package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/oschwald/geoip2-golang"
	"github.com/redis/go-redis/v9"
	"resty.dev/v3"
)

// noopLogger discards all Redis log messages
type noopLogger struct{}

func (noopLogger) Printf(ctx context.Context, format string, v ...any) {}

func init() {
	// Disable Redis client logging to prevent TUI breakage
	redis.SetLogger(noopLogger{})
}

// Constants for configuration
const (
	maxWorkers   = 1000                           // Maximum concurrent workers
	geoDBMaxMind = "/home/geo/GeoLite2-City.mmdb" // Path to MaxMind GeoLite2-City database (download from maxmind.com)
	timeout      = 30 * time.Second               // Timeout for each request
	proxiesURL   = "http://sarnet.ru/prt"         // URL to fetch proxy list (one per line)
	serverIDURL  = "http://sarnet.ru/id"          // URL to fetch serverID
	retryCount   = 5                              // Number of retry attempts on error
	retryDelay   = 1 * time.Second                // Delay between retries
	maxResults   = 1000                           // Max lines to keep in results buffer for scroll (to avoid OOM; increase if needed)
	httpTimeout  = 10 * time.Second               // Timeout for HTTP requests in main
	pageTimeout  = 30 * time.Second               // Timeout for page loads (matching bot behavior)
)

// Search keywords pool (simple set for DuckDuckGo testing)
var searchKeywords = []string{
	"technology", "science", "news", "weather", "sports",
	"travel", "food", "music", "books", "art",
	"history", "nature", "animals", "space", "ocean",
}

// AdTask represents a task for rod worker (simplified version)
type AdTask struct {
	URL       string         `json:"url"`
	RecordBid float64        `json:"recordBid"`
	Member    int64          `json:"member"`
	Port      map[string]any `json:"port"`
	Browser   map[string]any `json:"browser"`
	WebGL     map[string]any `json:"webgl"`
	Navigator map[string]any `json:"navigator"`
	Screen    map[string]any `json:"screen"`
	Battery   map[string]any `json:"battery"`
}

// Result struct to hold proxy check outcome
type proxyResult struct {
	proxy        string
	ip           string
	country      string
	timezone     string
	duration     time.Duration
	err          error
	pageLoadOK   bool          // Page load test passed
	pageLoadTime time.Duration // Page load duration
	pageLoadErr  error         // Page load error
	footerLine   string        // Random footer line from sarnet.ru/test (sarnet mode)
}

// Global variables for proxy checking
var (
	maxMindReader  *geoip2.Reader
	requestURL     string
	serverID       string
	teaProgram     *tea.Program
	pageLoadMode   bool // Enable page load testing mode
	sarnetMode     bool // Enable sarnet.ru/test mode
)

// Model for Bubble Tea TUI
type model struct {
	proxies []string // List of proxies to check

	// Proxies section
	proxiesTotal    int            // Total proxies
	proxiesChecked  int            // Checked proxies
	proxiesWorking  int            // Working proxies
	proxiesProgress progress.Model // Progress bar for proxies
	proxiesViewport viewport.Model // Viewport for proxies results
	proxiesResults  []string       // Proxies results
	proxiesFailed   []string       // Failed proxies

	quit   bool               // Flag to quit
	err    error              // Any error
	mu     sync.Mutex         // Mutex for safe updates
	ctx    context.Context    // Context for cancellation
	cancel context.CancelFunc // Cancel function

	width  int // Terminal width
	height int // Terminal height

	// Styles for colored output in TUI
	lineNumStyle     lipgloss.Style // White for line numbers
	hostStyle        lipgloss.Style // Cyan for host:port
	ipStyle          lipgloss.Style // Green for IP
	timeStyle        lipgloss.Style // Yellow for response time
	timezoneStyle    lipgloss.Style // Magenta for timezone
	errorStyle       lipgloss.Style // Red for error
	totalLabelStyle  lipgloss.Style // Style for "Total" label
	failedLabelStyle lipgloss.Style // Style for "Failed" label
}

// Init initializes the model
func (m *model) Init() tea.Cmd {
	m.lineNumStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))    // White for line numbers
	m.hostStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))        // Cyan for curl command
	m.ipStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))          // Green for IP
	m.timeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))        // Yellow for response time
	m.timezoneStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))    // Magenta for timezone
	m.errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))       // Red for error
	m.totalLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")) // White for Total
	m.failedLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // Red for Failed
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return nil
}

// Update handles messages and updates the model
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "Q", "й", "Й", "esc", "ctrl+c": // q -> й (quit)
			m.quit = true
			m.cancel() // Cancel context to stop workers
			return m, tea.Quit
		case "r", "R", "к", "К": // r -> к (retry)
			// Clear results and reset progress
			m.mu.Lock()
			m.proxiesChecked = 0
			m.proxiesWorking = 0
			m.proxiesResults = make([]string, 0)
			m.proxiesFailed = make([]string, 0)
			m.proxiesViewport.SetContent("")
			m.mu.Unlock()
			// Start new check
			go m.checkProxies()
			return m, m.proxiesProgress.SetPercent(0)
		case "d", "D", "в", "В": // d -> в (duckduckgo - toggle page load mode)
			pageLoadMode = !pageLoadMode
			sarnetMode = false // Disable sarnet mode when enabling duckduckgo
			// Restart check with new mode
			m.mu.Lock()
			m.proxiesChecked = 0
			m.proxiesWorking = 0
			m.proxiesResults = make([]string, 0)
			m.proxiesFailed = make([]string, 0)
			m.proxiesViewport.SetContent("")
			m.mu.Unlock()
			go m.checkProxies()
			return m, m.proxiesProgress.SetPercent(0)
		case "s", "S", "ы", "Ы": // s -> ы (sarnet - toggle sarnet mode)
			sarnetMode = !sarnetMode
			pageLoadMode = false // Disable duckduckgo mode when enabling sarnet
			// Restart check with new mode
			m.mu.Lock()
			m.proxiesChecked = 0
			m.proxiesWorking = 0
			m.proxiesResults = make([]string, 0)
			m.proxiesFailed = make([]string, 0)
			m.proxiesViewport.SetContent("")
			m.mu.Unlock()
			go m.checkProxies()
			return m, m.proxiesProgress.SetPercent(0)
		case "up", "down", "pgup", "pgdown":
			// Viewport navigation
			var cmd tea.Cmd
			m.proxiesViewport, cmd = m.proxiesViewport.Update(msg)
			return m, cmd
		case "home":
			m.proxiesViewport.GotoTop()
			return m, nil
		case "end":
			m.proxiesViewport.GotoBottom()
			return m, nil
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		// Progress bar width - reserve space for "Total: XXX" and "Failed: XXX"
		// Total:999 = 9 chars, Failed:999 = 10 chars, 2 spaces = 2 chars, total = 21 chars
		m.proxiesProgress.Width = msg.Width - 21

		// Viewport dimensions for bordered output
		// Height: total - progress line - help line - borders (3 lines)
		viewportHeight := msg.Height - 4
		if viewportHeight < 5 {
			viewportHeight = 5
		}
		m.proxiesViewport.Width = msg.Width - 4 // Account for border padding
		m.proxiesViewport.Height = viewportHeight
	case progress.FrameMsg:
		// Update progress bar
		newProxiesProgress, cmd := m.proxiesProgress.Update(msg)
		m.proxiesProgress = newProxiesProgress.(progress.Model)
		return m, cmd
	case proxyResult:
		// Process proxy result
		m.mu.Lock()
		m.proxiesChecked++
		var coloredLogLine string

		// Format curl command (cyan) - fixed width 47 chars
		proxyClean := strings.TrimPrefix(msg.proxy, "http://")
		curlCmd := fmt.Sprintf("curl -x %s s%s.sarnet.ru/ip", proxyClean, serverID)
		if serverID == "00" {
			curlCmd = fmt.Sprintf("curl -x %s 213.234.252.212/ip", proxyClean)
		}
		// Format for display: truncate to 47 chars if too long, or pad with spaces if too short
		displayCurl := curlCmd
		if len(displayCurl) > 47 {
			displayCurl = displayCurl[:44] + "..."
		}
		formattedCurl := fmt.Sprintf("%-47s", displayCurl)
		coloredHost := m.hostStyle.Render(formattedCurl)

		// Check if proxy is working:
		// 1. Connection must succeed (msg.err == nil)
		// 2. If page load mode or sarnet mode enabled, test must pass (msg.pageLoadOK == true)
		isWorking := msg.err == nil && (!(pageLoadMode || sarnetMode) || msg.pageLoadOK)

		if isWorking {
			m.proxiesWorking++
			// Calculate visible lengths WITHOUT styles for proper padding
			lineNumStr := fmt.Sprintf("%3d", m.proxiesChecked)
			ipStr := fmt.Sprintf("%-16s", msg.ip)
			timeStr := fmt.Sprintf("%.2f", msg.duration.Seconds())

			// Page load status (if enabled)
			var pageLoadStatus string
			if pageLoadMode || sarnetMode {
				if msg.pageLoadOK {
					if sarnetMode && msg.footerLine != "" {
						// Show footer line in sarnet mode
						pageLoadStatus = fmt.Sprintf(" [%.1fs: %s]", msg.pageLoadTime.Seconds(), msg.footerLine)
					} else {
						pageLoadStatus = fmt.Sprintf(" [PL:%.1fs]", msg.pageLoadTime.Seconds())
					}
				} else {
					// Show failure status
					if sarnetMode {
						// Show error message in sarnet mode
						if msg.pageLoadErr != nil {
							shortErr := getShortError(msg.pageLoadErr)
							pageLoadStatus = fmt.Sprintf(" [FAIL: %s]", shortErr)
						} else {
							pageLoadStatus = " [FAIL]"
						}
					} else {
						pageLoadStatus = " [PL:FAIL]"
					}
				}
			}

			// Calculate padding based on visible lengths (no ANSI codes)
			availableWidth := m.width - 4 // Account for viewport border padding
			leftLen := len(lineNumStr) + 1 + 47 + 1 + len(ipStr) + 1 + len(msg.timezone) + 1 + len(pageLoadStatus) // lineNum + space + curl + space + ip + space + timezone + space + pageLoadStatus
			rightLen := len(timeStr)                                                          // time only
			padding := max(0, availableWidth-leftLen-rightLen)

			// Now apply styles and build colored line
			lineNumPart := m.lineNumStyle.Render(lineNumStr)
			ipPart := m.ipStyle.Render(ipStr)
			timePart := m.timeStyle.Render(timeStr)
			timezonePart := m.timezoneStyle.Render(msg.timezone)

			// Color page load status
			pageLoadStatusColored := pageLoadStatus
			if pageLoadMode || sarnetMode {
				if msg.pageLoadOK {
					pageLoadStatusColored = m.ipStyle.Render(pageLoadStatus) // Green for success
				} else {
					pageLoadStatusColored = m.errorStyle.Render(pageLoadStatus) // Red for failure
				}
			}

			coloredLogLine = lineNumPart + " " + coloredHost + " " + ipPart + " " + timezonePart + pageLoadStatusColored + " " + strings.Repeat(" ", padding) + timePart
		} else if msg.err != nil {
			// Connection error - proxy doesn't work at all
			shortErr := getShortError(msg.err)
			logLine := fmt.Sprintf("%s %s", curlCmd, shortErr) // Use original curlCmd for log

			// Calculate visible lengths WITHOUT styles for proper padding
			lineNumStr := fmt.Sprintf("%3d", m.proxiesChecked)
			timeStr := fmt.Sprintf("%.2f", msg.duration.Seconds())

			// Calculate padding based on visible lengths (no ANSI codes)
			availableWidth := m.width - 4 // Account for viewport border padding
			leftLen := len(lineNumStr) + 1 + 47 + 1                // lineNum + space + curl + space
			rightLen := len(timeStr) + 1 + len(shortErr)           // time + space + error
			padding := max(0, availableWidth-leftLen-rightLen)

			// Now apply styles and build colored line
			lineNumPart := m.lineNumStyle.Render(lineNumStr)
			timePart := m.timeStyle.Render(timeStr)
			errorPart := m.errorStyle.Render(shortErr)

			coloredLogLine = lineNumPart + " " + coloredHost + " " + strings.Repeat(" ", padding) + timePart + " " + errorPart
			m.proxiesFailed = append(m.proxiesFailed, logLine)
		} else {
			// Proxy connected but page load/sarnet test failed
			// msg.err == nil, but !msg.pageLoadOK
			var shortErr string
			if msg.pageLoadErr != nil {
				shortErr = getShortError(msg.pageLoadErr)
			} else {
				shortErr = "Test failed"
			}
			logLine := fmt.Sprintf("%s %s %s %s", curlCmd, msg.ip, msg.timezone, shortErr)

			// Calculate visible lengths WITHOUT styles for proper padding
			lineNumStr := fmt.Sprintf("%3d", m.proxiesChecked)
			ipStr := fmt.Sprintf("%-16s", msg.ip)
			timeStr := fmt.Sprintf("%.2f", msg.duration.Seconds())

			// Calculate padding based on visible lengths (no ANSI codes)
			availableWidth := m.width - 4 // Account for viewport border padding
			leftLen := len(lineNumStr) + 1 + 47 + 1 + len(ipStr) + 1 + len(msg.timezone) + 1 // lineNum + space + curl + space + ip + space + timezone + space
			rightLen := len(timeStr) + 1 + len(shortErr)                                      // time + space + error
			padding := max(0, availableWidth-leftLen-rightLen)

			// Now apply styles and build colored line
			lineNumPart := m.lineNumStyle.Render(lineNumStr)
			ipPart := m.ipStyle.Render(ipStr)
			timePart := m.timeStyle.Render(timeStr)
			timezonePart := m.timezoneStyle.Render(msg.timezone)
			errorPart := m.errorStyle.Render(shortErr)

			coloredLogLine = lineNumPart + " " + coloredHost + " " + ipPart + " " + timezonePart + " " + strings.Repeat(" ", padding) + timePart + " " + errorPart
			m.proxiesFailed = append(m.proxiesFailed, logLine)
		}

		m.proxiesResults = append(m.proxiesResults, coloredLogLine)
		// Limit results buffer to maxResults (keep last N to avoid memory issues)
		if len(m.proxiesResults) > maxResults {
			m.proxiesResults = m.proxiesResults[len(m.proxiesResults)-maxResults:]
		}
		// Update progress
		cmd := m.proxiesProgress.SetPercent(float64(m.proxiesChecked) / float64(m.proxiesTotal))
		// Set content and scroll to bottom
		m.proxiesViewport.SetContent(strings.Join(m.proxiesResults, "\n"))
		m.proxiesViewport.GotoBottom()
		m.mu.Unlock()
		return m, cmd
	}

	var cmd tea.Cmd
	m.proxiesViewport, cmd = m.proxiesViewport.Update(msg)
	return m, cmd
}

// checkProxies runs proxy check test
func (m *model) checkProxies() {
	// Reset model state
	m.mu.Lock()
	m.proxiesChecked = 0
	m.proxiesWorking = 0
	m.proxiesResults = make([]string, 0)
	m.proxiesFailed = make([]string, 0)
	m.mu.Unlock()

	// Run checks in background
	go runChecks(m, teaProgram, maxMindReader, requestURL)
}

// View renders the TUI
func (m *model) View() string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v\nPress q (quit) | r (retry)", m.err)
	}
	if m.quit {
		return ""
	}

	// Progress bar: "Total: XXX" on left, progress in middle, "Failed: XXX" on right
	totalLabel := m.totalLabelStyle.Render(fmt.Sprintf("Total:%3d", m.proxiesTotal))
	failedCount := m.proxiesChecked - m.proxiesWorking
	failedLabel := m.failedLabelStyle.Render(fmt.Sprintf("Failed:%3d", failedCount))
	progressBar := lipgloss.JoinHorizontal(lipgloss.Left, totalLabel, " ", m.proxiesProgress.View(), " ", failedLabel)

	// Viewport with border (like run.go)
	viewportPanel := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("63")).
		Padding(0, 1).
		Render(m.proxiesViewport.View())

	// Help text pinned to bottom with page load mode indicator
	helpText := "Press q (quit) | r (retry) | d (duckduckgo) | s (sarnet.ru/test)"
	if pageLoadMode {
		helpText = "Press q (quit) | r (retry) | d (duckduckgo: ON) | s (sarnet)"
	} else if sarnetMode {
		helpText = "Press q (quit) | r (retry) | d (duckduckgo) | s (sarnet: ON)"
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		progressBar,
		viewportPanel,
		helpText,
	)
}

// getShortError returns a concise error message
func getShortError(err error) string {
	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "timeout") {
		return "Timeout"
	}
	if strings.Contains(errStr, "429") || strings.Contains(errStr, "too many requests") {
		return "Too Many Requests"
	}
	if strings.Contains(errStr, "failed to launch chrome") {
		return "Chrome launch failed"
	}
	if strings.Contains(errStr, "failed to navigate") {
		return "Navigation failed"
	}
	if strings.Contains(errStr, "failed to find") || strings.Contains(errStr, "element not found") {
		return "Element not found"
	}
	if strings.Contains(errStr, "failed to load") {
		return "Page load failed"
	}
	if strings.Contains(errStr, "operation cancelled") || strings.Contains(errStr, "context canceled") {
		return "Cancelled"
	}
	// Return full error if no match
	if len(err.Error()) > 30 {
		return err.Error()[:30] + "..."
	}
	return err.Error()
}

// checkProxy checks a single proxy with retries
func checkProxy(ctx context.Context, proxy string, results chan<- proxyResult, maxMindReader *geoip2.Reader, requestURL string) {
	start := time.Now()

	// Check if context is cancelled before starting
	select {
	case <-ctx.Done():
		results <- proxyResult{proxy: proxy, duration: time.Since(start), err: fmt.Errorf("operation cancelled")}
		return
	default:
	}

	var ip string
	ping := resty.New()
	defer ping.Close()
	ping.SetRetryCount(retryCount).SetRetryWaitTime(retryDelay).SetRetryMaxWaitTime(timeout / 2)
	ping.SetTimeout(timeout)
	ping.SetProxy(proxy)

	for range retryCount {
		// Check context before each attempt
		select {
		case <-ctx.Done():
			results <- proxyResult{proxy: proxy, duration: time.Since(start), err: fmt.Errorf("operation cancelled")}
			return
		default:
		}

		res, err := ping.R().Get(requestURL)
		if err != nil {
			results <- proxyResult{proxy: proxy, duration: time.Since(start), err: err}
			return
		}
		ip = strings.TrimSpace(res.String())
		if len(ip) > 0 {
			break
		}
		time.Sleep(retryDelay)
	}

	if len(ip) == 0 {
		results <- proxyResult{proxy: proxy, duration: time.Since(start), err: fmt.Errorf("empty response")}
		return
	}

	// Validate IP format
	netIP := net.ParseIP(ip)
	if netIP == nil {
		// Invalid IP - don't check geobase, just return error
		results <- proxyResult{proxy: proxy, ip: ip, duration: time.Since(start), err: fmt.Errorf("invalid IP")}
		return
	}

	// Get geo info from local MMDB only for valid IPs
	country, timezone, err := getGeoFromDB(maxMindReader, netIP)
	if err != nil {
		log.Printf("Geo lookup error for %s: %v", proxy, err)
		country = "Unknown"
		timezone = "Unknown"
	}
	if country == "" {
		country = "Unknown"
	}
	if timezone == "" {
		timezone = "Unknown"
	}

	// If page load mode is enabled, test real page loading
	var pageLoadOK bool
	var pageLoadTime time.Duration
	var pageLoadErr error
	var footerLine string

	if pageLoadMode {
		pageLoadTime, pageLoadErr = testPageLoad(ctx, proxy)
		pageLoadOK = (pageLoadErr == nil)
	} else if sarnetMode {
		footerLine, pageLoadTime, pageLoadErr = testSarnetPage(ctx, proxy)
		pageLoadOK = (pageLoadErr == nil)
	}

	results <- proxyResult{
		proxy:        proxy,
		ip:           ip,
		country:      country,
		timezone:     timezone,
		duration:     time.Since(start),
		err:          nil,
		pageLoadOK:   pageLoadOK,
		pageLoadTime: pageLoadTime,
		pageLoadErr:  pageLoadErr,
		footerLine:   footerLine,
	}
}

// getGeoFromDB gets country and timezone from a geoip2 Reader
func getGeoFromDB(reader *geoip2.Reader, ip net.IP) (country, timezone string, err error) {
	if reader == nil {
		return "", "", fmt.Errorf("reader is nil")
	}
	record, err := reader.City(ip)
	if err != nil {
		return "", "", err
	}

	country = record.Country.Names["en"]
	timezone = record.Location.TimeZone
	return country, timezone, nil
}

// testSarnetPage tests sarnet.ru/test page load and extracts random line from div.wguard
func testSarnetPage(ctx context.Context, proxy string) (footerLine string, duration time.Duration, err error) {
	start := time.Now()

	// Check context before starting
	select {
	case <-ctx.Done():
		return "", time.Since(start), fmt.Errorf("operation cancelled")
	default:
	}

	// Create tmpfs directory for Chrome user data
	tmpDir, err := os.MkdirTemp("/tmp", "prx-chrome-*")
	if err != nil {
		return "", time.Since(start), fmt.Errorf("failed to create tmp directory: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Launch Chrome with proxy
	proxyURL := proxy
	if !strings.HasPrefix(proxyURL, "http://") {
		proxyURL = "http://" + proxyURL
	}

	l := launcher.New().
		Bin("google-chrome").
		UserDataDir(tmpDir).
		Proxy(proxyURL).
		Headless(true).
		Set("disable-gpu").
		Set("no-sandbox").
		Set("disable-web-security").
		Set("disable-features", "IsolateOrigins,site-per-process").
		Set("disk-cache-size", "1").
		Set("media-cache-size", "1").
		Set("disable-application-cache").
		Delete("use-mock-keychain").
		Leakless(false)

	// Launch Chrome with timeout
	launchCtx, launchCancel := context.WithTimeout(ctx, 15*time.Second)
	defer launchCancel()

	controlURL, err := l.Context(launchCtx).Launch()
	if err != nil {
		return "", time.Since(start), fmt.Errorf("failed to launch Chrome: %v", err)
	}
	defer l.Cleanup()

	// Connect to Chrome
	browser := rod.New().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		return "", time.Since(start), fmt.Errorf("failed to connect to Chrome: %v", err)
	}
	defer browser.Close()

	// Create page with context
	page, err := browser.Context(ctx).Page(proto.TargetCreateTarget{})
	if err != nil {
		return "", time.Since(start), fmt.Errorf("failed to create page: %v", err)
	}
	defer page.Close()

	// Navigate to sarnet.ru/test
	if err := page.Timeout(pageTimeout).Navigate("https://sarnet.ru/test"); err != nil {
		return "", time.Since(start), fmt.Errorf("failed to navigate to sarnet.ru/test: %v", err)
	}

	if err := page.Timeout(pageTimeout).WaitLoad(); err != nil {
		return "", time.Since(start), fmt.Errorf("failed to load sarnet.ru/test: %v", err)
	}

	// Extract text from div.wguard
	wguard, err := page.Timeout(5 * time.Second).Element("div.wguard")
	if err != nil {
		return "", time.Since(start), fmt.Errorf("failed to find div.wguard: %v", err)
	}

	wguardText, err := wguard.Text()
	if err != nil {
		return "", time.Since(start), fmt.Errorf("failed to get wguard text: %v", err)
	}

	// Split wguard text into lines and pick a random non-empty line
	lines := strings.Split(wguardText, "\n")
	var nonEmptyLines []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			nonEmptyLines = append(nonEmptyLines, line)
		}
	}

	if len(nonEmptyLines) == 0 {
		return "", time.Since(start), fmt.Errorf("no lines found in div.wguard")
	}

	randomLine := nonEmptyLines[rand.IntN(len(nonEmptyLines))]

	// Success!
	return randomLine, time.Since(start), nil
}

// testPageLoad tests real page load using DuckDuckGo search (like bot does)
func testPageLoad(ctx context.Context, proxy string) (duration time.Duration, err error) {
	start := time.Now()

	// Check context before starting
	select {
	case <-ctx.Done():
		return time.Since(start), fmt.Errorf("operation cancelled")
	default:
	}

	// Create tmpfs directory for Chrome user data
	tmpDir, err := os.MkdirTemp("/tmp", "prx-chrome-*")
	if err != nil {
		return time.Since(start), fmt.Errorf("failed to create tmp directory: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Launch Chrome with proxy
	proxyURL := proxy
	if !strings.HasPrefix(proxyURL, "http://") {
		proxyURL = "http://" + proxyURL
	}

	l := launcher.New().
		Bin("google-chrome").
		UserDataDir(tmpDir).
		Proxy(proxyURL).
		Headless(true).
		Set("disable-gpu").
		Set("no-sandbox").
		Set("disable-web-security").
		Set("disable-features", "IsolateOrigins,site-per-process").
		Set("disk-cache-size", "1").
		Set("media-cache-size", "1").
		Set("disable-application-cache").
		Delete("use-mock-keychain").
		Leakless(false)

	// Launch Chrome with timeout
	launchCtx, launchCancel := context.WithTimeout(ctx, 15*time.Second)
	defer launchCancel()

	controlURL, err := l.Context(launchCtx).Launch()
	if err != nil {
		return time.Since(start), fmt.Errorf("failed to launch Chrome: %v", err)
	}
	defer l.Cleanup()

	// Connect to Chrome
	browser := rod.New().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		return time.Since(start), fmt.Errorf("failed to connect to Chrome: %v", err)
	}
	defer browser.Close()

	// Create page with context
	page, err := browser.Context(ctx).Page(proto.TargetCreateTarget{})
	if err != nil {
		return time.Since(start), fmt.Errorf("failed to create page: %v", err)
	}
	defer page.Close()

	// Navigate to DuckDuckGo
	if err := page.Timeout(pageTimeout).Navigate("https://duckduckgo.com/"); err != nil {
		return time.Since(start), fmt.Errorf("failed to navigate to DuckDuckGo: %v", err)
	}

	if err := page.Timeout(pageTimeout).WaitLoad(); err != nil {
		return time.Since(start), fmt.Errorf("failed to load DuckDuckGo: %v", err)
	}

	// Random search keyword
	keyword := searchKeywords[rand.IntN(len(searchKeywords))]

	// Find search input and type keyword
	searchInput, err := page.Timeout(5 * time.Second).Element("input[name='q']")
	if err != nil {
		return time.Since(start), fmt.Errorf("failed to find search input: %v", err)
	}

	// Type keyword
	if err := searchInput.Input(keyword); err != nil {
		return time.Since(start), fmt.Errorf("failed to type search: %v", err)
	}

	// Press Enter to submit search
	if err := searchInput.Type(input.Enter); err != nil {
		return time.Since(start), fmt.Errorf("failed to submit search: %v", err)
	}

	// Wait for search results page to load
	time.Sleep(2 * time.Second) // Give time for navigation to start
	if err := page.Timeout(pageTimeout).WaitLoad(); err != nil {
		return time.Since(start), fmt.Errorf("failed to load search results: %v", err)
	}

	// Find all result links (DuckDuckGo uses a[data-testid] for result links)
	links, err := page.Timeout(5 * time.Second).Elements("a[href]")
	if err != nil {
		return time.Since(start), fmt.Errorf("failed to find links: %v", err)
	}

	if len(links) == 0 {
		return time.Since(start), fmt.Errorf("no links found in search results")
	}

	// Filter for result links (exclude navigation, ads, etc)
	var resultLinks []*rod.Element
	for _, link := range links {
		href, err := link.Attribute("href")
		if err != nil || href == nil {
			continue
		}
		hrefStr := *href
		// Skip internal DuckDuckGo links, ads, and anchors
		if strings.Contains(hrefStr, "duckduckgo.com") || strings.HasPrefix(hrefStr, "#") || strings.HasPrefix(hrefStr, "javascript:") {
			continue
		}
		// Only external links (search results)
		if strings.HasPrefix(hrefStr, "http://") || strings.HasPrefix(hrefStr, "https://") {
			resultLinks = append(resultLinks, link)
		}
	}

	if len(resultLinks) == 0 {
		return time.Since(start), fmt.Errorf("no result links found")
	}

	// Click random result link
	randomLink := resultLinks[rand.IntN(len(resultLinks))]

	// Get href for logging (optional)
	href, _ := randomLink.Attribute("href")
	targetURL := "unknown"
	if href != nil {
		targetURL = *href
		// Truncate long URLs for logging
		if len(targetURL) > 50 {
			targetURL = targetURL[:47] + "..."
		}
	}

	if err := randomLink.Click("left", 1); err != nil {
		return time.Since(start), fmt.Errorf("failed to click link: %v", err)
	}

	// Wait for final page to load (this is the real test matching bot behavior)
	if err := page.Timeout(pageTimeout).WaitLoad(); err != nil {
		return time.Since(start), fmt.Errorf("failed to load final page (%s): %v", targetURL, err)
	}

	// Success!
	return time.Since(start), nil
}

// getFailedFilePath returns the path to failed.txt in the temp directory
func getFailedFilePath() (string, error) {
	return filepath.Join(os.TempDir(), "failed.txt"), nil
}

// runQuietMode runs all checks without TUI
func runQuietMode(proxies []string) {
	ctx := context.Background()

	// Proxies check
	proxiesTotal := len(proxies)
	proxiesChecked := 0
	proxiesWorking := 0
	var proxiesFailed []string

	results := make(chan proxyResult, maxWorkers)
	tasks := make(chan string, len(proxies))

	// Enqueue tasks
	for _, proxy := range proxies {
		tasks <- proxy
	}
	close(tasks)

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < maxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for proxy := range tasks {
				checkProxy(ctx, proxy, results, maxMindReader, requestURL)
			}
		}()
	}

	// Goroutine to close results channel
	go func() {
		wg.Wait()
		close(results)
	}()

	// Process results
	pageLoadWorking := 0
	pageLoadFailed := 0
	var firstPageLoadErr error
	for res := range results {
		proxiesChecked++
		if res.err == nil {
			proxiesWorking++
			// Track page load stats if enabled
			if pageLoadMode {
				if res.pageLoadOK {
					pageLoadWorking++
				} else {
					pageLoadFailed++
					// Save first page load error for debugging
					if firstPageLoadErr == nil && res.pageLoadErr != nil {
						firstPageLoadErr = res.pageLoadErr
					}
				}
			}
		} else {
			proxiesFailed = append(proxiesFailed, fmt.Sprintf("%s %s", res.proxy, getShortError(res.err)))
		}
	}

	// Save failed results to file
	failedFile, err := getFailedFilePath()
	if err == nil {
		var failedContent string
		if len(proxiesFailed) > 0 {
			failedContent = strings.Join(proxiesFailed, "\n")
		}
		os.WriteFile(failedFile, []byte(failedContent), 0644)
	}

	// Output stats (compact format for quiet mode)
	if pageLoadMode {
		fmt.Printf("Total: %d Checked: %d Working: %d Failed: %d | PageLoad: OK=%d FAIL=%d (%.1f%%)\n",
			proxiesTotal, proxiesChecked, proxiesWorking, len(proxiesFailed),
			pageLoadWorking, pageLoadFailed,
			float64(pageLoadWorking)/float64(proxiesWorking)*100)
		// Print first page load error for debugging
		if firstPageLoadErr != nil {
			fmt.Printf("First page load error: %v\n", firstPageLoadErr)
		}
	} else {
		fmt.Printf("Total: %d Checked: %d Working: %d Failed: %d\n",
			proxiesTotal, proxiesChecked, proxiesWorking, len(proxiesFailed))
	}
}

// Main function
func main() {
	// Parse command line flags
	quietMode := flag.Bool("q", false, "quiet mode - run tests without TUI")
	flag.BoolVar(quietMode, "quiet", false, "quiet mode - run tests without TUI")
	pageLoadFlag := flag.Bool("p", false, "page load mode - test real page loading via DuckDuckGo")
	flag.BoolVar(pageLoadFlag, "page", false, "page load mode - test real page loading via DuckDuckGo")
	sarnetFlag := flag.Bool("s", false, "sarnet mode - test sarnet.ru/test through proxy and show random footer line")
	flag.BoolVar(sarnetFlag, "sarnet", false, "sarnet mode - test sarnet.ru/test through proxy and show random footer line")
	flag.Parse()

	// Set global page load mode and sarnet mode
	if *sarnetFlag {
		sarnetMode = true
		pageLoadMode = false // Sarnet mode takes precedence
	} else {
		pageLoadMode = *pageLoadFlag
	}

	// Open GeoIP database
	var err error
	maxMindReader, err = geoip2.Open(geoDBMaxMind)
	if err != nil {
		log.Fatalf("Error opening GeoIP database: %v", err)
	}
	defer maxMindReader.Close()

	tr := &http.Transport{
		Proxy: nil, // Explicitly set Proxy to nil to disable environment proxy
	}

	// Create a custom Client using the custom Transport
	httpClient := &http.Client{
		Transport: tr,
		Timeout:   httpTimeout,
	}

	// Fetch serverID from URL
	respID, err := httpClient.Get(serverIDURL)
	if err != nil {
		log.Fatalf("Error fetching serverID from %s: %v", serverIDURL, err)
	}
	defer respID.Body.Close()

	if respID.StatusCode != http.StatusOK {
		log.Fatalf("Error fetching serverID: status %d", respID.StatusCode)
	}

	bodyID, err := io.ReadAll(respID.Body)
	if err != nil {
		log.Fatalf("Error reading serverID: %v", err)
	}
	serverID = strings.TrimSpace(string(bodyID))
	if serverID == "" {
		log.Fatalf("Error: Empty serverID from %s", serverIDURL)
	}

	// Construct requestURL
	requestURL = "http://s" + serverID + ".sarnet.ru/ip"
	if serverID == "00" {
		requestURL = "http://213.234.252.212/ip"
	}

	// Fetch proxy list from URL
	resp, err := httpClient.Get(proxiesURL)
	if err != nil {
		log.Fatalf("Error fetching proxy list from %s: %v", proxiesURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Fatalf("Error fetching proxy list: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Error reading proxy list: %v", err)
	}

	lines := strings.Split(string(body), "\n")
	var proxies []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			proxies = append(proxies, fields[0])
		}
	}

	total := len(proxies)
	if total == 0 {
		log.Println("No proxies found from URL.")
		return
	}

	// Run in quiet mode if flag is set
	if *quietMode {
		runQuietMode(proxies)
		return
	}

	// Initialize model with single viewport
	ctx, cancel := context.WithCancel(context.Background())
	m := model{
		proxies:         proxies,
		proxiesTotal:    total,
		proxiesProgress: progress.New(progress.WithDefaultGradient()),
		proxiesViewport: viewport.New(0, 0), // Will be set by WindowSizeMsg
		proxiesResults:  make([]string, 0),
		proxiesFailed:   make([]string, 0),
		ctx:             ctx,
		cancel:          cancel,
	}

	// Start Bubble Tea program
	teaProgram = tea.NewProgram(&m, tea.WithAltScreen())

	// Start proxy check automatically on startup
	go func() {
		m.checkProxies()
	}()

	if _, err := teaProgram.Run(); err != nil {
		log.Fatalf("Error running TUI: %v", err)
	}

	// Output stats (compact format with colors)
	// ANSI color codes: white=37, cyan=36, green=32, red=31
	fmt.Printf("\033[37mTotal\033[0m %d, \033[36mChecked\033[0m %d, \033[32mWorking\033[0m %d, \033[31mFailed\033[0m %d\n",
		m.proxiesTotal, m.proxiesChecked, m.proxiesWorking, len(m.proxiesFailed))

	// Get path to failed file
	failedFile, err := getFailedFilePath()
	if err != nil {
		log.Fatalf("Error getting home directory: %v", err)
	}

	// Write to file
	var failedContent string
	if len(m.proxiesFailed) > 0 {
		failedContent = strings.Join(m.proxiesFailed, "\n")
	}

	err = os.WriteFile(failedFile, []byte(failedContent), 0644)
	if err != nil {
		log.Printf("Error saving failed results: %v", err)
	}
}

// runChecks starts the workers and processes proxies
func runChecks(m *model, p *tea.Program, maxMindReader *geoip2.Reader, requestURL string) {
	tasks := make(chan string, len(m.proxies))
	results := make(chan proxyResult, maxWorkers) // Буферизованный канал для предотвращения блокировок

	// Enqueue tasks
	for _, proxy := range m.proxies {
		tasks <- proxy
	}
	close(tasks)

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < maxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for proxy := range tasks {
				checkProxy(m.ctx, proxy, results, maxMindReader, requestURL)
			}
		}()
	}

	// Goroutine to close results channel after all workers finish
	go func() {
		wg.Wait()
		close(results)
	}()

	// Process results and send to TUI in real-time
	for res := range results {
		p.Send(res)
	}

	// No auto-quit: wait for user to press q
}
