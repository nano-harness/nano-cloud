package main //nolint:revive

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strings"
)

func main() {
	bin := strings.TrimSpace(os.Getenv("RUNTIME_BIN"))
	prompt := strings.TrimSpace(os.Getenv("PROMPT"))
	if bin == "" || prompt == "" {
		os.Exit(2)
	}

	args := []string{}
	if v := strings.TrimSpace(os.Getenv("RUNTIME_ARGS")); v != "" {
		args = append(args, strings.Fields(v)...)
	}
	args = append(args, prompt)

	cmd := exec.CommandContext(context.Background(), bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n") //nolint:errcheck
		os.Exit(1)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n") //nolint:errcheck
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		os.Stderr.WriteString(err.Error() + "\n") //nolint:errcheck
		os.Exit(1)
	}

	done := make(chan struct{}, 2)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			os.Stdout.WriteString(sc.Text() + "\n") //nolint:errcheck
		}
		done <- struct{}{}
	}()
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			os.Stderr.WriteString(sc.Text() + "\n") //nolint:errcheck
		}
		done <- struct{}{}
	}()

	<-done
	<-done

	if err := cmd.Wait(); err != nil {
		os.Exit(1)
	}
}
