package worker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func relayToBase(relay string) (string, string, string, string, error) {
	u, err := url.Parse(strings.TrimSpace(relay))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", "", "", fmt.Errorf("invalid relay url: %q", relay)
	}
	scheme := u.Scheme
	host := u.Hostname()
	port := u.Port()
	baseScheme := "http"
	if scheme == "wss" {
		baseScheme = "https"
	}
	if port == "" {
		if baseScheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	base := baseScheme + "://" + net.JoinHostPort(host, port)
	return base, baseScheme, host, port, nil
}

func httpGet(ctx context.Context, url string, tr *http.Transport) (int, error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

func httpGetDetail(ctx context.Context, url string, tr *http.Transport) (int, http.Header, error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, resp.Header.Clone(), nil
}

func checkDNS(host string) (bool, []string, error) {
	addrs, err := net.LookupHost(host)
	if err != nil {
		return false, nil, err
	}
	return true, addrs, nil
}

func checkTCP(host, port string) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 4*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func checkTLS(host, port string) error {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, port), &tls.Config{ServerName: host})
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func classifyErr(err error) string {
	if err == nil {
		return "ok"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such host"):
		return "dns"
	case strings.Contains(msg, "connect: connection refused"), strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "timeout"):
		return "tcp"
	case strings.Contains(msg, "x509"), strings.Contains(msg, "certificate"):
		return "tls"
	case strings.Contains(msg, "proxy"), strings.Contains(msg, "407"):
		return "proxy"
	default:
		return "other"
	}
}

func wsURLFromRelay(relay string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(relay))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid relay url: %q", relay)
	}
	u.Path = "/v1/worker/connect"
	return u.String(), nil
}

func loadWorkerTokenFromState(stateDir string) string {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			stateDir = filepathJoin(home, ".nano-cloud", "state")
		}
	}
	b, err := os.ReadFile(filepathJoin(stateDir, "state.json"))
	if err != nil {
		return ""
	}
	type state struct {
		WorkerToken string `json:"worker_token"`
	}
	var s state
	if err := jsonUnmarshal(b, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s.WorkerToken)
}

func wsHandshakeProbe(ctx context.Context, wsURL string, token string) (string, http.Header, error) {
	h := http.Header{}
	if strings.TrimSpace(token) != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 8 * time.Second,
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, h)
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				_ = drainAndClose(resp.Body)
				return "unauthorized", resp.Header.Clone(), nil
			default:
				_ = drainAndClose(resp.Body)
				return fmt.Sprintf("http-%d", resp.StatusCode), resp.Header.Clone(), nil
			}
		}
		return "", nil, err
	}
	_ = conn.Close()
	var hdr http.Header
	if resp != nil {
		hdr = resp.Header.Clone()
	}
	return "ok", hdr, nil
}

func drainAndClose(r io.ReadCloser) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 1024))
	return r.Close()
}

