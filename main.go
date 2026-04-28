package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Connections []ConnectionConfig `yaml:"connections"`
}

type ConnectionConfig struct {
	Name             string `yaml:"name"`
	User             string `yaml:"user"`
	Host             string `yaml:"host"`
	CloudSQLInstance string `yaml:"cloudsql_instance"`
	LocalPort        int    `yaml:"local_port"`
	SocketPath       string `yaml:"socket_path"`
}

type ConnectionState struct {
	Config      ConnectionConfig
	TunnelCmd   *exec.Cmd
	TunnelPTY   *os.File
	TunnelIO    *commandIO
	LastError   string
	LastUpdated time.Time
	Logf        LogFunc
	Prompter    *PasswordPrompter
}

type LogFunc func(format string, args ...any)

const (
	cloudSQLAccessProgressInterval = 5 * time.Second
	cloudSQLAccessTimeout          = 90 * time.Second
)

func main() {
	if os.Getenv("LAZY_JUMPHOST_ASKPASS") == "1" {
		fmt.Print(os.Getenv("LAZY_JUMPHOST_ASKPASS_PASSWORD"))
		return
	}

	configPath := flag.String("config", "config.yaml", "Path to YAML config")
	debugMode := flag.Bool("debug", false, "Show debug logs in the UI")
	logFilePath := flag.String("log-file", "lazy-jumphost-debug.txt", "Path to debug log file; set empty to disable file logging")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	states := make([]*ConnectionState, 0, len(cfg.Connections))
	for _, conn := range cfg.Connections {
		states = append(states, &ConnectionState{Config: conn})
	}

	app := tview.NewApplication()
	list := tview.NewList()
	list.ShowSecondaryText(false)
	list.SetBorder(true).SetTitle("Connections")

	details := tview.NewTextView()
	details.SetDynamicColors(true)
	details.SetBorder(true).SetTitle("Status")

	statusBar := tview.NewTextView()
	statusBar.SetDynamicColors(true)
	statusBar.SetBorder(true).SetTitle("Messages")

	helpBar := tview.NewTextView()
	helpBar.SetBorder(true).SetTitle("Keys")
	helpBar.SetText("s=start  x=stop  r=refresh  q=quit")

	var logView *tview.TextView
	var debugLogf LogFunc
	var debugLogFile *os.File
	if *debugMode || *logFilePath != "" {
		var err error
		if *logFilePath != "" {
			debugLogFile, err = os.OpenFile(*logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to open debug log file: %v\n", err)
				os.Exit(1)
			}
			defer func() {
				_ = debugLogFile.Close()
			}()
		}
	}
	if *debugMode {
		logView = tview.NewTextView()
		logView.SetDynamicColors(true)
		logView.SetBorder(true).SetTitle("Debug Logs")
	}
	if *debugMode || debugLogFile != nil {
		logCh := make(chan string, 200)
		debugLogf = func(format string, args ...any) {
			message := stripANSICodes(fmt.Sprintf(format, args...))
			logCh <- message
		}
		go func() {
			for message := range logCh {
				if debugLogFile != nil {
					timestamp := time.Now().Format(time.RFC3339)
					_, _ = fmt.Fprintf(debugLogFile, "%s %s\n", timestamp, message)
				}
				if logView != nil {
					app.QueueUpdateDraw(func() {
						fmt.Fprintln(logView, message)
						logView.ScrollToEnd()
					})
				}
			}
		}()
		if *debugMode {
			debugLogf("debug mode enabled")
		} else {
			debugLogf("file logging enabled")
		}
		if debugLogFile != nil {
			debugLogf("debug log file: %s", *logFilePath)
		}
		debugLogf("log writer ready")
	}

	for _, state := range states {
		state.Logf = debugLogf
	}

	for _, state := range states {
		list.AddItem(state.Config.Name, "", 0, nil)
	}

	refresh := func() {
		for i, state := range states {
			running := state.IsRunning()
			status := "[red]stopped"
			if running {
				status = "[green]running"
			}
			label := fmt.Sprintf("%s [%s]", state.Config.Name, status)
			list.SetItemText(i, label, "")
		}
		updateDetails(details, list.GetCurrentItem(), states)
	}

	list.SetChangedFunc(func(index int, _ string, _ string, _ rune) {
		updateDetails(details, index, states)
	})

	startSelected := func() {
		index := list.GetCurrentItem()
		if index < 0 || index >= len(states) {
			return
		}
		state := states[index]
		app.QueueUpdateDraw(func() {
			statusBar.SetText("Starting connection...")
		})
		go func() {
			err := state.Start()
			app.QueueUpdateDraw(func() {
				if err != nil {
					statusBar.SetText(fmt.Sprintf("[red]Start failed: %s", err))
				} else {
					statusBar.SetText("[green]Connection started")
				}
				refresh()
			})
		}()
	}

	stopSelected := func() {
		index := list.GetCurrentItem()
		if index < 0 || index >= len(states) {
			return
		}
		state := states[index]
		app.QueueUpdateDraw(func() {
			statusBar.SetText("Stopping connection...")
		})
		go func() {
			err := state.Stop()
			app.QueueUpdateDraw(func() {
				if err != nil {
					statusBar.SetText(fmt.Sprintf("[red]Stop failed: %s", err))
				} else {
					statusBar.SetText("[green]Connection stopped")
				}
				refresh()
			})
		}()
	}

	refreshSelected := func() {
		refresh()
		statusBar.SetText("Status refreshed")
	}

	quitSelected := func() {
		for _, state := range states {
			_ = state.Stop()
		}
		app.Stop()
	}

	layout := tview.NewFlex().SetDirection(tview.FlexRow)
	mainRow := tview.NewFlex().SetDirection(tview.FlexColumn)
	mainRow.AddItem(list, 0, 1, true)
	mainRow.AddItem(details, 0, 2, false)

	layout.AddItem(mainRow, 0, 1, true)
	layout.AddItem(statusBar, 3, 0, false)
	layout.AddItem(helpBar, 3, 0, false)
	if logView != nil {
		layout.AddItem(logView, 8, 0, false)
	}

	pages := tview.NewPages()
	pages.AddPage("main", layout, true, true)
	prompter := NewPasswordPrompter(app, pages)
	for _, state := range states {
		state.Prompter = prompter
	}

	refresh()

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if prompter != nil && prompter.IsActive() {
			return event
		}
		switch event.Rune() {
		case 's':
			go startSelected()
			return nil
		case 'x':
			go stopSelected()
			return nil
		case 'r':
			go app.QueueUpdateDraw(refreshSelected)
			return nil
		case 'q':
			go app.QueueUpdateDraw(quitSelected)
			return nil
		}
		return event
	})

	quitSignals := make(chan os.Signal, 1)
	if runtime.GOOS == "windows" {
		signal.Notify(quitSignals, os.Interrupt)
	} else {
		signal.Notify(quitSignals, os.Interrupt, syscall.SIGTERM)
	}
	go func() {
		<-quitSignals
		app.QueueUpdateDraw(func() {
			for _, state := range states {
				_ = state.Stop()
			}
			app.Stop()
		})
	}()

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			app.QueueUpdateDraw(refresh)
		}
	}()

	if err := app.SetRoot(pages, true).EnableMouse(true).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run app: %v\n", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if len(cfg.Connections) == 0 {
		return nil, errors.New("no connections defined")
	}

	for i, conn := range cfg.Connections {
		if conn.Name == "" || conn.User == "" || conn.Host == "" || conn.CloudSQLInstance == "" || conn.LocalPort == 0 {
			return nil, fmt.Errorf("connection %d has missing required fields", i+1)
		}
	}

	return &cfg, nil
}

