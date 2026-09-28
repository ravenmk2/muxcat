package rabbitmq

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// healthChecks enumerates the checks of /api/health/checks/<check>. The
// last two are 4.x-only; requesting them on an older server answers 404,
// which the client maps to UNSUPPORTED_OPERATION.
var healthChecks = []string{
	"alarms", "local-alarms", "port-listener", "protocol-listener",
	"virtual-hosts", "node-is-quorum-critical", "certificate-expiration",
	"is-in-service", "ready-to-serve-clients",
}

func newHealthCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "health [check]",
		Short: "Run a health check (GET /api/health/checks/<check>)",
		Long: `Run a health check against the broker. Without arguments the
"alarms" check runs; valid check names are alarms, local-alarms,
port-listener, protocol-listener, virtual-hosts,
node-is-quorum-critical, certificate-expiration, is-in-service and
ready-to-serve-clients (the last two are RabbitMQ 4.x only — on older
servers they answer 404, reported as UNSUPPORTED_OPERATION).

A healthy broker answers {"status":"ok"}; a failed check makes the
server answer 503, reported as QUERY_ERROR with the server's reason.
--aliveness runs the aliveness test for a vhost instead
(GET /api/aliveness-test/<vhost>).`,
		Args: cobra.MaximumNArgs(1),
		Example: `  muxcat rabbitmq health
  muxcat rabbitmq health local-alarms
  muxcat rabbitmq health --aliveness --vhost /`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := ""
			switch {
			case cli.FlagBool(cmd, "aliveness"):
				path = "/api/aliveness-test/" + esc(vhostFlag(cmd))
			case len(args) == 1:
				check := args[0]
				valid := false
				for _, hc := range healthChecks {
					if hc == check {
						valid = true
						break
					}
				}
				if !valid {
					return output.NewError(output.CodeConfigInvalid,
						"unknown health check: "+check,
						"valid checks: "+joinHealthChecks())
				}
				path = "/api/health/checks/" + check
			default:
				path = "/api/health/checks/alarms"
			}
			resp, err := cl.do(cmd.Context(), "GET", path, nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}
			status := str(obj(raw)["status"])
			return cli.RenderResult(cmd, &output.Result{
				Value:    raw,
				JSONData: raw,
				Message:  fmt.Sprintf("status: %s", status),
			}, meta(name, start, false))
		},
	}
	c.Flags().Bool("aliveness", false, "run the aliveness test for --vhost instead of a health check")
	c.Flags().String("vhost", "", "vhost for the aliveness test (default: /)")
	return c
}

func joinHealthChecks() string {
	out := ""
	for i, c := range healthChecks {
		if i > 0 {
			out += ", "
		}
		out += c
	}
	return out
}
