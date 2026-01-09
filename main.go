package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
	LastError   string
	LastUpdated time.Time
}

func main() {
	configPath := flag.String("config", "config.yaml", "Path to YAML config")
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

	startButton := tview.NewButton("Start").SetSelectedFunc(func() {
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
	})

	stopButton := tview.NewButton("Stop").SetSelectedFunc(func() {
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
	})

	refreshButton := tview.NewButton("Refresh").SetSelectedFunc(func() {
		refresh()
		statusBar.SetText("Status refreshed")
	})

	quitButton := tview.NewButton("Quit").SetSelectedFunc(func() {
		for _, state := range states {
			_ = state.Stop()
		}
		app.Stop()
	})

	buttons := tview.NewFlex().SetDirection(tview.FlexColumn)
	buttons.AddItem(startButton, 12, 1, false)
	buttons.AddItem(stopButton, 12, 1, false)
	buttons.AddItem(refreshButton, 12, 1, false)
	buttons.AddItem(quitButton, 12, 1, false)

	layout := tview.NewFlex().SetDirection(tview.FlexRow)
	mainRow := tview.NewFlex().SetDirection(tview.FlexColumn)
	mainRow.AddItem(list, 0, 1, true)
	mainRow.AddItem(details, 0, 2, false)

	layout.AddItem(mainRow, 0, 1, true)
	layout.AddItem(buttons, 3, 0, false)
	layout.AddItem(statusBar, 3, 0, false)

	refresh()

	quitSignals := make(chan os.Signal, 1)
	signal.Notify(quitSignals, os.Interrupt, syscall.SIGTERM)
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

	if err := app.SetRoot(layout, true).EnableMouse(true).Run(); err != nil {
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

	if err := runCloudSQLAccess(state.Config); err != nil {
		state.LastError = err.Error()
		return err
	}

	localSpec := fmt.Sprintf("127.0.0.1:%d:%s", state.Config.LocalPort, escapeColons(state.remoteSocketPath()))
	cmd := exec.Command("ssh", "-N", "-T", "-L", localSpec, state.remoteHost())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Start(); err != nil {
		state.LastError = err.Error()
		return err
	}

	state.TunnelCmd = cmd
	state.LastError = ""
	state.LastUpdated = time.Now()

	go func() {
		err := cmd.Wait()
		if err != nil {
			state.LastError = err.Error()
		}
		state.TunnelCmd = nil
		state.LastUpdated = time.Now()
	}()

	return nil
}

func (state *ConnectionState) Stop() error {
	if !state.IsRunning() {
		return nil
	}

	cmd := state.TunnelCmd
	if cmd == nil || cmd.Process == nil {
		state.TunnelCmd = nil
		return nil
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		state.LastError = err.Error()
		return err
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	select {
	case err := <-waitCh:
		if err != nil {
			state.LastError = err.Error()
			return err
		}
	case <-time.After(2 * time.Second):
		if err := cmd.Process.Kill(); err != nil {
			state.LastError = err.Error()
			return err
		}
	}

	state.TunnelCmd = nil
	state.LastUpdated = time.Now()
	return nil
}

func (state *ConnectionState) IsRunning() bool {
	if state.TunnelCmd == nil || state.TunnelCmd.Process == nil {
		return false
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

func runCloudSQLAccess(cfg ConnectionConfig) error {
	cmd := exec.Command("ssh", fmt.Sprintf("%s@%s", cfg.User, cfg.Host), fmt.Sprintf("cloudsql_access.sh start %s", cfg.CloudSQLInstance))
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cloudsql_access failed: %s", strings.TrimSpace(output.String()))
	}
	return nil
}

func escapeColons(value string) string {
	return strings.ReplaceAll(value, ":", "\\:")
}
