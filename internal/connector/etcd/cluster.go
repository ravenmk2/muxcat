package etcd

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// newEndpointCmd builds the endpoint inspection group.
func newEndpointCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "endpoint",
		Short: "Inspect cluster endpoints",
		Long: `Inspect the instance's endpoints. status calls maintenance Status
on every endpoint, health probes every endpoint with a Get (the same
approach as etcdctl endpoint health). A failed endpoint degrades to a
row with only the error column filled; the command fails only when
every endpoint fails.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newEndpointStatusCmd(), newEndpointHealthCmd())
	return c
}

func newEndpointStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report maintenance Status for every endpoint of the instance",
		Args:  cobra.NoArgs,
		Example: `  muxcat etcd endpoint status
  muxcat etcd endpoint status -c prod --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			inst, err := cfg.instanceOf(conn)
			if err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			columns := []string{"endpoint", "id", "version", "db_size", "is_leader",
				"raft_term", "raft_index", "raft_applied_index", "error"}
			rows := make([][]any, 0, len(inst.Endpoints))
			var lastErr error
			failures := 0
			for _, ep := range inst.Endpoints {
				resp, err := client.Status(ctx, ep)
				if err != nil {
					failures++
					lastErr = err
					rows = append(rows, []any{ep, nil, nil, nil, nil, nil, nil, nil, err.Error()})
					continue
				}
				rows = append(rows, []any{ep, resp.Header.MemberId, resp.Version, resp.DbSize,
					resp.Leader == resp.Header.MemberId, resp.RaftTerm,
					resp.RaftIndex, resp.RaftAppliedIndex, ""})
			}
			if failures == len(inst.Endpoints) {
				return classifyErr(lastErr, "endpoint status failed")
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: columns,
				Rows:    rows,
			}, meta(cfg, conn, name, start, false))
		},
	}
}

func newEndpointHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Probe every endpoint of the instance (per-endpoint Get, like etcdctl)",
		Args:  cobra.NoArgs,
		Example: `  muxcat etcd endpoint health
  muxcat etcd endpoint health --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			inst, err := cfg.instanceOf(conn)
			if err != nil {
				return err
			}
			timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
			if err != nil {
				return err
			}

			// Probe each endpoint with its own client (a shared client would
			// let the balancer pick any endpoint); a permission-denied Get
			// still proves the endpoint is alive and authenticated.
			columns := []string{"endpoint", "health", "took_ms", "error"}
			rows := make([][]any, 0, len(inst.Endpoints))
			var lastErr error
			failures := 0
			for _, ep := range inst.Endpoints {
				epStart := time.Now()
				err := probeEndpoint(cmd, cfg, conn, ep, timeout)
				took := time.Since(epStart).Milliseconds()
				switch {
				case err == nil || isPermissionErr(err):
					rows = append(rows, []any{ep, true, took, ""})
				default:
					failures++
					lastErr = err
					rows = append(rows, []any{ep, false, took, err.Error()})
				}
			}
			if failures == len(inst.Endpoints) {
				return classifyErr(lastErr, "endpoint health failed")
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: columns,
				Rows:    rows,
			}, meta(cfg, conn, name, start, false))
		},
	}
}

// probeEndpoint issues a bounded Get("health") against a single endpoint
// with a dedicated single-endpoint client.
func probeEndpoint(cmd *cobra.Command, cfg *Config, conn Connection, endpoint string, timeout time.Duration) error {
	cc, err := clientConfig(cfg, conn, timeout)
	if err != nil {
		return err
	}
	cc.Endpoints = []string{endpoint}
	client, err := clientv3.New(*cc)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	_, err = client.Get(ctx, "health")
	return err
}

// newMemberCmd builds the member inspection group.
func newMemberCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "member",
		Short: "Inspect cluster members",
		Long: `Inspect cluster members. member list shows the cluster membership
via the client's MemberList (ID, name, peer/client URLs, learner flag).
Member add/remove/update are out of scope.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newMemberListCmd())
	return c
}

func newMemberListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List cluster members",
		Args:  cobra.NoArgs,
		Example: `  muxcat etcd member list
  muxcat etcd member list -c prod --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			resp, err := client.MemberList(ctx)
			if err != nil {
				return classifyErr(err, "member list failed")
			}
			rows := make([][]any, 0, len(resp.Members))
			for _, m := range resp.Members {
				rows = append(rows, []any{m.ID, m.Name,
					strings.Join(m.PeerURLs, ","), strings.Join(m.ClientURLs, ","), m.IsLearner})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"id", "name", "peer_urls", "client_urls", "is_learner"},
				Rows:    rows,
			}, meta(cfg, conn, name, start, false))
		},
	}
}

// newAlarmCmd builds the alarm inspection group.
func newAlarmCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "alarm",
		Short: "Inspect and disarm cluster alarms",
		Long: `Inspect and disarm cluster alarms. alarm list shows active alarms
(NOSPACE, CORRUPT, ...); alarm disarm disarms every active alarm —
it counts as a write, so readonly connections reject it, but it is
not a dangerous operation (no allowDangerous needed).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newAlarmListCmd(), newAlarmDisarmCmd())
	return c
}

func newAlarmListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List active cluster alarms",
		Args:  cobra.NoArgs,
		Example: `  muxcat etcd alarm list
  muxcat etcd alarm list --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			resp, err := client.AlarmList(ctx)
			if err != nil {
				return classifyErr(err, "alarm list failed")
			}
			rows := make([][]any, 0, len(resp.Alarms))
			for _, a := range resp.Alarms {
				if a.Alarm == etcdserverpb.AlarmType_NONE {
					continue
				}
				rows = append(rows, []any{a.MemberID, a.Alarm.String()})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"member_id", "alarm"},
				Rows:    rows,
			}, meta(cfg, conn, name, start, false))
		},
	}
}

func newAlarmDisarmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disarm",
		Short: "Disarm all active cluster alarms (counts as a write)",
		Args:  cobra.NoArgs,
		Example: `  muxcat etcd alarm disarm
  muxcat etcd alarm disarm -c prod --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "alarm disarm"); err != nil {
				return err
			}
			ctx, cancel, client, err := dial(cmd, cfg, conn)
			if err != nil {
				return err
			}
			defer cancel()
			defer func() { _ = client.Close() }()

			resp, err := client.AlarmList(ctx)
			if err != nil {
				return classifyErr(err, "alarm list failed")
			}
			disarmed := 0
			for _, a := range resp.Alarms {
				if a.Alarm == etcdserverpb.AlarmType_NONE {
					continue
				}
				if _, err := client.AlarmDisarm(ctx, (*clientv3.AlarmMember)(a)); err != nil {
					return classifyErr(err, "alarm disarm failed")
				}
				disarmed++
			}
			if disarmed == 0 {
				return cli.RenderResult(cmd, &output.Result{
					Message: "no active alarms",
				}, meta(cfg, conn, name, start, false))
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{"disarmed": disarmed},
				Bare:  true,
			}, meta(cfg, conn, name, start, false))
		},
	}
}