func updateDetails(view *tview.TextView, index int, states []*ConnectionState) {
	if index < 0 || index >= len(states) {
		view.SetText("No connection selected")
		return
	}
	state := states[index]
	cfg := state.Config
	status := "stopped"
	if state.IsRunning() {
		status = "running"
	}
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Name: %s\n", cfg.Name))
	builder.WriteString(fmt.Sprintf("Host: %s@%s\n", cfg.User, cfg.Host))
	builder.WriteString(fmt.Sprintf("CloudSQL: %s\n", cfg.CloudSQLInstance))
	builder.WriteString(fmt.Sprintf("Local Port: %d\n", cfg.LocalPort))
	builder.WriteString(fmt.Sprintf("Status: %s\n", status))
	if state.LastError != "" {
		builder.WriteString(fmt.Sprintf("Last Error: %s\n", state.LastError))
	}
	view.SetText(builder.String())
}

func (state *ConnectionState) Start() error {
	if state.IsRunning() {
		return errors.New("connection already running")
	}
	if err := ensureLocalPortAvailable(state.Config.LocalPort); err != nil {
		state.LastError = err.Error()
		state.LastUpdated = time.Now()
		state.logf("local port check failed: %v", err)
		return err
	}

	passwordCache := NewPasswordCache("")
	if state.Prompter != nil {
		entered, ok := state.Prompter.PromptOptional("SSH password (leave blank to use keys)")
		if !ok {
			return errors.New("start cancelled")
		}
		if entered != "" {
			passwordCache.Set(entered)
		}
	}
	if passwordCache.Get() != "" {
		state.logf("ssh password provided for non-interactive auth")
	} else {
		state.logf("no ssh password provided; relying on ssh keys or agent")
	}

	state.LastError = ""
	state.LastUpdated = time.Now()
	state.logf("starting connection %s on %s/%s", state.Config.Name, runtime.GOOS, runtime.GOARCH)
	state.logf("cloudsql_access starting for %s via %s", state.Config.CloudSQLInstance, state.remoteHost())
	if err := runCloudSQLAccess(state.Config, state.logf, state.Prompter, NewPasswordResponder(passwordCache)); err != nil {
		state.LastError = err.Error()
		state.logf("cloudsql_access failed for %s: %v", state.Config.Name, err)
		return err
	}
	state.logf("cloudsql_access completed for %s", state.Config.Name)

	localSpec := fmt.Sprintf("127.0.0.1:%d:%s", state.Config.LocalPort, escapeColons(state.remoteSocketPath()))
	cmd := exec.Command("ssh", append(sshArgs(), "-N", "-T", "-L", localSpec, state.remoteHost())...)
	state.logf("ssh tunnel starting: ssh -N -T -L %s %s", localSpec, state.remoteHost())
	askpassCleanup, err := configureSSHAskpass(cmd, "ssh tunnel", passwordCache.Get(), state.logf)
	if err != nil {
		state.LastError = err.Error()
		state.logf("failed to configure ssh askpass: %v", err)
		return err
	}
	cmdIO, err := startCommandIO("ssh tunnel", cmd, state.logf)
	if err != nil {
		askpassCleanup()
		state.LastError = err.Error()
		state.logf("failed to start ssh: %v", err)
		return err
	}
	cmdIO.addCleanup(askpassCleanup)

	state.TunnelCmd = cmd
	state.TunnelPTY = cmdIO.ptyFile
	state.TunnelIO = cmdIO
	state.LastError = ""
	state.LastUpdated = time.Now()
	state.logf("ssh command: ssh -N -T -L %s %s", localSpec, state.remoteHost())

	go monitorPTYOutput("ssh tunnel", cmdIO.reader, state.Logf, state.Prompter, cmdIO.writer, NewPasswordResponder(passwordCache), nil)

	go func() {
		err := cmdIO.Wait()
		if err != nil {
			state.LastError = err.Error()
			state.logf("ssh exited with error: %v", err)
		} else {
			state.logf("ssh exited successfully")
		}
		state.TunnelCmd = nil
		cmdIO.Close()
		state.TunnelPTY = nil
		state.TunnelIO = nil
		state.LastUpdated = time.Now()
	}()

	return nil
}

