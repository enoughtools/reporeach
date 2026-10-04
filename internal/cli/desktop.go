package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/desktop"
	"github.com/cloudflare/artifact-fs/internal/logging"
	ucli "github.com/urfave/cli"
)

type desktopJSONError struct{ cause error }

func (e *desktopJSONError) Error() string { return e.cause.Error() }
func (e *desktopJSONError) Unwrap() error { return e.cause }

func desktopCommand(ctx context.Context, stdout, stderr io.Writer) ucli.Command {
	home, _ := os.UserHomeDir()
	stateDir := filepath.Join(home, "Library", "Application Support", "RepoReach")
	socket := filepath.Join(stateDir, "engine.sock")
	jsonFailure := func(err error) error {
		_ = json.NewEncoder(stdout).Encode(map[string]string{"error": err.Error()})
		return &desktopJSONError{cause: err}
	}
	return ucli.Command{
		Name: "desktop", Usage: "RepoReach's private desktop control service",
		Subcommands: []ucli.Command{
			{
				Name: "serve", Usage: "run the background service",
				Flags: []ucli.Flag{
					ucli.StringFlag{Name: "state-dir", Value: stateDir},
					ucli.StringFlag{Name: "mount-root", Value: filepath.Join(home, "Repos")},
					ucli.StringFlag{Name: "socket", Value: socket},
					ucli.StringFlag{Name: "gh", Usage: "path to the bundled official GitHub CLI"},
				},
				Action: func(c *ucli.Context) error {
					ghPath := c.String("gh")
					if ghPath == "" || !filepath.IsAbs(ghPath) {
						return errors.New("--gh must name the bundled GitHub CLI with an absolute path")
					}
					// Authentication belongs to each repository. Keep the user's
					// native Git configuration and SSH agent available to manual
					// remotes; only disable terminal prompts and telemetry here.
					type priorEnvironment struct {
						value   string
						present bool
					}
					previous := map[string]priorEnvironment{}
					defer func() {
						for key, entry := range previous {
							if entry.present {
								_ = os.Setenv(key, entry.value)
							} else {
								_ = os.Unsetenv(key)
							}
						}
					}()
					for _, value := range []string{"GIT_TERMINAL_PROMPT=0", "GH_TELEMETRY=false"} {
						key, content, _ := strings.Cut(value, "=")
						old, present := os.LookupEnv(key)
						previous[key] = priorEnvironment{old, present}
						if err := os.Setenv(key, content); err != nil {
							return err
						}
					}
					return desktop.Serve(ctx, desktop.Options{StateDir: c.String("state-dir"), MountRoot: c.String("mount-root"), Socket: c.String("socket"), GHPath: ghPath, Logger: logging.NewJSONLogger(stderr, slog.LevelInfo)})
				},
			},
			{
				Name: "request", Usage: "send one local JSON request",
				Flags: []ucli.Flag{
					ucli.StringFlag{Name: "socket", Value: socket},
					ucli.StringFlag{Name: "method", Value: "GET"},
					ucli.StringFlag{Name: "path", Value: "/v1/status"},
					ucli.StringFlag{Name: "body"},
				},
				Action: func(c *ucli.Context) error {
					data, status, err := desktop.Request(ctx, c.String("socket"), c.String("method"), c.String("path"), []byte(c.String("body")))
					if err != nil {
						return jsonFailure(err)
					}
					if _, err := stdout.Write(data); err != nil {
						return err
					}
					if len(data) == 0 || data[len(data)-1] != '\n' {
						_, _ = fmt.Fprintln(stdout)
					}
					if status < 200 || status >= 300 {
						return &desktopJSONError{cause: fmt.Errorf("API request failed (%d)", status)}
					}
					return nil
				},
			},
		},
	}
}
