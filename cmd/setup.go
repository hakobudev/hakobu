package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
)

var (
	setupReconnect bool
	setupDomain    string
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Connect Cloudflare and choose the panel's address (run by install.sh)",
	RunE:  runSetup,
}

func init() {
	setupCmd.Flags().BoolVar(&setupReconnect, "reconnect", false, "give hakobu a new Cloudflare API token")
	setupCmd.Flags().StringVar(&setupDomain, "domain", "", "move the panel to this domain (keeping its subdomain) or host, with the apps and emails on its old domain")
	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, args []string) error {
	if err := config.PrepareDataDir(); err != nil {
		return err
	}
	s, err := store.OpenWithKey(config.DatabaseFile, config.MasterKeyFile)
	if err != nil {
		return err
	}
	if err := ensureCloudflare(s); err != nil {
		return err
	}
	if setupDomain != "" {
		return movePanel(s)
	}
	if ops.TunnelReady(s) {
		fmt.Println("Cloudflare is connected, panel: https://" + config.PublicHost())
		return nil
	}

	zones, err := ops.Zones(s)
	if err != nil {
		return err
	}
	if len(zones) == 0 {
		return fmt.Errorf("the connected Cloudflare account has no active domains; add one to Cloudflare and run `hakobu setup` again")
	}
	in := bufio.NewReader(os.Stdin)
	zone := zones[0]
	if len(zones) > 1 {
		fmt.Println("\nWhich domain should hakobu use?")
		for i, z := range zones {
			fmt.Printf("  %d) %s\n", i+1, z.Name)
		}
		for {
			fmt.Printf("Choice [1-%d]: ", len(zones))
			line, err := readLine(in)
			if err != nil {
				return err
			}
			if n, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && n >= 1 && n <= len(zones) {
				zone = zones[n-1]
				break
			}
		}
	}
	fmt.Printf("Panel address: <subdomain>.%s [hakobu]: ", zone.Name)
	sub, err := readLine(in)
	if err != nil {
		return err
	}
	sub = strings.TrimSpace(sub)
	if sub == "" {
		sub = "hakobu"
	}

	host, err := ops.SetupTunnel(s, zone.ID, sub)
	if err != nil {
		return err
	}
	fmt.Println("Created the tunnel and https://" + host)
	return nil
}

func movePanel(s *store.Store) error {
	m, err := ops.MovePanel(s, setupDomain)
	if err != nil {
		return err
	}
	fmt.Println("The panel is at https://" + m.Host + "; hakobu takes the new address as it runs, no restart needed.")
	for _, d := range m.Done {
		fmt.Println("  moved " + d)
	}
	fmt.Println("\nWithin 10 seconds the panel tells its connected servers the new address (restart hakobu")
	fmt.Println("before then and they aren't told), then restarts the apps one by one for their SENTRY_PUBLIC_DSN.")
	fmt.Println("A server that is away learns it when it connects, if the old address still reaches the panel; if not,")
	fmt.Println("set \"panel\" in /opt/hakobu/data/node.json on it to https://" + m.Host + " and restart hakobu there.")
	if len(m.Manual) > 0 {
		fmt.Println("\nStill to change:")
		for _, l := range m.Manual {
			fmt.Println("  " + l)
		}
	}
	return nil
}

// ensureCloudflare asks for a token when there is none, when told to, or
// when the saved one stopped working: a rerun of the installer after a
// setup that failed on a deleted token would otherwise keep using it.
func ensureCloudflare(s *store.Store) error {
	if !setupReconnect && ops.CloudflareConnected(s) {
		_, err := ops.Zones(s)
		if err == nil {
			return nil
		}
		fmt.Println("The saved Cloudflare token doesn't work:", err)
	}
	return connectCloudflare(s)
}

// connectCloudflare asks for an API token, through the dashboard form for
// one with hakobu's permissions filled in, or takes CLOUDFLARE_API_TOKEN.
func connectCloudflare(s *store.Store) error {
	if token := os.Getenv("CLOUDFLARE_API_TOKEN"); token != "" {
		_, err := ops.ConnectCloudflare(s, token)
		return err
	}
	host, _ := os.Hostname()
	fmt.Println("\nOpen this link, select Continue to summary and Create Token (the permissions are filled in):")
	fmt.Println("\n  " + cloudflare.TokenTemplateURL("hakobu "+strings.Split(host, ".")[0]))
	fmt.Println("\nUnder Zone Resources, pick only the domains hakobu should use: the token can")
	fmt.Println("change the DNS of every domain it covers.")
	fmt.Println("\nCopy the token Cloudflare shows and paste it here (it isn't echoed).")
	for attempt := 0; ; attempt++ {
		fmt.Print("API token: ")
		token, err := readSecret()
		if err != nil {
			return err
		}
		if strings.TrimSpace(token) == "" {
			continue
		}
		if _, err = ops.ConnectCloudflare(s, token); err == nil {
			fmt.Println("Connected.")
			return nil
		}
		fmt.Println(err)
		if attempt == 2 {
			return fmt.Errorf("no working token; run `hakobu setup` again")
		}
	}
}

// errNoSecret is what readSecret returns when stdin is neither a terminal
// nor holds a line, as under ssh without -t.
var errNoSecret = errors.New("no Cloudflare API token: run this in a terminal, or set CLOUDFLARE_API_TOKEN")

// readSecret reads a line without echoing it when stdin is a terminal.
func readSecret() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if errors.Is(err, io.EOF) && line == "" {
		fmt.Println() // end the prompt's line
		return "", errNoSecret
	}
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// readLine reads an answer; a closed stdin ends setup instead of asking
// forever.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		fmt.Println() // end the prompt's line
		return "", errors.New("no answer: run this in a terminal")
	}
	return line, nil
}