func (state *ConnectionState) Stop() error {
	if !state.IsRunning() {
		return nil
	}

	state.logf("stopping connection %s", state.Config.Name)
	cmd := state.TunnelCmd
	if cmd == nil || cmd.Process == nil {
		state.TunnelCmd = nil
		return nil
	}

	if err := sendTerminate(cmd.Process); err != nil {
		state.LastError = err.Error()
		state.logf("failed to terminate ssh: %v", err)
		return err
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !state.IsRunning() {
			state.TunnelCmd = nil
			state.LastUpdated = time.Now()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := cmd.Process.Kill(); err != nil {
		state.LastError = err.Error()
		state.logf("failed to kill ssh: %v", err)
		return err
	}

	if state.TunnelPTY != nil {
		_ = state.TunnelPTY.Close()
		state.TunnelPTY = nil
	}
	if state.TunnelIO != nil {
		state.TunnelIO.Close()
		state.TunnelIO = nil
	}
	state.TunnelCmd = nil
	state.LastUpdated = time.Now()
	return nil
}

func (state *ConnectionState) IsRunning() bool {
	if state.TunnelCmd == nil || state.TunnelCmd.Process == nil {
		return false
	}
	if runtime.GOOS == "windows" {
		if state.TunnelCmd.ProcessState == nil {
			return true
		}
		return !state.TunnelCmd.ProcessState.Exited()
	}
	if err := state.TunnelCmd.Process.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

func (state *ConnectionState) remoteHost() string {
	return fmt.Sprintf("%s@%s", state.Config.User, state.Config.Host)
}

func (state *ConnectionState) remoteSocketPath() string {
	if state.Config.SocketPath != "" {
		return state.Config.SocketPath
	}
	return fmt.Sprintf("/home/%s/%s/.s.PGSQL.5432", state.Config.User, state.Config.CloudSQLInstance)
}

func runCloudSQLAccess(cfg ConnectionConfig, logf LogFunc, prompter *PasswordPrompter, responder *PasswordResponder) error {
	cmd := exec.Command("ssh", append(sshArgs(), fmt.Sprintf("%s@%s", cfg.User, cfg.Host), fmt.Sprintf("cloudsql_access.sh start %s", cfg.CloudSQLInstance))...)
	if logf != nil {
		logf("cloudsql_access command: %s", commandDebugString(cmd))
	}
	askpassCleanup, err := configureSSHAskpass(cmd, "cloudsql_access", cachedResponderPassword(responder), logf)
	if err != nil {
		return fmt.Errorf("cloudsql_access failed: %w", err)
	}
	cmdIO, err := startCommandIO("cloudsql_access", cmd, logf)
	if err != nil {
		askpassCleanup()
		return fmt.Errorf("cloudsql_access failed: %w", err)
	}
	cmdIO.addCleanup(askpassCleanup)
	defer cmdIO.Close()

	done := make(chan error, 1)
	ready := make(chan struct{}, 1)
	go func() {
		if logf != nil {
			logf("cloudsql_access waiting for command to finish")
		}
		done <- cmdIO.Wait()
	}()

	go monitorPTYOutput("cloudsql_access", cmdIO.reader, logf, prompter, cmdIO.writer, responder, ready)
	ticker := time.NewTicker(cloudSQLAccessProgressInterval)
	defer ticker.Stop()
	timeout := time.NewTimer(cloudSQLAccessTimeout)
	defer timeout.Stop()
	startedAt := time.Now()
	for {
		select {
		case err = <-done:
			cmdIO.Close()
			if err != nil {
				return fmt.Errorf("cloudsql_access failed: %v", err)
			}
			if logf != nil {
				logf("cloudsql_access finished after %s", time.Since(startedAt).Round(time.Second))
			}
			return nil
		case <-ready:
			if logf != nil {
				logf("cloudsql_access printed local tunnel command after %s; continuing with app-managed tunnel", time.Since(startedAt).Round(time.Second))
			}
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			cmdIO.Close()
			return nil
		case <-ticker.C:
			if logf != nil {
				logf("cloudsql_access still running after %s with no completion", time.Since(startedAt).Round(time.Second))
				if runtime.GOOS == "windows" {
					logf("cloudsql_access on windows is using pipes; if ssh is waiting for terminal auth, configure SSH keys/agent or it may not prompt inside the UI")
				}
			}
		case <-timeout.C:
			if logf != nil {
				logf("cloudsql_access timed out after %s; terminating ssh pid=%d", time.Since(startedAt).Round(time.Second), processID(cmd))
			}
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			cmdIO.Close()
			return fmt.Errorf("cloudsql_access timed out after %s", cloudSQLAccessTimeout)
		}
	}
}

type commandIO struct {
	reader       io.Reader
	writer       io.Writer
	ptyFile      *os.File
	cmd          *exec.Cmd
	stdinReader  *io.PipeReader
	stdinWriter  *io.PipeWriter
	outputReader *io.PipeReader
	outputWriter *io.PipeWriter
	cleanups     []func()
	closeOnce    sync.Once
	waitOnce     sync.Once
	waitErr      error
}

func startCommandIO(label string, cmd *exec.Cmd, logf LogFunc) (*commandIO, error) {
	if logf != nil {
		logf("%s: starting with pty", label)
	}
	ptyFile, err := pty.Start(cmd)
	if err == nil {
		if logf != nil && cmd.Process != nil {
			logf("%s: started with pty pid=%d", label, cmd.Process.Pid)
		}
		return &commandIO{
			reader:  ptyFile,
			writer:  ptyFile,
			ptyFile: ptyFile,
			cmd:     cmd,
		}, nil
	}
	if !errors.Is(err, pty.ErrUnsupported) {
		if logf != nil {
			logf("%s: pty start failed: %v", label, err)
		}
		return nil, err
	}
	if logf != nil {
		logf("%s: pty unsupported; starting with pipes", label)
	}
	return startCommandWithPipes(label, cmd, logf)
}

func startCommandWithPipes(label string, cmd *exec.Cmd, logf LogFunc) (*commandIO, error) {
	stdinReader, stdinWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	cmd.Stdin = stdinReader
	cmd.Stdout = outputWriter
	cmd.Stderr = outputWriter

	if err := cmd.Start(); err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		_ = outputReader.Close()
		_ = outputWriter.Close()
		if logf != nil {
			logf("%s: pipe start failed: %v", label, err)
		}
		return nil, err
	}
	if logf != nil && cmd.Process != nil {
		logf("%s: started with pipes pid=%d", label, cmd.Process.Pid)
	}

	return &commandIO{
		reader:       outputReader,
		writer:       stdinWriter,
		cmd:          cmd,
		stdinReader:  stdinReader,
		stdinWriter:  stdinWriter,
		outputReader: outputReader,
		outputWriter: outputWriter,
	}, nil
}

func configureSSHAskpass(cmd *exec.Cmd, label string, password string, logf LogFunc) (func(), error) {
	if runtime.GOOS != "windows" || password == "" {
		return func() {}, nil
	}

	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable for SSH_ASKPASS: %w", err)
	}

	env := os.Environ()
	env = append(env,
		"SSH_ASKPASS="+executablePath,
		"SSH_ASKPASS_REQUIRE=force",
		"DISPLAY=lazy-jumphost",
		"LAZY_JUMPHOST_ASKPASS=1",
		"LAZY_JUMPHOST_ASKPASS_PASSWORD="+password,
	)
	cmd.Env = env
	if logf != nil {
		logf("%s: configured SSH_ASKPASS for windows password auth", label)
	}
	return func() {
	}, nil
}

func sshArgs() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	return []string{
		"-o", "BatchMode=no",
		"-o", "NumberOfPasswordPrompts=1",
		"-o", "StrictHostKeyChecking=accept-new",
	}
}

