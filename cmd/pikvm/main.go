package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"pikvm-key-cli/internal/client"
	"pikvm-key-cli/internal/server"

	"github.com/spf13/cobra"
)

var (
	flagHost   string
	flagUser   string
	flagPass   string
	flagKeymap string
)

func pikvm() *client.Client {
	host := flagHost
	if host == "" {
		host = os.Getenv("PIKVM_HOST")
	}
	user := flagUser
	if user == "" {
		user = os.Getenv("PIKVM_USER")
		if user == "" {
			user = "admin"
		}
	}
	pass := flagPass
	if pass == "" {
		pass = os.Getenv("PIKVM_PASS")
		if pass == "" {
			pass = "admin"
		}
	}
	if host == "" {
		fmt.Fprintln(os.Stderr, "error: --host or PIKVM_HOST required")
		os.Exit(1)
	}
	if !strings.HasPrefix(host, "http") {
		host = "https://" + host
	}
	return client.New(host, user, pass)
}

func printJSON(v interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func main() {
	root := &cobra.Command{
		Use:   "pikvm",
		Short: "CLI and web UI for PiKVM v3 control",
		Long: `pikvm controls a PiKVM device via its HTTP API.

Configure the target with flags or environment variables:
  PIKVM_HOST   https://192.168.1.x  (required for device commands)
  PIKVM_USER   admin                (default: admin)
  PIKVM_PASS   admin                (default: admin)`,
	}

	root.PersistentFlags().StringVarP(&flagHost, "host", "H", "", "PiKVM URL (e.g. https://192.168.1.10 or PIKVM_HOST env)")
	root.PersistentFlags().StringVarP(&flagUser, "user", "u", "", "Username (default: admin)")
	root.PersistentFlags().StringVarP(&flagPass, "pass", "p", "", "Password (default: admin)")

	// --- server ---
	var serverPort string
	cmdServer := &cobra.Command{
		Use:   "server",
		Short: "Start the web UI dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			host := flagHost
			if host == "" {
				host = os.Getenv("PIKVM_HOST")
			}
			user := flagUser
			if user == "" {
				user = os.Getenv("PIKVM_USER")
				if user == "" {
					user = "admin"
				}
			}
			pass := flagPass
			if pass == "" {
				pass = os.Getenv("PIKVM_PASS")
				if pass == "" {
					pass = "admin"
				}
			}
			if !strings.HasPrefix(host, "http") && host != "" {
				host = "https://" + host
			}
			return server.Run(serverPort, host, user, pass)
		},
	}
	cmdServer.Flags().StringVarP(&serverPort, "port", "P", "8095", "Port to listen on")

	// --- info ---
	cmdInfo := &cobra.Command{
		Use:   "info",
		Short: "Print system info from the PiKVM device",
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := pikvm().Info(context.Background())
			if err != nil {
				return err
			}
			printJSON(info)
			return nil
		},
	}

	// --- type ---
	cmdType := &cobra.Command{
		Use:   "type <text>",
		Short: "Type text on the target machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return pikvm().TypeText(context.Background(), args[0], flagKeymap)
		},
	}
	cmdType.Flags().StringVarP(&flagKeymap, "keymap", "k", "", "Keyboard layout (e.g. en-us, de, ru)")

	// --- key ---
	cmdKey := &cobra.Command{
		Use:   "key <keyname>",
		Short: "Send a single key press (e.g. Enter, Escape, KeyA, F1)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return pikvm().SendKey(context.Background(), args[0])
		},
	}

	// --- shortcut ---
	cmdShortcut := &cobra.Command{
		Use:   "shortcut <key1,key2,...>",
		Short: "Send a key combo (e.g. ControlLeft,AltLeft,Delete)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return pikvm().SendShortcut(context.Background(), args[0])
		},
	}

	// --- power ---
	cmdPower := &cobra.Command{
		Use:   "power <on|off|off-hard|reset>",
		Short: "ATX power control",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			action := args[0]
			// normalize dashes to underscores
			action = strings.ReplaceAll(action, "-", "_")
			return pikvm().Power(context.Background(), action)
		},
	}

	// --- screenshot ---
	var screenshotOut string
	cmdScreenshot := &cobra.Command{
		Use:   "screenshot",
		Short: "Capture a JPEG screenshot from the PiKVM",
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := pikvm().Screenshot(context.Background())
			if err != nil {
				return err
			}
			out := screenshotOut
			if out == "" {
				out = "screenshot.jpg"
			}
			if err := os.WriteFile(out, data, 0o644); err != nil {
				return err
			}
			fmt.Printf("saved %d bytes to %s\n", len(data), out)
			return nil
		},
	}
	cmdScreenshot.Flags().StringVarP(&screenshotOut, "out", "o", "screenshot.jpg", "Output file path")

	// --- msd ---
	cmdMSD := &cobra.Command{Use: "msd", Short: "Mass Storage Device control"}
	cmdMSDConnect := &cobra.Command{
		Use:   "connect",
		Short: "Connect MSD to the target host",
		RunE: func(cmd *cobra.Command, args []string) error {
			return pikvm().MSDConnect(context.Background(), true)
		},
	}
	cmdMSDDisconnect := &cobra.Command{
		Use:   "disconnect",
		Short: "Disconnect MSD from the target host",
		RunE: func(cmd *cobra.Command, args []string) error {
			return pikvm().MSDConnect(context.Background(), false)
		},
	}
	cmdMSDState := &cobra.Command{
		Use:   "state",
		Short: "Show MSD state",
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := pikvm().MSDState(context.Background())
			if err != nil {
				return err
			}
			printJSON(state)
			return nil
		},
	}
	cmdMSD.AddCommand(cmdMSDConnect, cmdMSDDisconnect, cmdMSDState)

	// --- gpio ---
	cmdGPIO := &cobra.Command{Use: "gpio", Short: "GPIO control"}
	cmdGPIOState := &cobra.Command{
		Use:   "state",
		Short: "Show GPIO state",
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := pikvm().GPIOState(context.Background())
			if err != nil {
				return err
			}
			printJSON(state)
			return nil
		},
	}
	cmdGPIOPulse := &cobra.Command{
		Use:   "pulse <channel>",
		Short: "Pulse a GPIO channel (e.g. __v3_usb_breaker__)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return pikvm().GPIOPulse(context.Background(), args[0])
		},
	}
	cmdGPIO.AddCommand(cmdGPIOState, cmdGPIOPulse)

	root.AddCommand(cmdServer, cmdInfo, cmdType, cmdKey, cmdShortcut, cmdPower, cmdScreenshot, cmdMSD, cmdGPIO)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
