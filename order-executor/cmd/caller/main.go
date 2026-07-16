// Written with assistance from Claude Sonnet 4.6
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	"encoding/json"
	"order-executor/executor"
)
// hold the config details
type config struct {
	Symbol   string  `json:"symbol"`
	Side     string  `json:"side"`
	Qty      float64 `json:"qty"`     // to support partial orders
	Slices   int     `json:"slices"`
	Duration string  `json:"duration"` // e.g. "30s", "5m"
}

func loadConfig(path string) (config, error) {
	var cfg config
	f, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("opening config file: %w", err)
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decoding config: %w", err)
	}

	return cfg, nil
}

func main() {
	// open log file 
	fileName := fmt.Sprintf("twap_%s.log", time.Now().Format("20060102_150405"))
	logFile, err := os.Create(fileName)
	if err != nil {
		log.Fatalf("opening log file: %v", err)
	}
	defer logFile.Close()

	// write prints a line to both stdout and the log file
	write := func(line string) {
		fmt.Println(line)
		fmt.Fprintln(logFile, line)
	}

	// context cancelled on Ctrl+C or SIGTERM(kill <pid>)
	// coordinator's select sees ctx.Done() and runs the shutdown path
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	/* signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM) tells Go's runtime to route Ctrl+C (SIGINT) and kill signals (SIGTERM) into sigCh instead of the default behavior (immediate process exit). The goroutine blocks on <-sigCh waiting for one of those signals. When it arrives, it calls cancel() which triggers the coordinator's shutdown path. Without this, Ctrl+C would kill the process instantly with no cleanup.
	*/

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM) // gets notified when these signals are interrupted
	go func() {
		<-sigCh
		write("\n[caller] interrupt received — cancelling execution")
		cancel()
	}()
	
	// what is this exeecutor? : executor is the package we have coded!
		// it contains a function called newHTTPExchange
	exchange := executor.NewHTTPExchange("http://127.0.0.1:9101")

	// parse the order details from the config.json
	cfg, err := loadConfig("cmd/caller/config.json")
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	duration, err := time.ParseDuration(cfg.Duration)
	if err != nil {
		log.Fatalf("parsing duration %q: %v", cfg.Duration, err)
	}

	twap := executor.NewTWAPStrategy(cfg.Symbol, cfg.Side, cfg.Qty, duration, cfg.Slices)

	write(fmt.Sprintf("[caller] starting TWAP — symbol=%s side=%s qty=%.4f slices=%d duration=%s",
		cfg.Symbol, cfg.Side, cfg.Qty, cfg.Slices, cfg.Duration))
	
	/*

	2026-07-11T12:35:40+05:30

Breaking that down:
- 2026-07-11 — date
- T — literal separator between date and time
- 12:35:40 — time (24-hour, from 15:04:05)
- +05:30 — timezone offset from UTC (your machine is IST). If it were UTC it'd show Z instead.

Why it's used here

This is the [caller] started at ... line (main.go:87). RFC3339 is a good choice for a log/audit timestamp because it's unambiguous, timezone-aware, and lexicographically sortable — sorting the strings sorts them chronologically.
*/
	write(fmt.Sprintf("[caller] started at %s", time.Now().Format(time.RFC3339)))
	write("---")

	ch := twap.Execute(ctx, exchange)



	var final executor.ProgressUpdate
	for update := range ch {
		for _, l := range update.Logs {
			write(fmt.Sprintf("[log]    %s", l))
		}

		write(fmt.Sprintf(
			"[%s] status=%-10s filled=%.4f/%.4f avg_price=%.2f slices=%d/%d placed=%d",
			update.Timestamp.Format("15:04:05"),
			update.Status,
			update.FilledQty,
			update.TotalQty,
			update.AvgPrice,
			update.SlicesFilled,
			update.SlicesTotal,
			update.SlicesPlaced,
		))
		final = update
	}

	// print the final status and (errors if they exist)
	write("---")
	write(fmt.Sprintf("[result] status=%s filled=%.4f/%.4f avg_price=%.2f",
		final.Status, final.FilledQty, final.TotalQty, final.AvgPrice))
	write(fmt.Sprintf("[result] schedule: deviation_from_ideal=%.2f%% max_interval_drift=%.1fms",
		final.ScheduleDeviationPct, final.MaxIntervalDriftMs))

	if len(final.Errors) > 0 {
		write("[result] slice errors:")
		for _, e := range final.Errors {
			write(fmt.Sprintf("         %s", e))
		}
	}
}