func cachedResponderPassword(responder *PasswordResponder) string {
	if responder == nil || responder.cache == nil {
		return ""
	}
	return responder.cache.Get()
}

func commandDebugString(cmd *exec.Cmd) string {
	if cmd == nil {
		return ""
	}
	return strings.Join(cmd.Args, " ")
}

func processID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

func (cmdIO *commandIO) addCleanup(cleanup func()) {
	if cmdIO == nil || cleanup == nil {
		return
	}
	cmdIO.cleanups = append(cmdIO.cleanups, cleanup)
}

func (cmdIO *commandIO) Wait() error {
	if cmdIO == nil || cmdIO.cmd == nil {
		return errors.New("process not started")
	}
	cmdIO.waitOnce.Do(func() {
		cmdIO.waitErr = cmdIO.cmd.Wait()
		if cmdIO.outputWriter != nil {
			_ = cmdIO.outputWriter.Close()
		}
		if cmdIO.stdinReader != nil {
			_ = cmdIO.stdinReader.Close()
		}
	})
	return cmdIO.waitErr
}

func (cmdIO *commandIO) Close() {
	if cmdIO == nil {
		return
	}
	cmdIO.closeOnce.Do(func() {
		if cmdIO.ptyFile != nil {
			_ = cmdIO.ptyFile.Close()
		}
		if cmdIO.stdinWriter != nil {
			_ = cmdIO.stdinWriter.Close()
		}
		if cmdIO.stdinReader != nil {
			_ = cmdIO.stdinReader.Close()
		}
		if cmdIO.outputReader != nil {
			_ = cmdIO.outputReader.Close()
		}
		if cmdIO.outputWriter != nil {
			_ = cmdIO.outputWriter.Close()
		}
		for _, cleanup := range cmdIO.cleanups {
			cleanup()
		}
	})
}

