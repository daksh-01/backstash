// Package target backs up and restores one kind of thing (Postgres, Redis, a folder) by
// shelling out through execx. Every backup is staged as a private local file first.
package target

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"backstash/internal/config"
	"backstash/internal/execx"
	"backstash/internal/secrets"
)

const (
	dirMode  = 0o700
	fileMode = 0o600
	// stamp is the timestamp in every backup name; retention matches on this shape.
	stamp = "20060102-150405"
)

// Deps is what every target needs.
type Deps struct {
	Run        execx.Runner
	Secrets    secrets.Set
	StagingDir string
	Now        func() time.Time
}

// Artifact is a finished local backup file.
type Artifact struct {
	Path string
	Name string
	Size int64
}

func (deps Deps) now() time.Time {
	if deps.Now != nil {
		return deps.Now().UTC()
	}
	return time.Now().UTC()
}

// BackupName returns <label>-YYYYmmdd-HHMMSS<ext>.
func BackupName(label, ext string, timestamp time.Time) string {
	return fmt.Sprintf("%s-%s%s", label, timestamp.UTC().Format(stamp), ext)
}

// place runs c inside the container in docker mode and on this host otherwise.
func place(mode config.Mode, container string, cmd execx.Cmd) execx.Cmd {
	if mode == config.ModeDocker {
		return execx.InContainer(container, cmd)
	}
	return cmd
}

// reserve creates the private staging dir and an exclusive 0600 file for name.
func (deps Deps) reserve(name string) (*os.File, string, error) {
	if err := os.MkdirAll(deps.StagingDir, dirMode); err != nil {
		return nil, "", fmt.Errorf("create staging dir: %w", err)
	}
	if err := os.Chmod(deps.StagingDir, dirMode); err != nil {
		return nil, "", fmt.Errorf("secure staging dir: %w", err)
	}
	path := filepath.Join(deps.StagingDir, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return nil, "", fmt.Errorf("create %s: %w", path, err)
	}
	return file, path, nil
}

// capture stages the stdout of write() as a backup file. A failed or empty run leaves nothing
// behind, so a partial dump is never uploaded.
func (deps Deps) capture(name string, write func(out io.Writer) error) (Artifact, error) {
	file, path, err := deps.reserve(name)
	if err != nil {
		return Artifact{}, err
	}
	err = write(file)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	return finish(path, name, err)
}

// ErrEmptyBackup indicates a dump command succeeded but produced a 0-byte file.
var ErrEmptyBackup = errors.New("backup is empty")

// finish turns a staged file into an Artifact, deleting it if err is set or the file is empty.
func finish(path, name string, err error) (Artifact, error) {
	var size int64
	if err == nil {
		var info os.FileInfo
		if info, err = os.Stat(path); err == nil {
			size = info.Size()
		}
	}
	if err == nil && size == 0 {
		err = ErrEmptyBackup
	}
	if err != nil {
		_ = os.Remove(path)
		return Artifact{}, err
	}
	return Artifact{Path: path, Name: name, Size: size}, nil
}

// ErrNoSecret says which secret a target needs and could not find.
type ErrNoSecret struct{ Name string }

func (err ErrNoSecret) Error() string { return "secret " + err.Name + " not found in Infisical" }
