package cli

import (
	"fmt"
	"strconv"

	"drip/internal/client/tcp"
	"drip/internal/shared/protocol"

	"github.com/spf13/cobra"
)

var tcpCmd = &cobra.Command{
	Use:   "tcp <port>",
	Short: "Start TCP tunnel",
	Long: `Start a TCP tunnel to expose any TCP service.

Example:
  drip tcp 5432                     Tunnel PostgreSQL
  drip tcp 3306                     Tunnel MySQL
  drip tcp 22                       Tunnel SSH
  drip tcp 6379 --subdomain myredis Tunnel Redis with custom subdomain

Supported Services:
  - Databases: PostgreSQL (5432), MySQL (3306), Redis (6379), MongoDB (27017)
  - SSH: Port 22
  - Any TCP service

Configuration:
  First time: Run 'drip config init' to save server and token
  Subsequent: Just run 'drip tcp <port>'

Note: Uses TCP over TLS 1.3 for secure communication`,
	Args: cobra.ExactArgs(1),
	RunE: runTCP,
}

func init() {
	tcpCmd.Flags().StringVarP(&subdomain, "subdomain", "n", "", "Custom subdomain (optional)")
	tcpCmd.Flags().BoolVarP(&daemonMode, "daemon", "d", false, "Run in background (daemon mode)")
	tcpCmd.Flags().StringVarP(&localAddress, "address", "a", "127.0.0.1", "Local address to forward to (default: 127.0.0.1)")
	tcpCmd.Flags().BoolVar(&daemonMarker, "daemon-child", false, "Internal flag for daemon child process")
	tcpCmd.Flags().MarkHidden("daemon-child")
	rootCmd.AddCommand(tcpCmd)
}

func runTCP(_ *cobra.Command, args []string) error {
	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port number: %s", args[0])
	}

	if daemonMode && !daemonMarker {
		return StartDaemon("tcp", port, buildDaemonArgs("tcp", args, subdomain, localAddress))
	}

	serverAddr, token, err := resolveServerAddrAndToken("tcp", port)
	if err != nil {
		return err
	}

	connConfig := &tcp.ConnectorConfig{
		ServerAddr: serverAddr,
		Token:      token,
		TunnelType: protocol.TunnelTypeTCP,
		LocalHost:  localAddress,
		LocalPort:  port,
		Subdomain:  subdomain,
		Insecure:   insecure,
	}

	var daemon *DaemonInfo
	if daemonMarker {
		daemon = newDaemonInfo("tcp", port, subdomain, serverAddr)
	}

	return runTunnelWithUI(connConfig, daemon)
}
