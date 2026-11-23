// rtc-check is an explicit acceptance tool. It never prints tickets or TURN
// credentials and uses recorded input by default, so synthetic tests cannot
// type into a user's real applications.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	root "github.com/muq9738/vantry-plugin-remote-desktop"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
)

type inputRecorder struct{ count atomic.Uint64 }

func (r *inputRecorder) Mouse(context.Context, root.MouseEvent) error { r.count.Add(1); return nil }
func (r *inputRecorder) Wheel(context.Context, root.WheelEvent) error { r.count.Add(1); return nil }
func (r *inputRecorder) Key(context.Context, root.KeyEvent) error     { r.count.Add(1); return nil }
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	mode := flag.String("mode", "loopback", "loopback, host, or viewer")
	sshHost := flag.String("relay-ssh", "", "optional user-configured relay SSH alias; empty uses LAN")
	config := flag.String("ssh-config", "", "optional existing OpenSSH configuration")
	helper := flag.String("helper", os.Getenv("VANTRY_REMOTE_HELPER"), "packaged native H264 helper")
	quality := flag.String("quality", "balanced", "ultra, balanced, or low")
	duration := flag.Duration("duration", 12*time.Second, "measurement duration after connection")
	output := flag.String("out", "", "JSON metrics destination")
	ticketFile := flag.String("ticket-file", "", "0600 file used to hand the private ticket between machines")
	capture := flag.Bool("capture", false, "use actual ScreenCaptureKit instead of a synthetic pattern")
	noise := flag.Bool("noise", false, "stress the synthetic stream with noisy motion")
	allowInput := flag.Bool("allow-input", false, "explicitly allow real input injection on the host")
	testInput := flag.Bool("test-input", false, "explicitly send controlled click/text from a viewer")
	inputX := flag.Float64("input-x", .5, "normalized controlled test target X")
	inputY := flag.Float64("input-y", .5, "normalized controlled test target Y")
	inputText := flag.String("input-text", "v", "committed Unicode for controlled acceptance")
	flag.Parse()
	if *mode != "loopback" && *mode != "host" && *mode != "viewer" {
		return errors.New("invalid mode")
	}
	if *helper == "" || *output == "" {
		return errors.New("--helper and --out are required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	limit, cancelLimit := context.WithTimeout(ctx, *duration+60*time.Second)
	defer cancelLimit()
	recorder := &inputRecorder{}
	var host *rtc.Host
	var hostRelay *rtc.SSHRelay
	var ticket string
	var err error
	if *mode != "viewer" {
		cfg := rtc.HostConfig{SSHHost: *sshHost, HelperPath: *helper, Quality: *quality}
		if *sshHost != "" {
			hostRelay, err = rtc.NewSSHRelay(rtc.SSHConfig{Host: *sshHost, ConfigFile: *config})
			if err != nil {
				return err
			}
			defer hostRelay.Close()
			cfg.Relay = hostRelay
		}
		if !*allowInput {
			cfg.InputSink = recorder
		}
		if !*capture {
			cfg.SourceFactory = func(ctx context.Context, v rtc.VideoConfig) (rtc.EncodedSource, error) {
				return rtc.NewPatternSource(ctx, *helper, v, 0, *noise)
			}
		}
		host, err = rtc.StartHost(limit, cfg)
		if err != nil {
			return err
		}
		defer host.Close()
		ticket = host.Ticket()
		if *ticketFile != "" {
			if err = os.MkdirAll(filepath.Dir(*ticketFile), 0700); err != nil {
				return err
			}
			f, err := os.OpenFile(*ticketFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = f.WriteString(ticket)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	if *mode == "host" {
		if *ticketFile == "" {
			return errors.New("host mode requires --ticket-file; credentials are never printed")
		}
		fmt.Println("RTC host ready; private connection link written to the requested file")
		select {
		case <-ctx.Done():
		case <-time.After(*duration):
		}
		return writeResult(*output, map[string]any{"status": "finished", "mode": "host", "synthetic": !*capture, "inputMessagesReceived": recorder.count.Load(), "hostError": errorText(host.Err())})
	}
	if *mode == "viewer" {
		if *ticketFile == "" {
			return errors.New("viewer mode requires --ticket-file")
		}
		raw, err := os.ReadFile(*ticketFile)
		if err != nil {
			return err
		}
		ticket = string(raw)
	}
	viewerConfig := rtc.ConnectConfig{Ticket: ticket, HelperPath: *helper, SSHHost: *sshHost}
	if *sshHost != "" {
		viewerRelay, err := rtc.NewSSHRelay(rtc.SSHConfig{Host: *sshHost, ConfigFile: *config})
		if err != nil {
			return err
		}
		defer viewerRelay.Close()
		viewerConfig.Relay = viewerRelay
	}
	client, err := rtc.Dial(limit, viewerConfig)
	if err != nil {
		return err
	}
	defer client.Close()
	timer := time.NewTimer(*duration)
	defer timer.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	var stats rtc.Stats
	var last uint64
	received := uint64(0)
	inputSent := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-client.Done():
			_, _, failedStats := client.LatestFrame()
			hostError := ""
			if host != nil {
				hostError = errorText(host.Err())
			}
			_ = writeResult(*output, map[string]any{"status": "failed", "stats": failedStats, "hostError": hostError, "viewerError": errorText(client.Err()), "inputMessagesReceived": recorder.count.Load()})
			return fmt.Errorf("RTC ended before acceptance: viewer=%v host=%s", client.Err(), hostError)
		case <-timer.C:
			goto complete
		case <-poll.C:
			frame, seq, current := client.LatestFrame()
			stats = current
			if frame != nil && seq != last {
				received++
				last = seq
			}
			if !inputSent && ((!*allowInput && *mode == "loopback") || *testInput) {
				if *testInput {
					if err := client.SendMouse("down", *inputX, *inputY, 0); err != nil {
						return err
					}
					if err := client.SendMouse("up", *inputX, *inputY, 0); err != nil {
						return err
					}
					time.Sleep(150 * time.Millisecond)
					if err := client.SendKey("down", 0, *inputText, 0); err != nil {
						return err
					}
					if err := client.SendKey("up", 0, "", 0); err != nil {
						return err
					}
					inputSent = true
				} else if err := client.SendKey("down", 0, "", 0); err == nil {
					_ = client.SendKey("up", 0, "", 0)
					_ = client.SendMouse("down", .25, .25, 0)
					_ = client.SendMouse("up", .25, .25, 0)
					_ = client.SendWheel(0, 1)
					inputSent = true
				}
			}
		}
	}
complete:
	_, _, stats = client.LatestFrame()
	expectedCandidate, expectedMode := "relay", "relay-webrtc"
	if strings.HasPrefix(ticket, "vantry-lan:") {
		expectedCandidate, expectedMode = "host", "direct-webrtc"
	}
	passed := received >= 10 && stats.Mode == expectedMode && stats.LocalCandidateType == expectedCandidate && stats.RemoteCandidateType == expectedCandidate && stats.Hardware
	if *mode == "loopback" && !*allowInput {
		passed = passed && inputSent && recorder.count.Load() >= 5
	}
	status := "passed"
	if !passed {
		status = "failed"
	}
	result := map[string]any{"status": status, "mode": *mode, "synthetic": !*capture, "stats": stats, "framesPolled": received, "inputMessagesReceived": recorder.count.Load(), "hostError": ""}
	if host != nil {
		result["hostError"] = errorText(host.Err())
	}
	if err = writeResult(*output, result); err != nil {
		return err
	}
	fmt.Println("RTC acceptance", status, "frames", received, "transport", stats.Transport, "rttMs", stats.RTTMs, "estimatedFrameAgeP95Ms", stats.FrameAgeP95Ms)
	if !passed {
		return errors.New("RTC acceptance did not satisfy the selected route, video and input requirements")
	}
	return nil
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func writeResult(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}
