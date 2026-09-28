package amqp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newConsumeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "consume <queue>",
		Short: "Consume messages from a queue (one-shot batch)",
		Long: `Consume messages from a queue in one-shot mode: collect up to
--count messages or stop at --timeout, settle them all, and return
one envelope.

Settlement defaults to --requeue (non-destructive, allowed on
readonly connections); --ack (consume) and --reject
(discard/dead-letter) are destructive and refused on readonly
connections. Text output truncates each payload at 1KiB; --json
keeps full payloads (base64 for non-UTF-8 bodies); --file writes
payloads to a directory (0600). A streaming --follow mode is not
implemented.`,
		Args: cli.ExactArgs(1, "<queue>", "queue"),
		Example: `  muxcat amqp consume my-queue
  muxcat amqp consume my-queue --count 10 --timeout 10s
  muxcat amqp consume my-queue --count 1 --ack
  muxcat amqp consume my-queue --count 5 --file ./payloads`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			mode, err := consumeMode(cmd)
			if err != nil {
				return err
			}
			writeOp := ""
			if mode != "requeue" {
				writeOp = "consume --" + mode
			}
			env, err := openForCmd(cmd, writeOp)
			if err != nil {
				return err
			}
			defer env.close()
			count, _ := cmd.Flags().GetInt("count")
			if count < 1 || count > 10000 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid --count: %d", count), "valid range: 1-10000")
			}
			wait, _ := cmd.Flags().GetDuration("timeout")
			if wait <= 0 {
				return output.NewError(output.CodeConfigInvalid,
					"invalid --timeout: "+wait.String(), "examples: 5s, 1m (must be > 0)")
			}

			rcv, err := env.session.NewReceiver(env.ctx, args[0])
			if err != nil {
				return err
			}
			defer func() { _ = rcv.Close() }()

			ctx, cancel := context.WithTimeout(env.ctx, wait)
			defer cancel()
			var collected []DeliveryContext
			for len(collected) < count {
				dc, err := rcv.Receive(ctx)
				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) {
						break
					}
					return err
				}
				collected = append(collected, dc)
			}
			if len(collected) == 0 {
				return output.NewError(output.CodeTimeout,
					fmt.Sprintf("no message received from %s within %s", args[0], wait),
					"check that the queue holds messages (amqp queue show "+args[0]+")")
			}
			// Settle on a fresh context: the collection window above has
			// typically expired (it is what stopped the loop), but
			// settlement must still go through.
			settleCtx, settleCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer settleCancel()
			for _, dc := range collected {
				if err := settle(settleCtx, dc, mode); err != nil {
					return err
				}
			}

			msgs := make([]*Delivery, 0, len(collected))
			for _, dc := range collected {
				msgs = append(msgs, dc.Delivery())
			}
			rows := make([][]any, 0, len(msgs))
			jsonRows := make([]any, 0, len(msgs))
			for _, m := range msgs {
				rows = append(rows, []any{m.Exchange, m.RoutingKey, m.Redelivered, displayPayload(m.Payload)})
				jsonRows = append(jsonRows, messageData(m))
			}
			res := &output.Result{
				Columns:  []string{"exchange", "routing_key", "redelivered", "payload"},
				Rows:     rows,
				JSONData: jsonRows,
			}
			if dir := cli.FlagString(cmd, "file"); dir != "" {
				files, err := writePayloads(dir, args[0], msgs)
				if err != nil {
					return err
				}
				summary := map[string]any{"files": files, "count": len(files)}
				res.Value = summary
				res.JSONData = summary
				res.Rows = nil
				res.Columns = nil
				res.Message = fmt.Sprintf("wrote %d payload(s) to %s", len(files), strings.Join(files, ", "))
			}
			return cli.RenderResult(cmd, res, meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	c.Flags().Int("count", 1, "number of messages to collect (1-10000)")
	c.Flags().Duration("timeout", 30*time.Second, "stop collecting after this long without enough messages")
	c.Flags().Bool("ack", false, "settle: consume the messages (destructive)")
	c.Flags().Bool("reject", false, "settle: discard/dead-letter the messages (destructive)")
	c.Flags().Bool("requeue", false, "settle: return the messages to the queue (default, non-destructive)")
	c.Flags().String("file", "", "write payloads to this directory: <queue>-0.bin, <queue>-1.bin... (0600)")
	return c
}

// consumeMode resolves the mutually exclusive settlement flags; the
// default is requeue.
func consumeMode(cmd *cobra.Command) (string, error) {
	var mode string
	for _, f := range []string{"ack", "reject", "requeue"} {
		if cli.FlagBool(cmd, f) {
			if mode != "" {
				return "", output.NewError(output.CodeConfigInvalid,
					"--"+mode+" and --"+f+" are mutually exclusive", "pick one settlement mode")
			}
			mode = f
		}
	}
	if mode == "" {
		mode = "requeue"
	}
	return mode, nil
}
