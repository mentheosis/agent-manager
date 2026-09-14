package taskqueues

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
)

// Settings belong to the durable UI instance, never to the task-pool identifier.
type controllerControls struct {
	mu         sync.Mutex
	MaxWorkers int    `json:"max_workers"`
	Paused     bool   `json:"paused"`
	Revision   int64  `json:"revision"`
	Limits     Limits `json:"limits"`
	path       string
}

func loadControls(c Config) (*controllerControls, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(c.ControllerID) {
		return nil, errors.New("invalid controller ID")
	}
	path := filepath.Join(c.WorkspaceRoot, "controllers", c.ControllerID+".json")
	value := &controllerControls{MaxWorkers: max(1, c.InitialMaxWorkers), Paused: true, path: path, Limits: c.limits()}
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, value); err != nil {
			return nil, errors.New("invalid saved controller settings")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if value.MaxWorkers < 1 {
		return nil, errors.New("saved worker limit must be positive")
	}
	return value, nil
}
func (c *controllerControls) save(limit int, paused bool, updated ...Limits) error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0700); err != nil {
		return err
	}
	limits := c.Limits
	if len(updated) > 0 {
		limits = updated[0]
	}
	data, err := json.Marshal(map[string]any{"max_workers": limit, "paused": paused, "revision": c.Revision + 1, "limits": limits})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(c.path), ".settings-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), c.path); err != nil {
		return err
	}
	c.Limits = limits
	c.MaxWorkers = limit
	c.Paused = paused
	c.Revision++
	return nil
}

// Only one Go process may drive a UI controller, including during recovery.
func (c *controllerControls) lockProcess() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(c.path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(c.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("controller is already running")
	}
	return file, nil
}