func escapeColons(value string) string {
	return strings.ReplaceAll(value, ":", "\\:")
}

func ensureLocalPortAvailable(port int) error {
	address := fmt.Sprintf("127.0.0.1:%d", port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("local port %d is already in use or unavailable: %w", port, err)
	}
	return listener.Close()
}

func sendTerminate(process *os.Process) error {
	if process == nil {
		return errors.New("process not started")
	}
	if runtime.GOOS == "windows" {
		return process.Kill()
	}
	return process.Signal(syscall.SIGTERM)
}

func (state *ConnectionState) logf(format string, args ...any) {
	if state.Logf == nil {
		return
	}
	state.Logf(format, args...)
}

type logChannelWriter struct {
	ch     chan<- string
	logf   LogFunc
	prefix string
}

func (writer *logChannelWriter) Write(p []byte) (int, error) {
	if writer.logf != nil {
		lines := strings.Split(string(p), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			writer.logf("%s%s", writer.prefix, line)
		}
		return len(p), nil
	}
	if writer.ch == nil {
		return len(p), nil
	}
	lines := strings.Split(string(p), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		writer.ch <- fmt.Sprintf("%s%s", writer.prefix, line)
	}
	return len(p), nil
}

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSICodes(message string) string {
	if message == "" {
		return message
	}
	return ansiEscapePattern.ReplaceAllString(message, "")
}