func PrintGatewayDiagnostics(ctx context.Context, relay string, stateDir string, verbose bool, asJSON bool) { //nolint:revive
	base, scheme, host, port, err := relayToBase(relay)
	if err != nil {
		if asJSON {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
				"relay": relay, "error": err.Error(),
			})
		} else {
			fmt.Println()
			fmt.Println("--- Gateway Diagnostics ---")
			fmt.Println("Relay URL:", relay)
			fmt.Println("Parse error:", err)
			fmt.Println("---------------------------")
		}
		return
	}
	out := map[string]any{
		"relay":  relay,
		"base":   base,
		"host":   host,
		"port":   port,
		"scheme": scheme,
		"env": map[string]string{
			"http_proxy":  os.Getenv("HTTP_PROXY"),
			"https_proxy": os.Getenv("HTTPS_PROXY"),
			"no_proxy":    os.Getenv("NO_PROXY"),
		},
	}
	if !asJSON {
		fmt.Println()
		fmt.Println("--- Gateway Diagnostics ---")
		fmt.Println("Relay URL:", relay)
		fmt.Println("HTTP Base:", base)
		fmt.Println("Host:", host, "Port:", port, "Scheme:", scheme)
		fmt.Println("HTTP_PROXY:", os.Getenv("HTTP_PROXY"))
		fmt.Println("HTTPS_PROXY:", os.Getenv("HTTPS_PROXY"))
		fmt.Println("NO_PROXY:", os.Getenv("NO_PROXY"))
	}

	okDNS, addrs, dnsErr := checkDNS(host)
	if asJSON {
		out["dns"] = map[string]any{"ok": okDNS, "addrs": addrs, "error": errString(dnsErr), "class": classifyErr(dnsErr)}
	} else {
		if okDNS {
			fmt.Println("DNS:", "OK", strings.Join(addrs, ","))
		} else {
			fmt.Println("DNS:", "FAIL", dnsErr)
		}
	}

	if dnsErr == nil {
		if err := checkTCP(host, port); err != nil {
			if asJSON {
				out["tcp"] = map[string]any{"ok": false, "error": err.Error(), "class": classifyErr(err)}
			} else {
				fmt.Println("TCP:", "FAIL", classifyErr(err), err)
			}
		} else {
			if asJSON {
				out["tcp"] = map[string]any{"ok": true}
			} else {
				fmt.Println("TCP:", "OK")
			}
		}
	}

	if scheme == "https" {
		if err := checkTLS(host, port); err != nil {
			if asJSON {
				out["tls"] = map[string]any{"ok": false, "error": err.Error(), "class": classifyErr(err)}
			} else {
				fmt.Println("TLS:", "FAIL", classifyErr(err), err)
			}
		} else {
			if asJSON {
				out["tls"] = map[string]any{"ok": true}
			} else {
				fmt.Println("TLS:", "OK")
			}
		}
	}

	trEnv := http.DefaultTransport.(*http.Transport).Clone()
	var sEnv int
	var hEnv http.Header
	var errEnv error
	if verbose {
		sEnv, hEnv, errEnv = httpGetDetail(ctx, base+"/console", trEnv)
	} else {
		sEnv, errEnv = httpGet(ctx, base+"/console", trEnv)
	}
	if asJSON {
		out["http_env"] = map[string]any{
			"ok":      errEnv == nil,
			"status":  sEnv,
			"error":   errString(errEnv),
			"class":   classifyErr(errEnv),
			"headers": headerToMap(hEnv),
		}
	} else {
		if errEnv != nil {
			fmt.Println("HTTP /console (env-proxy):", "FAIL", classifyErr(errEnv), errEnv)
		} else {
			fmt.Println("HTTP /console (env-proxy):", sEnv)
			if verbose {
				for k, v := range hEnv {
					fmt.Printf("  H %s: %s\n", k, strings.Join(v, ","))
				}
			}
		}
	}

	trNoProxy := http.DefaultTransport.(*http.Transport).Clone()
	trNoProxy.Proxy = nil
	var sNP int
	var hNP http.Header
	var errNP error
	if verbose {
		sNP, hNP, errNP = httpGetDetail(ctx, base+"/console", trNoProxy)
	} else {
		sNP, errNP = httpGet(ctx, base+"/console", trNoProxy)
	}
	if asJSON {
		out["http_no_proxy"] = map[string]any{
			"ok":      errNP == nil,
			"status":  sNP,
			"error":   errString(errNP),
			"class":   classifyErr(errNP),
			"headers": headerToMap(hNP),
		}
	} else {
		if errNP != nil {
			fmt.Println("HTTP /console (no-proxy):", "FAIL", classifyErr(errNP), errNP)
		} else {
			fmt.Println("HTTP /console (no-proxy):", sNP)
			if verbose {
				for k, v := range hNP {
					fmt.Printf("  H %s: %s\n", k, strings.Join(v, ","))
				}
			}
		}
	}

	if errEnv != nil && errNP == nil {
		suggest := host
		if host != "localhost" && host != "127.0.0.1" {
			suggest = "localhost,127.0.0.1," + host
		}
		if asJSON {
			out["no_proxy_suggestion"] = suggest
		} else {
			fmt.Println("Suggestion: set NO_PROXY to include:", suggest)
		}
	}

	wsURL, wserr := wsURLFromRelay(relay)
	if wserr == nil {
		token := loadWorkerTokenFromState(stateDir)
		status, hdr, wsErr := wsHandshakeProbe(ctx, wsURL, token)
		if asJSON {
			out["ws"] = map[string]any{
				"ok":      wsErr == nil,
				"status":  status,
				"error":   errString(wsErr),
				"class":   classifyErr(wsErr),
				"headers": headerToMap(hdr),
			}
		} else {
			if wsErr != nil {
				fmt.Println("WS /v1/worker/connect:", "FAIL", classifyErr(wsErr), wsErr)
			} else {
				fmt.Println("WS /v1/worker/connect:", status)
				if verbose && hdr != nil {
					for k, v := range hdr {
						fmt.Printf("  H %s: %s\n", k, strings.Join(v, ","))
					}
				}
			}
		}
	}

	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(out)
	} else {
		fmt.Println("---------------------------")
	}
}

// local helpers to avoid importing extra modules in main package
func jsonUnmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}

func filepathJoin(parts ...string) string {
	return strings.Join(parts, string(os.PathSeparator))
}

