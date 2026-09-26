package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
		Long: `Inspect cluster endpoints. status calls maintenance Status on every
endpoint, health probes every endpoint with a Get plus an alarm check
(the same semantics as etcdctl endpoint health). Both support
--cluster: probe every cluster member (client URLs discovered via
MemberList) instead of the configured endpoints.

A failed endpoint degrades to a row with only the error column filled;
the partial table still renders and the command then exits non-zero
(degraded result, see: muxcat help output).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newEndpointStatusCmd(), newEndpointHealthCmd())
	return c
}

// lazyClient builds a client without any reachability probe, so a dead
// first endpoint cannot sink whole-cluster inspection commands.
func lazyClient(cfg *Config, conn Connection, timeout time.Duration) (*clientv3.Client, error) {
	cc, err := clientConfig(cfg, conn, timeout)
	if err != nil {
		return nil, err
	}
	client, err := clientv3.New(*cc)
	if err != nil {
		return nil, classifyErr(err, "failed to connect")
	}
	return client, nil
}

// probeTargets resolves the endpoints to probe: the instance's endpoints,
// or every member's client URLs with --cluster (discovered via MemberList
// on the lazy client).
func probeTargets(ctx context.Context, client *clientv3.Client, inst Instance, cluster bool) ([]string, error) {
	if !cluster {
		return inst.Endpoints, nil
	}
	resp, err := client.MemberList(ctx)
	if err != nil {
		return nil, classifyErr(err, "member list failed")
	}
	var targets []string
	for _, m := range resp.Members {
		targets = append(targets, m.ClientURLs...)
	}
	if len(targets) == 0 {
		return nil, output.NewError(output.CodeQueryError,
			"no client URLs found in the cluster membership", "")
	}
	return targets, nil
}

func newEndpointStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Report maintenance Status for every endpoint",
		Long: `Report maintenance Status for every endpoint, each with its own
timeout budget. A failed endpoint degrades to a row with only the
error column filled; any failure makes the command exit non-zero
after rendering the partial table (degraded result).`,
		Args: cobra.NoArgs,
		Example: `  muxcat etcd endpoint status
  muxcat etcd endpoint status --cluster
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
			timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
			if err != nil {
				return err
			}
			client, err := lazyClient(cfg, conn, timeout)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			mctx, mcancel := context.WithTimeout(cmd.Context(), timeout)
			targets, err := probeTargets(mctx, client, inst, cli.FlagBool(cmd, "cluster"))
			mcancel()
			if err != nil {
				return err
			}

			columns := []string{"endpoint", "id", "version", "db_size", "is_leader",
				"raft_term", "raft_index", "raft_applied_index", "error"}
			rows := make([][]any, 0, len(targets))
			var lastErr error
			failures := 0
			for _, ep := range targets {
				ectx, ecancel := context.WithTimeout(cmd.Context(), timeout)
				resp, err := client.Status(ectx, ep)
				ecancel()
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
			m := meta(cfg, conn, name, start, false)
			res := &output.Result{Columns: columns, Rows: rows}
			if failures > 0 {
				return cli.RenderPartial(cmd, res, m, classifyErr(lastErr,
					fmt.Sprintf("%d of %d endpoints failed", failures, len(targets))))
			}
			return cli.RenderResult(cmd, res, m)
		},
	}
	c.Flags().Bool("cluster", false, "probe every cluster member (client URLs via MemberList) instead of the configured endpoints")
	return c
}

// healthProbe is the per-endpoint outcome of an endpoint health probe.
type healthProbe struct {
	endpoint string
	healthy  bool
	tookMS   int64
	errText  string
	err      error
}

func newEndpointHealthCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "health",
		Short: "Probe every endpoint (parallel Get + alarm check, like etcdctl)",
		Long: `Probe every endpoint in parallel with a dedicated single-endpoint
client: a bounded Get("health") proves liveness (permission denied
still counts, the endpoint answered), then the alarm list is checked —
active alarms (NOSPACE, CORRUPT) mark the endpoint unhealthy. Any
unhealthy endpoint makes the command exit non-zero after rendering
the partial table (degraded result).`,
		Args: cobra.NoArgs,
		Example: `  muxcat etcd endpoint health
  muxcat etcd endpoint health --cluster
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
			client, err := lazyClient(cfg, conn, timeout)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			mctx, mcancel := context.WithTimeout(cmd.Context(), timeout)
			targets, err := probeTargets(mctx, client, inst, cli.FlagBool(cmd, "cluster"))
			mcancel()
			if err != nil {
				return err
			}

			probes := make([]healthProbe, len(targets))
			var wg sync.WaitGroup
			for i, ep := range targets {
				wg.Add(1)
				go func(i int, ep string) {
					defer wg.Done()
					probes[i] = probeHealth(cmd, cfg, conn, ep, timeout)
				}(i, ep)
			}
			wg.Wait()

			rows := make([][]any, 0, len(probes))
			failures := 0
			var lastErr error
			for _, p := range probes {
				rows = append(rows, []any{p.endpoint, p.healthy, p.tookMS, p.errText})
				if p.healthy {
					continue
				}
				failures++
				if p.err != nil {
					lastErr = p.err
				}
			}
			m := meta(cfg, conn, name, start, false)
			res := &output.Result{
				Columns: []string{"endpoint", "health", "took_ms", "error"},
				Rows:    rows,
			}
			if failures > 0 {
				if lastErr == nil {
					lastErr = errors.New("active alarms")
				}
				return cli.RenderPartial(cmd, res, m, classifyErr(lastErr,
					fmt.Sprintf("%d of %d endpoints unhealthy", failures, len(targets))))
			}
			return cli.RenderResult(cmd, res, m)
		},
	}
	c.Flags().Bool("cluster", false, "probe every cluster member (client URLs via MemberList) instead of the configured endpoints")
	return c
}

// probeHealth probes one endpoint with a dedicated single-endpoint client
// (a shared client would let the balancer pick any endpoint).
func probeHealth(cmd *cobra.Command, cfg *Config, conn Connection, endpoint string, timeout time.Duration) healthProbe {
	start := time.Now()
	p := healthProbe{endpoint: endpoint}
	fail := func(err error, text string) healthProbe {
		p.err, p.errText = err, text
		p.tookMS = time.Since(start).Milliseconds()
		return p
	}
	cc, err := clientConfig(cfg, conn, timeout)
	if err != nil {
		return fail(err, err.Error())
	}
	cc.Endpoints = []string{endpoint}
	client, err := clientv3.New(*cc)
	if err != nil {
		return fail(err, err.Error())
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	if _, err := client.Get(ctx, "health"); err != nil && !isPermissionErr(err) {
		return fail(err, err.Error())
	}
	p.healthy = true
	resp, err := client.AlarmList(ctx)
	if err != nil {
		p.healthy = false
		return fail(err, "Unable to fetch the alarm list")
	}
	var active []string
	for _, a := range resp.Alarms {
		if a.Alarm != etcdserverpb.AlarmType_NONE {
			active = append(active, a.Alarm.String())
		}
	}
	if len(active) > 0 {
		p.healthy = false
		p.errText = "Active Alarm(s): " + strings.Join(active, ", ")
	}
	p.tookMS = time.Since(start).Milliseconds()
	return p
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
