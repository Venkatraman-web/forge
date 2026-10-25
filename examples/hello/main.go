// Command hello is the smallest complete Forge application: one program that can run a worker
// and submit a job.
//
//	go run ./examples/hello worker            # runs the "hello.greet" handler until Ctrl-C
//	go run ./examples/hello submit Ada        # submits a job, waits, prints the result
//
// Settings come from the environment, so the same program works against a Forge on your laptop
// or on another machine:
//
//	FORGE_URL          submission API         (default http://127.0.0.1:8080)
//	FORGE_API_KEY      submit key             (required for "submit")
//	FORGE_WORKER_URL   worker API             (default http://127.0.0.1:8081)
//	FORGE_WORKER_API_KEY  worker key          (required for "worker")
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Venkatraman-web/forge/sdk/forgeclient"
	"github.com/Venkatraman-web/forge/sdk/workerclient"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hello worker | hello submit [name]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "worker":
		err = runWorker()
	case "submit":
		name := "world"
		if len(os.Args) > 2 {
			name = os.Args[2]
		}
		err = submit(name)
	default:
		err = errors.New("unknown command " + os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hello:", err)
		os.Exit(1)
	}
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// greeting is the job's input; the handler decodes it and returns a result.
type greeting struct{ Name string }

func runWorker() error {
	client, err := workerclient.New(workerclient.Config{
		BaseURL: env("FORGE_WORKER_URL", "http://127.0.0.1:8081"), APIKey: os.Getenv("FORGE_WORKER_API_KEY"),
	})
	if err != nil {
		return err
	}
	w, err := workerruntime.New(workerruntime.Config{Client: client, Name: "hello", Concurrency: 4})
	if err != nil {
		return err
	}
	// Forge delivers a job at least once, so a handler must be safe to run twice.
	err = w.Handle("hello.greet", func(ctx context.Context, job workerruntime.Job) (any, error) {
		var in greeting
		if err := job.Decode(&in); err != nil {
			return nil, workerruntime.Permanent(err) // a bad input will never get better: do not retry
		}
		return map[string]string{"message": "Hello, " + in.Name + "!"}, nil
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("worker running; press Ctrl-C to stop")
	return w.Run(ctx)
}

func submit(name string) error {
	client, err := forgeclient.New(forgeclient.Config{
		BaseURL: env("FORGE_URL", "http://127.0.0.1:8080"), APIKey: os.Getenv("FORGE_API_KEY"),
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	job, err := client.Submit(ctx, forgeclient.SubmitRequest{Type: "hello.greet", Input: greeting{Name: name}})
	if err != nil {
		return err
	}
	fmt.Println("submitted", job.ID)
	for !(job.Status == "succeeded" || job.Status == "failed") {
		time.Sleep(200 * time.Millisecond)
		if job, err = client.Get(ctx, job.ID); err != nil {
			return err
		}
	}
	fmt.Printf("%s: result=%s\n", job.Status, job.Result)
	return nil
}
