package target

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"backstash/internal/config"
	"backstash/internal/execx"
)

func tarCmd(target *config.FilesTarget, stdin io.Reader, stdout io.Writer, args ...string) execx.Cmd {
	return place(target.Mode, target.Container, execx.Cmd{Name: "tar", Args: args, Stdin: stdin, Stdout: stdout})
}

// gnuTar reports whether the tar this target runs is GNU tar. Only GNU tar understands --warning;
// BusyBox tar (Alpine-based images) rejects it and the whole backup fails.
func (deps Deps) gnuTar(ctx context.Context, target *config.FilesTarget) bool {
	var out bytes.Buffer
	if err := deps.Run.Run(ctx, tarCmd(target, nil, &out, "--version")); err != nil {
		return false
	}
	return strings.Contains(out.String(), "GNU tar")
}

// DumpFiles archives the configured folder (in a container or on this host) as a .tar.gz.
func (deps Deps) DumpFiles(ctx context.Context, target *config.FilesTarget) (Artifact, error) {
	name := BackupName(target.Label, config.Files.Extension(), deps.now())
	args := []string{"-czf", "-"}
	// A file changing while it is read is normal for a live uploads folder; GNU tar would exit 1 for it.
	if deps.gnuTar(ctx, target) {
		args = append(args, "--warning=no-file-changed")
	}
	args = append(args, "-C", target.Path, ".")
	return deps.capture(name, func(out io.Writer) error {
		return deps.Run.Run(ctx, tarCmd(target, nil, out, args...))
	})
}

// RestoreFiles unpacks the archive over the folder. Files in the archive overwrite existing
// ones; files that are not in the archive are kept.
func (deps Deps) RestoreFiles(ctx context.Context, target *config.FilesTarget, path string) error {
	if target.Mode == config.ModePath {
		if err := os.MkdirAll(target.Path, 0o755); err != nil {
			return fmt.Errorf("create destination directory: %w", err)
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return deps.Run.Run(ctx, tarCmd(target, file, nil, "-xzf", "-", "-C", target.Path))
}