func headerToMap(h http.Header) map[string][]string {
	if h == nil {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type runInfo struct {
	RunID   string    `json:"run_id"`
	Dir     string    `json:"dir"`
	ModTime time.Time `json:"-"`
	ModStr  string    `json:"mod_time"`
	HasErr  bool      `json:"has_stderr"`
}

// PrintAgentLogsDiagnostics lists recent agent runs from the log root directory
// and shows a tail of the most recent run's stderr log.  When logRoot is empty
// it falls back to scanning the workspace root.
func PrintAgentLogsDiagnostics(logRoot string, workspaceRoot string, verbose bool) { //nolint:revive

	collectFromLogRoot := func(root string) ([]runInfo, error) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		var runs []runInfo
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepathJoin(root, e.Name())
			info, _ := e.Info()
			var modTime time.Time
			if info != nil {
				modTime = info.ModTime()
			}
			// Try reading run metadata
			runID := e.Name()
			if b, err := os.ReadFile(filepathJoin(dir, ".nano-run.json")); err == nil {
				var meta struct {
					RunID string `json:"run_id"`
				}
				if err := json.Unmarshal(b, &meta); err == nil && meta.RunID != "" {
					runID = meta.RunID
				}
			}
			hasStderr := false
			if fi, err := os.Stat(filepathJoin(dir, "agent.stderr.log")); err == nil && fi.Size() > 0 {
				hasStderr = true
			}
			runs = append(runs, runInfo{
				RunID:   runID,
				Dir:     dir,
				ModTime: modTime,
				ModStr:  modTime.Format("2006-01-02 15:04:05"),
				HasErr:  hasStderr,
			})
		}
		return runs, nil
	}

	collectFromWorkspace := func(root string) ([]runInfo, error) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		var runs []runInfo
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepathJoin(root, e.Name())
			// Only include dirs with agent logs
			if _, err := os.Stat(filepathJoin(dir, "agent.stdout.log")); err != nil {
				continue
			}
			info, _ := e.Info()
			var modTime time.Time
			if info != nil {
				modTime = info.ModTime()
			}
			runID := e.Name()
			if b, err := os.ReadFile(filepathJoin(dir, ".nano-workspace.json")); err == nil {
				var meta struct {
					RunID string `json:"run_id"`
				}
				if err := json.Unmarshal(b, &meta); err == nil && meta.RunID != "" {
					runID = meta.RunID
				}
			}
			hasStderr := false
			if fi, err := os.Stat(filepathJoin(dir, "agent.stderr.log")); err == nil && fi.Size() > 0 {
				hasStderr = true
			}
			runs = append(runs, runInfo{
				RunID:   runID,
				Dir:     dir,
				ModTime: modTime,
				ModStr:  modTime.Format("2006-01-02 15:04:05"),
				HasErr:  hasStderr,
			})
		}
		return runs, nil
	}

	var runs []runInfo
	var collectErr error
	source := ""
	if logRoot != "" {
		runs, collectErr = collectFromLogRoot(logRoot)
		source = logRoot
	}
	if len(runs) == 0 && workspaceRoot != "" {
		runs, collectErr = collectFromWorkspace(workspaceRoot)
		source = workspaceRoot
	}

	// Sort by mod time descending (most recent first)
	sortRunsByModTime(runs)

	const maxDisplay = 10
	if len(runs) > maxDisplay {
		runs = runs[:maxDisplay]
	}

	fmt.Println()
	fmt.Println("--- Agent Logs ---")
	if source != "" {
		fmt.Println("Source:", source)
	}

	if collectErr != nil && verbose {
		fmt.Fprintf(os.Stderr, "Warning: failed to read %s: %v\n", source, collectErr)
	}

	if len(runs) == 0 {
		if logRoot == "" && workspaceRoot == "" {
			fmt.Println("No log-root or workspace-root configured.")
		} else {
			fmt.Println("No agent runs found.")
		}
		fmt.Println("------------------")
		return
	}

	fmt.Printf("Recent runs (last %d):\n", len(runs))
	for _, r := range runs {
		errMark := ""
		if r.HasErr {
			errMark = " [stderr]"
		}
		fmt.Printf("  %s  %s%s\n", r.ModStr, r.RunID, errMark)
	}

	// Show tail of most recent run's stderr if it has errors
	if runs[0].HasErr || verbose {
		stderrPath := filepathJoin(runs[0].Dir, "agent.stderr.log")
		if tail := tailFile(stderrPath, 20); tail != "" {
			fmt.Printf("\nLatest run stderr (%s):\n", runs[0].RunID)
			fmt.Println(tail)
		}
	}

	if verbose && len(runs) > 0 {
		stdoutPath := filepathJoin(runs[0].Dir, "agent.stdout.log")
		if tail := tailFile(stdoutPath, 20); tail != "" {
			fmt.Printf("\nLatest run stdout (%s):\n", runs[0].RunID)
			fmt.Println(tail)
		}
	}

	fmt.Println("------------------")
}

func sortRunsByModTime(runs []runInfo) {
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].ModTime.After(runs[j].ModTime)
	})
}

func tailFile(path string, lines int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return ""
	}

	const maxReadSize int64 = 1024 * 1024 // read at most 1 MiB from the end
	size := info.Size()
	if size == 0 {
		return ""
	}
	start := int64(0)
	if size > maxReadSize {
		start = size - maxReadSize
	}

	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return ""
		}
	}

	b, err := io.ReadAll(f)
	if err != nil || len(b) == 0 {
		return ""
	}

	content := strings.TrimRight(string(b), "\n")
	allLines := strings.Split(content, "\n")
	lineStart := len(allLines) - lines
	if lineStart < 0 {
		lineStart = 0
	}
	return strings.Join(allLines[lineStart:], "\n")
}
