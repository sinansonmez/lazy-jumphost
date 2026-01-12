package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
	LastError   string
	LastUpdated time.Time
	Logf        LogFunc
	Prompter    *PasswordPrompter
}

type LogFunc func(format string, args ...any)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to YAML config")
	debugMode := flag.Bool("debug", false, "Show debug logs in the UI")
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
	if *debugMode {
		logView = tview.NewTextView()
		logView.SetDynamicColors(true)
		logView.SetBorder(true).SetTitle("Debug Logs")
		logCh := make(chan string, 200)
		debugLogf = func(format string, args ...any) {
			message := fmt.Sprintf(format, args...)
			logCh <- message
		}
		go func() {
			for message := range logCh {
				app.QueueUpdateDraw(func() {
					fmt.Fprintln(logView, message)
					logView.ScrollToEnd()
				})
			}
		}()
		debugLogf("debug mode enabled")
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

	password := ""
	if state.Prompter != nil {
		entered, ok := state.Prompter.PromptOptional("SSH password (leave blank to use keys)")
		if !ok {
			return errors.New("start cancelled")
		}
		password = entered
	}

	state.logf("starting connection %s", state.Config.Name)
	if err := runCloudSQLAccess(state.Config, state.logf, state.Prompter, password); err != nil {
		state.LastError = err.Error()
		return err
	}

	localSpec := fmt.Sprintf("127.0.0.1:%d:%s", state.Config.LocalPort, escapeColons(state.remoteSocketPath()))
	cmd := exec.Command("ssh", "-N", "-T", "-L", localSpec, state.remoteHost())
	ptyFile, err := pty.Start(cmd)
	if err != nil {
		state.LastError = err.Error()
		state.logf("failed to start ssh: %v", err)
		return err
	}

	state.TunnelCmd = cmd
	state.TunnelPTY = ptyFile
	state.LastError = ""
	state.LastUpdated = time.Now()
	state.logf("ssh command: ssh -N -T -L %s %s", localSpec, state.remoteHost())
	writePassword(ptyFile, password)

	go monitorPTYOutput(ptyFile, state.Logf, state.Prompter, ptyFile)

	go func() {
		err := cmd.Wait()
		if err != nil {
			state.LastError = err.Error()
			state.logf("ssh exited with error: %v", err)
		} else {
			state.logf("ssh exited successfully")
		}
		state.TunnelCmd = nil
		if state.TunnelPTY != nil {
			_ = state.TunnelPTY.Close()
			state.TunnelPTY = nil
		}
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

func runCloudSQLAccess(cfg ConnectionConfig, logf LogFunc, prompter *PasswordPrompter, password string) error {
	cmd := exec.Command("ssh", fmt.Sprintf("%s@%s", cfg.User, cfg.Host), fmt.Sprintf("cloudsql_access.sh start %s", cfg.CloudSQLInstance))
	ptyFile, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("cloudsql_access failed: %w", err)
	}
	defer func() {
		_ = ptyFile.Close()
	}()

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	writePassword(ptyFile, password)
	go monitorPTYOutput(ptyFile, logf, prompter, ptyFile)
	err = <-done
	_ = ptyFile.Close()
	if err != nil {
		return fmt.Errorf("cloudsql_access failed: %v", err)
	}
	if logf != nil {
		logf("cloudsql_access ok")
	}
	return nil
}

func escapeColons(value string) string {
	return strings.ReplaceAll(value, ":", "\\:")
}

func sendTerminate(process *os.Process) error {
	if process == nil {
		return errors.New("process not started")
	}
	if runtime.GOOS == "windows" {
		return process.Signal(os.Interrupt)
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

func monitorPTYOutput(reader io.Reader, logf LogFunc, prompter *PasswordPrompter, writer io.Writer) {
	buf := make([]byte, 4096)
	var pending strings.Builder
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
			needle := strings.ToLower(pending.String())
			if strings.Contains(needle, "password:") {
				if prompter == nil {
					pending.Reset()
					continue
				}
				password, ok := prompter.Prompt("SSH password")
				if ok && password != "" {
					_, _ = writer.Write([]byte(password + "\n"))
				} else if ok {
					_, _ = writer.Write([]byte("\n"))
				}
				pending.Reset()
			}
			if pending.Len() > 4096 {
				pending.Reset()
			}
		}
		if err != nil {
			return
		}
	}
}

func writePassword(writer io.Writer, password string) {
	if writer == nil || password == "" {
		return
	}
	_, _ = writer.Write([]byte(password + "\n"))
}