type PasswordCache struct {
	mu    sync.Mutex
	value string
}

func NewPasswordCache(initial string) *PasswordCache {
	return &PasswordCache{value: initial}
}

func (cache *PasswordCache) Get() string {
	if cache == nil {
		return ""
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.value
}

func (cache *PasswordCache) Set(value string) {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	cache.value = value
	cache.mu.Unlock()
}

type PasswordResponder struct {
	cache *PasswordCache
	used  bool
}

func NewPasswordResponder(cache *PasswordCache) *PasswordResponder {
	return &PasswordResponder{cache: cache}
}

func (responder *PasswordResponder) Next(prompter *PasswordPrompter, prompt string) (string, bool) {
	if responder == nil {
		return "", false
	}
	if !responder.used {
		if cached := responder.cache.Get(); cached != "" {
			responder.used = true
			return cached, true
		}
	}
	if prompter == nil {
		return "", false
	}
	password, ok := prompter.Prompt(prompt)
	if !ok {
		return "", false
	}
	responder.used = true
	if password != "" {
		responder.cache.Set(password)
	}
	return password, true
}

type PasswordPrompter struct {
	app    *tview.Application
	pages  *tview.Pages
	mu     sync.Mutex
	active bool
}

func NewPasswordPrompter(app *tview.Application, pages *tview.Pages) *PasswordPrompter {
	return &PasswordPrompter{app: app, pages: pages}
}

func (prompter *PasswordPrompter) IsActive() bool {
	if prompter == nil {
		return false
	}
	prompter.mu.Lock()
	defer prompter.mu.Unlock()
	return prompter.active
}

func (prompter *PasswordPrompter) Prompt(prompt string) (string, bool) {
	if prompter == nil {
		return "", false
	}
	prompter.mu.Lock()
	if prompter.active {
		prompter.mu.Unlock()
		return "", false
	}
	prompter.active = true
	prompter.mu.Unlock()

	resultCh := make(chan string, 1)
	cancelCh := make(chan struct{}, 1)

	prompter.app.QueueUpdateDraw(func() {
		previous := prompter.app.GetFocus()
		input := tview.NewInputField()
		input.SetLabel("Password")
		input.SetMaskCharacter('*')

		form := tview.NewForm()
		form.SetBorder(true).SetTitle(prompt)
		form.AddFormItem(input)
		form.AddButton("Submit", func() {
			prompter.pages.RemovePage("password")
			if previous != nil {
				prompter.app.SetFocus(previous)
			}
			resultCh <- input.GetText()
		})
		form.AddButton("Cancel", func() {
			prompter.pages.RemovePage("password")
			if previous != nil {
				prompter.app.SetFocus(previous)
			}
			cancelCh <- struct{}{}
		})

		modal := centerPrimitive(form, 60, 9)
		prompter.pages.AddPage("password", modal, true, true)
		prompter.app.SetFocus(input)
	})

	var password string
	var ok bool
	select {
	case password = <-resultCh:
		ok = true
	case <-cancelCh:
		ok = false
	}

	prompter.mu.Lock()
	prompter.active = false
	prompter.mu.Unlock()
	return password, ok
}

func (prompter *PasswordPrompter) PromptOptional(prompt string) (string, bool) {
	if prompter == nil {
		return "", true
	}
	prompter.mu.Lock()
	if prompter.active {
		prompter.mu.Unlock()
		return "", true
	}
	prompter.active = true
	prompter.mu.Unlock()

	resultCh := make(chan string, 1)
	cancelCh := make(chan struct{}, 1)

	prompter.app.QueueUpdateDraw(func() {
		previous := prompter.app.GetFocus()
		input := tview.NewInputField()
		input.SetLabel("Password")
		input.SetMaskCharacter('*')

		form := tview.NewForm()
		form.SetBorder(true).SetTitle(prompt)
		form.AddFormItem(input)
		form.AddButton("Continue", func() {
			prompter.pages.RemovePage("password")
			if previous != nil {
				prompter.app.SetFocus(previous)
			}
			resultCh <- input.GetText()
		})
		form.AddButton("Cancel", func() {
			prompter.pages.RemovePage("password")
			if previous != nil {
				prompter.app.SetFocus(previous)
			}
			cancelCh <- struct{}{}
		})

		modal := centerPrimitive(form, 60, 9)
		prompter.pages.AddPage("password", modal, true, true)
		prompter.app.SetFocus(input)
	})

	var password string
	var ok bool
	select {
	case password = <-resultCh:
		ok = true
	case <-cancelCh:
		ok = false
	}

	prompter.mu.Lock()
	prompter.active = false
	prompter.mu.Unlock()
	return password, ok
}

func centerPrimitive(primitive tview.Primitive, width int, height int) tview.Primitive {
	row := tview.NewFlex().SetDirection(tview.FlexRow)
	row.AddItem(nil, 0, 1, false)
	row.AddItem(primitive, height, 1, true)
	row.AddItem(nil, 0, 1, false)

	col := tview.NewFlex().SetDirection(tview.FlexColumn)
	col.AddItem(nil, 0, 1, false)
	col.AddItem(row, width, 1, true)
	col.AddItem(nil, 0, 1, false)
	return col
}

func monitorPTYOutput(label string, reader io.Reader, logf LogFunc, prompter *PasswordPrompter, writer io.Writer, responder *PasswordResponder, ready chan<- struct{}) {
	if logf != nil {
		logf("%s: output monitor started", label)
	}
	buf := make([]byte, 4096)
	var pending strings.Builder
	readySent := false
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			if logf != nil {
				for _, line := range strings.Split(chunk, "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					logf("%s", line)
				}
			}
			pending.WriteString(chunk)
			pendingText := pending.String()
			if !readySent && cloudSQLAccessReady(pendingText) {
				if logf != nil {
					logf("%s: local tunnel command detected", label)
				}
				if ready != nil {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
				readySent = true
			}
			if sshPasswordPromptDetected(pendingText) {
				if logf != nil {
					logf("%s: password prompt detected", label)
				}
				responded := false
				if responder != nil {
					password, ok := responder.Next(prompter, "SSH password")
					if ok {
						if password != "" {
							_, _ = writer.Write([]byte(password + "\n"))
						} else {
							_, _ = writer.Write([]byte("\n"))
						}
						responded = true
						if logf != nil {
							logf("%s: responded to password prompt", label)
						}
					}
				}
				if !responded && prompter != nil {
					password, ok := prompter.Prompt("SSH password")
					if ok && password != "" {
						_, _ = writer.Write([]byte(password + "\n"))
					} else if ok {
						_, _ = writer.Write([]byte("\n"))
					}
					if logf != nil && ok {
						logf("%s: prompted for password and wrote response", label)
					}
				}
				if !responded && prompter == nil && logf != nil {
					logf("%s: password prompt detected but no prompter was available", label)
				}
				pending.Reset()
			}
			if pending.Len() > 4096 {
				pending.Reset()
			}
		}
		if err != nil {
			if logf != nil {
				if errors.Is(err, io.EOF) {
					logf("%s: output monitor closed", label)
				} else {
					logf("%s: output monitor stopped: %v", label, err)
				}
			}
			return
		}
	}
}

func cloudSQLAccessReady(output string) bool {
	return strings.Contains(output, "ssh -fnNT -L")
}

func sshPasswordPromptDetected(output string) bool {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		normalized := strings.TrimSpace(stripANSICodes(line))
		lower := strings.ToLower(normalized)
		if !strings.HasSuffix(lower, "password:") {
			continue
		}
		if strings.HasPrefix(lower, "password:") {
			continue
		}
		return true
	}
	return false
}
