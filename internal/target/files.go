package target

import (
	"context"
	"fmt"
	"io"
	"os"

	"backstash/internal/config"
	"backstash/internal/execx"
)

func tarCmd(target *config.FilesTarget, stdin io.Reader, stdout io.Writer, args ...string) execx.Cmd {
	return place(target.Mode, target.Container, execx.Cmd{Name: "tar", Args: args, Stdin: stdin, Stdout: stdout})
}

// DumpFiles archives the configured folder (in a container or on this host) as a .tar.gz.
func (deps Deps) DumpFiles(ctx context.Context, target *config.FilesTarget) (Artifact, error) {
	name := BackupName(target.Label, config.Files.Extension(), deps.now())
	return deps.capture(name, func(out io.Writer) error {
		return deps.Run.Run(ctx, tarCmd(target, nil, out, "-czf", "-", "--warning=no-file-changed", "-C", target.Path, "."))
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
