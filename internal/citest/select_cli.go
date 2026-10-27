package citest

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func EventFromEnvironment(env []string) (Event, error) {
	e := Event{Name: envValue(env, "GITHUB_EVENT_NAME"), Base: envValue(env, "CI_BASE_SHA"), Head: envValue(env, "GITHUB_SHA"), Full: strings.EqualFold(envValue(env, "CI_FULL_TESTS"), "true")}
	data, err := os.ReadFile(envValue(env, "GITHUB_EVENT_PATH"))
	if err != nil {
		return e, err
	}
	var payload struct {
		PullRequest struct {
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return e, err
	}
	e.Author = payload.PullRequest.User.Login
	for _, label := range payload.PullRequest.Labels {
		e.Labels = append(e.Labels, label.Name)
	}
	return e, nil
}
func appendFile(path, text string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(f, text)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
func RunSelect(ctx context.Context, root string, args, env []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("ci-select", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "", "selection manifest destination")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" || flags.NArg() != 0 {
		return fmt.Errorf("set --output manifest.json")
	}
	event, err := EventFromEnvironment(env)
	if err != nil {
		return err
	}
	p := NewPlanner(root)
	p.Env = env
	m, err := p.Plan(ctx, event)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*output, append(data, '\n'), 0600); err != nil {
		return err
	}
	report := m.Summary()
	if _, err := io.WriteString(stdout, report); err != nil {
		return err
	}
	if err := appendFile(envValue(env, "GITHUB_STEP_SUMMARY"), report); err != nil {
		return err
	}
	matrix, err := json.Marshal(m.Matrix)
	if err != nil {
		return err
	}
	return appendFile(envValue(env, "GITHUB_OUTPUT"), fmt.Sprintf("mode=%s\nhas_tests=%t\nempty_ok=%t\nmatrix=%s\n", m.Mode, len(m.Matrix.Include) > 0, m.EmptyOK, matrix))
}
