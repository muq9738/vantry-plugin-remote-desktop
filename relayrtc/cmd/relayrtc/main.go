package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/muq9738/vantry-plugin-remote-desktop/relayrtc"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "relayrtc:", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use serve --config PATH or proxy --socket PATH")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	socket := flags.String("socket", "/run/vantry-rtc/signaling.sock", "private signaling Unix socket")
	configPath := flags.String("config", "/etc/vantry-rtc/signaling.json", "signaling configuration")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	switch args[0] {
	case "proxy":
		return relayrtc.Proxy(ctx, *socket, os.Stdin, os.Stdout)
	case "serve":
		file, err := os.Open(*configPath)
		if err != nil {
			return err
		}
		defer file.Close()
		var config relayrtc.Config
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&config); err != nil {
			return fmt.Errorf("invalid signaling configuration")
		}
		if *socket != "/run/vantry-rtc/signaling.sock" {
			config.Socket = *socket
		}
		return relayrtc.Serve(ctx, config)
	default:
		return fmt.Errorf("unknown command")
	}
}
